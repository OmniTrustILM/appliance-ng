package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	testRestoreHost   = "restored.appliance.internal"
	testBackupContent = "encrypted-backup-bytes-5c1d"
	testNewKitContent = "reissued-kit-bytes-9a0e"
)

// restoreConsole models the console's restore API: a started restore waits for its kit once, is resumed and is done
// on the poll after that. The reissued kit is newKit.
type restoreConsole struct {
	kit, newKit                  []byte
	uploaded, uploadName         string
	inspected, resumed, reissued map[string]string
	started                      map[string]any
	polls                        int
	confirmed                    bool
	refuseInspect, failRestore   bool
}

// multipartFields reads the parts of a multipart request into a map.
func multipartFields(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
		return nil
	}
	out := map[string]string{}
	for name, values := range r.MultipartForm.Value {
		out[name] = values[0]
	}
	for name, files := range r.MultipartForm.File {
		f, err := files[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(f)
		_ = f.Close()
		out[name] = string(data)
	}
	return out
}

func (rc *restoreConsole) serve(t *testing.T) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, status int, body any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /appliance/api/v1/restore", func(w http.ResponseWriter, _ *http.Request) {
		if rc.started == nil {
			reply(w, http.StatusOK, map[string]any{"setup": "restore"})
			return
		}
		rc.polls++
		progress := map[string]any{"backup": rc.uploadName, "inPlace": false, "step": "platform"}
		switch {
		case rc.failRestore:
			progress["message"] = "failed"
		case rc.resumed == nil:
			progress["step"], progress["pausedStep"] = "waitingForKit", "database"
		default:
			progress["step"] = "done"
		}
		reply(w, http.StatusOK, map[string]any{"setup": "restore", "restore": progress})
	})
	mux.HandleFunc("PUT /appliance/api/v1/restore/upload", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		rc.uploaded, rc.uploadName = string(data), r.URL.Query().Get("name")
		if r.ContentLength != int64(len(data)) {
			t.Errorf("upload Content-Length = %d, want %d", r.ContentLength, len(data))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /appliance/api/v1/restore/inspect", func(w http.ResponseWriter, r *http.Request) {
		rc.inspected = multipartFields(t, r)
		if rc.refuseInspect {
			reply(w, http.StatusBadRequest, map[string]string{"code": "wrong_passphrase", "message": "no " + testPassphrase})
			return
		}
		reply(w, http.StatusOK, map[string]any{
			"backup": rc.uploadName, "hostName": testHostName, "ilmHostName": testHostName, "nodeHostName": testRestoreHost,
			"ilm": "2.20.0", "operator": "1.1.0", "appliance": "0.1.0", "database": "managed", "databaseIncluded": true,
			"accounts": []string{"admin"}, "clashes": []string{}, "components": []string{"software-cryptography-provider"},
		})
	})
	mux.HandleFunc("POST /appliance/api/v1/restore/start", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&rc.started)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /appliance/api/v1/restore/resume", func(w http.ResponseWriter, r *http.Request) {
		rc.resumed = multipartFields(t, r)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /appliance/api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]any{"backup": map[string]any{"lastRestoreTest": time.Now().UTC().Format(time.RFC3339)}})
	})
	mux.HandleFunc("POST /appliance/api/v1/recovery-kit/reissue", func(w http.ResponseWriter, r *http.Request) {
		rc.reissued = multipartFields(t, r)
		_, _ = w.Write(rc.newKit)
	})
	mux.HandleFunc("GET /appliance/api/v1/recovery-kit", func(w http.ResponseWriter, _ *http.Request) {
		key, _ := recovery.Open(rc.newKit, testPassphrase)
		reply(w, http.StatusOK, map[string]any{
			"issued": true, "confirmed": rc.confirmed, "recipient": key.BackupKey.Recipient().String(), "issuance": testIssuance,
		})
	})
	mux.HandleFunc("POST /appliance/api/v1/recovery-kit/confirm", func(w http.ResponseWriter, _ *http.Request) {
		rc.confirmed = true
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newRestoreConsole returns a console, the options of a restore onto it and the secrets drive must not print.
func newRestoreConsole(t *testing.T) (*restoreConsole, options, []string) {
	t.Helper()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	build := func(kubeSecret string) []byte {
		data, err := recovery.Build(recovery.Kit{
			BackupKey: key, Kubeconfig: []byte("token: " + kubeSecret), Talosconfig: []byte("key: " + testTalosSecret),
		}, testPassphrase)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	dir := t.TempDir()
	files := map[string]string{"kit.pass": testPassphrase + "\n", testBackup: testBackupContent}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rc := &restoreConsole{kit: build(testKubeSecret), newKit: build("new-" + testKubeSecret)}
	kitFile := filepath.Join(dir, "kit.tar.age")
	if err := os.WriteFile(kitFile, rc.kit, 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{
		backupFile: filepath.Join(dir, testBackup), kitFile: kitFile, passphraseFile: filepath.Join(dir, "kit.pass"),
		ilmHost: testRestoreHost, out: filepath.Join(dir, "new-kit.tar.age"), hostName: testRestoreHost,
	}
	return rc, o, []string{testPassphrase, testKubeSecret, testTalosSecret, key.String(), "AGE-SECRET-KEY-"}
}

func fastPolls(t *testing.T) {
	t.Helper()
	saved := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = saved })
}

func TestDriveRestoreNeverPrintsSecrets(t *testing.T) {
	fastPolls(t)
	rc, o, secrets := newRestoreConsole(t)
	srv := rc.serve(t)
	cl := &fakeCluster{}
	var err error
	out := captureStdout(t, func() { err = newClient(srv.URL, nil, "").restore(context.Background(), o, cl) })
	if err != nil {
		t.Fatalf("restore = %v\n%s", err, out)
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed a secret:\n%s", out)
		}
	}
	for _, want := range []string{testBackup, "ILM 2.20.0", testHostName, "waitingForKit", "done", "last restore test"} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
	if rc.uploaded != testBackupContent || rc.uploadName != testBackup {
		t.Errorf("uploaded %q as %q, want the backup file under its name", rc.uploaded, rc.uploadName)
	}
	if rc.inspected["source"] != "upload" || rc.inspected["name"] != testBackup || rc.inspected["kit"] != string(rc.kit) ||
		rc.inspected["passphrase"] != testPassphrase {
		t.Errorf("inspected with parts %v, want the uploaded backup, the kit and its passphrase", keys(rc.inspected))
	}
	if rc.started["ilmHostName"] != testRestoreHost || rc.started["confirm"] != true || rc.started["inPlace"] != false {
		t.Errorf("started with %v, want a confirmed fresh restore onto %s", rc.started, testRestoreHost)
	}
	if rc.resumed["kit"] != string(rc.kit) || rc.resumed["passphrase"] != testPassphrase {
		t.Error("the restore that waited for its kit was not resumed with the kit and its passphrase")
	}
	if rc.reissued["kit"] != string(rc.kit) || rc.reissued["passphrase"] != testPassphrase || rc.reissued["newPassphrase"] != testPassphrase {
		t.Errorf("reissued with parts %v, want the backup's kit, its passphrase and the new passphrase", keys(rc.reissued))
	}
	if data, err := os.ReadFile(o.out); err != nil || string(data) != string(rc.newKit) {
		t.Errorf("the reissued kit was not written to --out: %v", err)
	}
	if info, err := os.Stat(o.out); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("reissued kit file = %v, %v, want mode 0600", info, err)
	}
	if !rc.confirmed {
		t.Error("the reissued kit was not confirmed")
	}
	if len(cl.kubeconfigs) != 1 || !strings.Contains(cl.kubeconfigs[0], "new-"+testKubeSecret) {
		t.Errorf("checked kubeconfigs = %d, want the reissued kit's", len(cl.kubeconfigs))
	}
	if !strings.Contains(strings.Join(cl.logSecrets, " "), testPassphrase) {
		t.Error("the manager logs were not searched for the passphrase")
	}
}

func TestDriveRestoreFails(t *testing.T) {
	fastPolls(t)
	for name, tc := range map[string]struct {
		change func(*restoreConsole)
		want   string
	}{
		"a refused inspection": {func(rc *restoreConsole) { rc.refuseInspect = true }, "wrong_passphrase"},
		"a failed restore":     {func(rc *restoreConsole) { rc.failRestore = true }, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			rc, o, secrets := newRestoreConsole(t)
			tc.change(rc)
			srv := rc.serve(t)
			var err error
			out := captureStdout(t, func() { err = newClient(srv.URL, nil, "").restore(context.Background(), o, &fakeCluster{}) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("restore = %v, want an error naming %q", err, tc.want)
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error(), secret) || strings.Contains(out, secret) {
					t.Errorf("drive showed a secret: %v\n%s", err, out)
				}
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestValidateRequiresTheRestoreFlags(t *testing.T) {
	signedIn := options{user: "u", password: "p", seedFile: "s", hostName: "h"}
	o := signedIn
	o.backupFile, o.kitFile, o.passphraseFile, o.out = "b", "k", "p", "o"
	if err := o.validate("restore"); err == nil {
		t.Error("restore without --ilm-host passed validation")
	}
	o.ilmHost = testRestoreHost
	if err := o.validate("restore"); err != nil {
		t.Errorf("restore with its flags = %v", err)
	}
	if err := signedIn.validate("restore"); err == nil {
		t.Error("restore without its flags passed validation")
	}
}
