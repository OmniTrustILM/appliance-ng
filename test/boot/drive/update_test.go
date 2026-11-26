package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/setup"
)

// platformConsole models the updates API through an update and its rollback: the uploaded bundle is loaded, an
// update steps to done, a rollback waits for its kit once, is resumed and steps to done, and the status shows the ILM
// version that runs.
type platformConsole struct {
	mu                  sync.Mutex
	started             map[string]any
	rolledBack, resumed map[string]string
	steps               []string
	failAt              string
	offer               map[string]any
	ilm                 string
	kind, from, to      string
}

func (pc *platformConsole) serve(t *testing.T) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, body any) { _ = json.NewEncoder(w).Encode(body) }
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /appliance/api/v1/updates/upload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /appliance/api/v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		body := map[string]any{
			"available": []any{}, "loaded": []map[string]any{{"kind": "platform", "version": "0.1.1", "arch": "arm64", "ilm": "2.20.0"}},
			"load":     map[string]any{"state": "loaded", "kind": "platform", "version": "0.1.1"},
			"rollback": pc.offer,
		}
		if pc.kind != "" {
			u := map[string]any{"kind": pc.kind, "from": pc.from, "to": pc.to, "step": pc.steps[0]}
			if pc.steps[0] == pc.failAt {
				u["message"] = "timeout"
			}
			if len(pc.steps) > 1 && pc.steps[0] != restoreStepPause {
				pc.steps = pc.steps[1:]
			}
			if pc.steps[0] == restoreStepDone {
				pc.ilm = map[string]string{kindPlatform: "2.20.0", kindRollback: "2.19.0"}[pc.kind]
			}
			body["update"] = u
		}
		reply(w, body)
	})
	mux.HandleFunc("POST /appliance/api/v1/updates/platform", func(w http.ResponseWriter, r *http.Request) {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&pc.started)
		pc.kind, pc.from, pc.to = kindPlatform, "0.1.0", "0.1.1"
		pc.steps = []string{"backup", "images", "operator", "version", "wait", restoreStepDone}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /appliance/api/v1/updates/rollback", func(w http.ResponseWriter, r *http.Request) {
		fields := multipartFields(t, r)
		pc.mu.Lock()
		defer pc.mu.Unlock()
		pc.rolledBack = fields
		pc.kind, pc.from, pc.to = kindRollback, "0.1.1", "0.1.0"
		pc.steps = []string{"stop", "uninstall", restoreStepPause}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /appliance/api/v1/restore/resume", func(w http.ResponseWriter, r *http.Request) {
		fields := multipartFields(t, r)
		pc.mu.Lock()
		defer pc.mu.Unlock()
		pc.resumed = fields
		pc.steps = []string{"reinstall", "database", "start", restoreStepDone}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /appliance/api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		pc.mu.Lock()
		defer pc.mu.Unlock()
		reply(w, map[string]any{"versions": map[string]string{"ilm": pc.ilm}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// updateSecrets writes a kit and its passphrase and returns the options of an update and a rollback, the kit and
// the secrets drive must not print.
func updateSecrets(t *testing.T) (options, []byte, []string) {
	t.Helper()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	kit, err := recovery.Build(recovery.Kit{
		BackupKey: key, Kubeconfig: []byte("token: " + testKubeSecret), Talosconfig: []byte("key: " + testTalosSecret),
	}, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, _ := writeTestBundle(t)
	o := options{
		bundleFile: path, kitFile: filepath.Join(dir, "kit.tar.age"), passphraseFile: filepath.Join(dir, "kit.pass"),
		password: "console-password-0d2e", ilmVersion: "2.19.0",
	}
	for file, content := range map[string][]byte{o.kitFile: kit, o.passphraseFile: []byte(testPassphrase + "\n")} {
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return o, kit, []string{testPassphrase, testKubeSecret, testTalosSecret, key.String(), backupKeyPrefix, o.password}
}

func TestDriveUpdateNeverPrintsSecrets(t *testing.T) {
	fastPolls(t)
	o, kit, secrets := updateSecrets(t)
	pc := &platformConsole{offer: map[string]any{"available": true}}
	c := newClient(pc.serve(t).URL, nil, "")
	var err error
	out := captureStdout(t, func() {
		if err = c.update(context.Background(), o); err == nil {
			err = c.rollBack(context.Background(), o, kit, testPassphrase)
		}
	})
	if err != nil {
		t.Fatalf("update and rollback = %v\n%s", err, out)
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed a secret:\n%s", out)
		}
	}
	for _, want := range []string{
		"platform bundle 0.1.1-arm64 with ILM 2.20.0", "platform from 0.1.0 to 0.1.1 at step backup", "runs ILM 2.20.0",
		"rollback from 0.1.1 to 0.1.0 at step uninstall", "waitingForKit", "at step database", "runs ILM 2.19.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
	if pc.started["version"] != "0.1.1" || pc.started["confirm"] != true {
		t.Errorf("update started with %v, want the loaded bundle confirmed", pc.started)
	}
	if pc.rolledBack["kit"] != string(kit) || pc.rolledBack[partPassphrase] != testPassphrase || pc.rolledBack["confirm"] != "true" {
		t.Errorf("rolled back with parts %v, want the kit, its passphrase and the confirmation", keys(pc.rolledBack))
	}
	if pc.resumed["kit"] != string(kit) || pc.resumed[partPassphrase] != testPassphrase {
		t.Error("the rollback that waited for its kit was not resumed with the kit and its passphrase")
	}
}

func TestDriveUpdateFails(t *testing.T) {
	fastPolls(t)
	for name, tc := range map[string]struct {
		pc       *platformConsole
		rollback bool
		want     string
	}{
		"a failed update":        {&platformConsole{failAt: "images"}, false, "stopped at step images: timeout"},
		"a rollback not offered": {&platformConsole{offer: map[string]any{"available": false, "reason": "operatorChanged"}}, true, "operatorChanged"},
		"no rollback in answer":  {&platformConsole{}, true, "no rollback"},
		"a failed rollback":      {&platformConsole{offer: map[string]any{"available": true}, failAt: "uninstall"}, true, "stopped at step uninstall"},
	} {
		t.Run(name, func(t *testing.T) {
			o, kit, secrets := updateSecrets(t)
			c := newClient(tc.pc.serve(t).URL, nil, "")
			var err error
			out := captureStdout(t, func() {
				if tc.rollback {
					err = c.rollBack(context.Background(), o, kit, testPassphrase)
				} else {
					err = c.update(context.Background(), o)
				}
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q\n%s", err, tc.want, out)
			}
			for _, secret := range secrets {
				if strings.Contains(err.Error()+out, secret) {
					t.Errorf("drive showed a secret: %v\n%s", err, out)
				}
			}
		})
	}
}

// TestCoreClientSendsTheCertificateAsURLEncodedPEM sends the credential's certificate in the header ILM 2.19.0 Core
// decodes, on every request.
func TestCoreClientSendsTheCertificateAsURLEncodedPEM(t *testing.T) {
	cert, _, err := setup.SelfSigned([]string{testNodeIP})
	if err != nil {
		t.Fatal(err)
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(clientCertHeader))
		_, _ = w.Write([]byte(`[{"name":"` + markerGroup + `"}]`))
	}))
	t.Cleanup(srv.Close)
	out := captureStdout(t, func() { err = markerPresent(context.Background(), coreClient(cert), srv.URL) })
	if err != nil {
		t.Fatalf("markerPresent = %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	decoded, err := url.QueryUnescape(got[0])
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(decoded))
	if block == nil || string(block.Bytes) != string(cert.Leaf.Raw) || strings.ContainsAny(got[0], "\n +/") {
		t.Errorf("header %q is not the URL-encoded PEM of the certificate", got[0])
	}
}

func TestValidateRequiresTheUpdateFlags(t *testing.T) {
	signedIn := options{user: "u", password: "p", seedFile: "s", hostName: "h"}
	for cmd, complete := range map[string]func(*options){
		cmdUpdate:     func(o *options) { o.bundleFile = "b" },
		cmdRollback:   func(o *options) { o.kitFile, o.passphraseFile, o.ilmVersion = "k", "p", "2.19.0" },
		cmdCredential: func(o *options) { o.credential, o.credentialPasswordFile = "c", "p" },
	} {
		if err := signedIn.validate(cmd); err == nil {
			t.Errorf("%s without its flags passed validation", cmd)
		}
		o := signedIn
		complete(&o)
		if err := o.validate(cmd); err != nil {
			t.Errorf("%s with its flags = %v", cmd, err)
		}
	}
	marker := options{hostName: "h", credential: "c", credentialPasswordFile: "p", uiURL: "u", uiAddr: "a"}
	for _, o := range []options{
		{}, {present: true, check: true}, {direct: true, present: true}, {direct: true, check: true, kitFile: "k", passphraseFile: "p"},
	} {
		o.hostName, o.credential, o.credentialPasswordFile, o.uiURL, o.uiAddr = marker.hostName, marker.credential, marker.credentialPasswordFile, marker.uiURL, marker.uiAddr
		if err := o.validate("ilm-marker"); err == nil {
			t.Errorf("ilm-marker %+v passed validation", o)
		}
	}
	marker.present, marker.direct, marker.kitFile, marker.passphraseFile = true, true, "k", "p"
	if err := marker.validate("ilm-marker"); err != nil {
		t.Errorf("ilm-marker --present --direct with the kit = %v", err)
	}
}
