package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	testHostName = "ilm.appliance.internal"
	testDump     = "INSERT INTO certificate VALUES ('dump-content-3e8a1f');"
	testBackup   = "ilm-20260101T000000Z-v1.ilmbackup"
)

// backupConsole models the console's backup API; a backup it starts is copied to both servers' directories.
type backupConsole struct {
	key         recovery.Kit
	dir         string
	fingerprint string
	manifest    backup.Manifest
	hostName    string
	failTarget  string
	archive     []byte
	puts        map[string]map[string]any
	tested      []string
}

func (b *backupConsole) build(t *testing.T) {
	t.Helper()
	spec, err := yaml.Marshal(v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: b.hostName}}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, b.key.BackupKey.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add("appliance.yaml", int64(len(spec)), bytes.NewReader(spec)); err != nil {
		t.Fatal(err)
	}
	if b.manifest.DatabaseIncluded {
		if err := w.Add("database.sql", -1, strings.NewReader(testDump)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(b.manifest); err != nil {
		t.Fatal(err)
	}
	b.archive = buf.Bytes()
}

func (b *backupConsole) serve(t *testing.T) *httptest.Server {
	t.Helper()
	b.build(t)
	b.puts = map[string]map[string]any{}
	started := false
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, status int, body any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("POST /appliance/api/v1/backups/targets/hostkey", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]string{"fingerprint": b.fingerprint})
	})
	mux.HandleFunc("PUT /appliance/api/v1/backups/targets/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.puts[r.PathValue("name")] = body
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /appliance/api/v1/backups/targets/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		b.tested = append(b.tested, r.PathValue("name"))
		reply(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /appliance/api/v1/backups", func(w http.ResponseWriter, _ *http.Request) {
		started = true
		for _, dir := range []string{sftpDir(b.dir), s3Files(b.dir)} {
			if err := os.WriteFile(filepath.Join(dir, testBackup), b.archive, 0o600); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /appliance/api/v1/backups", func(w http.ResponseWriter, _ *http.Request) {
		list := map[string]any{"running": false, "local": []map[string]any{}, "targets": []map[string]any{}}
		if started {
			list["local"] = []map[string]any{{"name": testBackup, "sizeBytes": len(b.archive), "database": "included"}}
			health := map[string]any{"lastSuccess": time.Now().Format(time.RFC3339)}
			if b.failTarget != "" {
				health = map[string]any{"lastError": "unreachable"}
			}
			list["targets"] = []map[string]any{{"name": "sftp", "health": health}, {"name": "s3", "health": health}}
		}
		reply(w, http.StatusOK, list)
	})
	mux.HandleFunc("GET /appliance/api/v1/backups/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != testBackup {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b.archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newBackupConsole(t *testing.T) (*backupConsole, options) {
	t.Helper()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, d := range []string{sftpDir(dir), s3Files(dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"sftp.password": testSFTPPassword, "s3.secret": testS3Secret}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := &backupConsole{
		key: recovery.Kit{BackupKey: key}, dir: dir, fingerprint: "SHA256:served", hostName: testHostName,
		manifest: backup.Manifest{Format: backup.FormatVersion, Database: "managed", DatabaseIncluded: true, HostName: testHostName},
	}
	return b, options{
		dir: dir, hostName: testHostName, sftpFingerprint: "SHA256:served", sftpGuest: "10.0.2.2:2222", s3Guest: "10.0.2.2:9000",
		sftpPasswordFile: filepath.Join(dir, "sftp.password"), s3SecretFile: filepath.Join(dir, "s3.secret"),
	}
}

func TestDriveBackupNeverPrintsSecrets(t *testing.T) {
	b, o := newBackupConsole(t)
	srv := b.serve(t)
	cl := &fakeCluster{}
	var err error
	out := captureStdout(t, func() { err = newClient(srv.URL, nil, "").backUp(context.Background(), o, b.key, cl) })
	if err != nil {
		t.Fatalf("backUp = %v", err)
	}
	for _, secret := range []string{testSFTPPassword, testS3Secret, testPassphrase, b.key.BackupKey.String(), "AGE-SECRET-KEY-", "INSERT INTO", "dump-content"} {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{testBackup, "database managed, included true", testHostName} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
	sftp, _ := b.puts["sftp"]["sftp"].(map[string]any)
	s3, _ := b.puts["s3"]["s3"].(map[string]any)
	if sftp["password"] != testSFTPPassword || sftp["port"] != float64(2222) || sftp["path"] != "/" || sftp["hostKey"] != "SHA256:served" {
		t.Errorf("the SFTP target saved = %v", sftp)
	}
	if s3["secretKey"] != testS3Secret || s3["endpoint"] != "http://10.0.2.2:9000" || s3["prefix"] != s3Prefix {
		t.Errorf("the S3 target saved = %v", s3)
	}
	if got := strings.Join(cl.logSecrets, " "); !strings.Contains(got, testSFTPPassword) || !strings.Contains(got, testS3Secret) ||
		!strings.Contains(got, "INSERT INTO") || !strings.Contains(got, "COPY public.") {
		t.Errorf("the manager logs were searched for %q, want both target secrets and the dump markers", cl.logSecrets)
	}
	if len(b.tested) != 2 {
		t.Errorf("targets tested = %v, want both", b.tested)
	}
}

func TestDriveBackupFails(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*backupConsole, *options)
		want   string
	}{
		"another host key":     {func(_ *backupConsole, o *options) { o.sftpFingerprint = "SHA256:other" }, "host key"},
		"no database":          {func(b *backupConsole, _ *options) { b.manifest.DatabaseIncluded = false }, "included=false"},
		"external database":    {func(b *backupConsole, _ *options) { b.manifest.Database = "external" }, "managed database"},
		"another host name":    {func(b *backupConsole, _ *options) { b.hostName = "other.example" }, "host name"},
		"a target that failed": {func(b *backupConsole, _ *options) { b.failTarget = "s3" }, "unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			b, o := newBackupConsole(t)
			tc.change(b, &o)
			srv := b.serve(t)
			var err error
			cl := &fakeCluster{}
			captureStdout(t, func() { err = newClient(srv.URL, nil, "").backUp(context.Background(), o, b.key, cl) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("backUp = %v, want an error naming %q", err, tc.want)
			}
			for _, secret := range []string{testSFTPPassword, testS3Secret, "INSERT INTO"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error shows a secret: %v", err)
				}
			}
		})
	}
}

func TestValidateRequiresTheBackupFlags(t *testing.T) {
	o := options{user: "u", password: "p", seedFile: "s", hostName: "h"}
	if err := o.validate("backup"); err == nil {
		t.Error("backup without its flags passed validation")
	}
	o = options{kitFile: "k", passphraseFile: "p", dir: "d", sftpFingerprint: "f", sftpPasswordFile: "a", s3SecretFile: "b", user: "u", password: "p", seedFile: "s", hostName: "h"}
	if err := o.validate("backup"); err != nil {
		t.Errorf("backup with its flags = %v", err)
	}
	if err := (options{}).validate("serve-targets"); err == nil {
		t.Error("serve-targets without its flags passed validation")
	}
	if err := (options{dir: "d", sftpPasswordFile: "a", s3SecretFile: "b"}).validate("serve-targets"); err != nil {
		t.Errorf("serve-targets with its flags = %v", err)
	}
}
