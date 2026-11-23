package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

const (
	pathBackups      = "/appliance/api/v1/backups"
	pathTargets      = pathBackups + "/targets"
	pathHostKey      = pathTargets + "/hostkey"
	s3AccessKey      = "AKIA-access-key-never-leaves"
	s3SecretKey      = "s3-secret-key-never-leaves"
	sftpPassword     = "sftp-Pa55word-never-leaves"
	targetLeak       = "driver-error-text-never-leaves"
	backupHost       = "appliance.example.com"
	backupTestVer    = "0.1.0"
	sftpHostKey      = "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	backupTargetName = "offsite"
)

// fakeRunner serves the local backups of a directory and records the backups it is asked to start.
type fakeRunner struct {
	*backup.Runner
	startErr    error
	started     [][]string
	testErr     error
	tested      []string
	fingerprint string
	scanErr     error
	scanned     []string
	scanTime    time.Duration
}

func newFakeRunner(t *testing.T) (*fakeRunner, string) {
	t.Helper()
	dir := t.TempDir()
	return &fakeRunner{Runner: backup.NewRunner(backup.RunnerDeps{Dir: dir, Log: slog.New(slog.DiscardHandler)})}, dir
}

func (f *fakeRunner) Start(_ context.Context, reason string, targets []string, opts ...backup.RunOption) error {
	f.started = append(f.started, append([]string{reason, fmt.Sprint(len(opts))}, targets...))
	return f.startErr
}

func (f *fakeRunner) TestTarget(_ context.Context, name string) error {
	f.tested = append(f.tested, name)
	return f.testErr
}

func (f *fakeRunner) ScanHostKey(_ context.Context, host string, port int32) (string, error) {
	f.scanned = append(f.scanned, fmt.Sprintf("%s:%d", host, port))
	time.Sleep(f.scanTime)
	return f.fingerprint, f.scanErr
}

// withRunner gives the harness's server a fakeRunner.
func withRunner(t *testing.T, h *harness) (*fakeRunner, string) {
	t.Helper()
	f, dir := newFakeRunner(t)
	h.srv.deps.Backups = f
	return f, dir
}

func s3Update(accessKey, secretKey *string) BackupTargetUpdate {
	u := BackupTargetUpdate{Type: BackupTargetUpdateTypeS3, TimeZone: "Europe/Prague"}
	u.S3 = &struct {
		AccessKey *string `json:"accessKey,omitempty"`
		Bucket    string  `json:"bucket"`
		Ca        *string `json:"ca,omitempty"`
		Endpoint  string  `json:"endpoint"`
		Prefix    *string `json:"prefix,omitempty"`
		Region    *string `json:"region,omitempty"`
		RemoveCa  *bool   `json:"removeCa,omitempty"`
		SecretKey *string `json:"secretKey,omitempty"`
	}{AccessKey: accessKey, SecretKey: secretKey, Bucket: "ilm", Endpoint: "https://s3.example.com", Prefix: ptr("/appliance/")}
	return u
}

func sftpUpdate(password, privateKey *string) BackupTargetUpdate {
	u := BackupTargetUpdate{Type: BackupTargetUpdateTypeSftp, TimeZone: "UTC", Schedule: ptr("30 1 * * *"), Retention: ptr(7)}
	u.Sftp = &struct {
		Host       string  `json:"host"`
		HostKey    string  `json:"hostKey"`
		Password   *string `json:"password,omitempty"`
		Path       string  `json:"path"`
		Port       *int    `json:"port,omitempty"`
		PrivateKey *string `json:"privateKey,omitempty"`
		User       string  `json:"user"`
	}{Host: "sftp.example.com", HostKey: sftpHostKey, Password: password, PrivateKey: privateKey, Path: "/backups", User: "ilm"}
	return u
}

func nfsUpdate() BackupTargetUpdate {
	return BackupTargetUpdate{Type: BackupTargetUpdateTypeNfs, TimeZone: "UTC", Nfs: &NFSSettings{Server: "nfs.example.com", Export: "/export"}}
}

// sshKey is a new unencrypted OpenSSH private key in PEM.
func sshKey(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func getTarget(t *testing.T, h *harness, s *session, name string) BackupTarget {
	t.Helper()
	rec := h.request(http.MethodGet, pathTargets+"/"+name, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var out BackupTarget
	decode(t, rec, &out)
	return out
}

func targetSpec(t *testing.T, name string) *v1alpha1.BackupTarget {
	t.Helper()
	for _, target := range backupTargets(getAppliance(t)) {
		if target.Name == name {
			return &target
		}
	}
	return nil
}

func TestTargetSecretsWriteOnly(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	ca := newTestCA(t)
	secretName := backupTargetSecretName(backupTargetName)

	u := s3Update(ptr(s3AccessKey), ptr(s3SecretKey))
	u.S3.Ca = ptr(string(ca.pem))
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, u, s), http.StatusAccepted)
	got := getTarget(t, h, s, backupTargetName)
	if want := (BackupSecretsSet{AccessKey: true, SecretKey: true, Ca: true}); got.SecretsSet != want {
		t.Errorf("secrets set %+v", got.SecretsSet)
	}
	if got.Schedule != backup.DefaultSchedule || got.TimeZone != "Europe/Prague" || got.Retention != backup.DefaultRetention || got.S3 == nil || deref(got.S3.Prefix) != "appliance" {
		t.Errorf("target %+v", got)
	}
	spec := targetSpec(t, backupTargetName)
	if spec.CredentialsRef != secretName || spec.S3.CA == nil || spec.S3.CA.Name != secretName || spec.S3.CA.Key != target.CAKey {
		t.Errorf("spec %+v", spec)
	}
	if data := systemSecret(t, secretName).Data; string(data[target.AccessKeyKey]) != s3AccessKey || string(data[target.SecretKeyKey]) != s3SecretKey || !bytes.Equal(data[target.CAKey], ca.pem) {
		t.Errorf("secret %v", data)
	}

	// Without secrets, or with empty ones, the stored ones stay; a new secret key replaces only that one.
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, s3Update(nil, nil), s), http.StatusAccepted)
	empty := s3Update(ptr(""), ptr(""))
	empty.S3.Ca = ptr("")
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, empty, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, s3Update(nil, ptr("next-secret")), s), http.StatusAccepted)
	if data := systemSecret(t, secretName).Data; string(data[target.AccessKeyKey]) != s3AccessKey || string(data[target.SecretKeyKey]) != "next-secret" || len(data[target.CAKey]) == 0 {
		t.Errorf("secret after the updates %v", data)
	}
	// The stored CA goes only on request, and not together with a new one.
	both := s3Update(nil, nil)
	both.S3.Ca, both.S3.RemoveCa = ptr(string(ca.pem)), ptr(true)
	wantTargetField(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, both, s), "s3.ca")
	remove := s3Update(nil, nil)
	remove.S3.RemoveCa = ptr(true)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, remove, s), http.StatusAccepted)
	if data := systemSecret(t, secretName).Data; len(data[target.CAKey]) != 0 || targetSpec(t, backupTargetName).S3.CA != nil || getTarget(t, h, s, backupTargetName).SecretsSet.Ca {
		t.Error("the CA stays after its removal")
	}

	// A new SFTP target needs a credential; a private key replaces the password.
	rec := h.request(http.MethodPut, pathTargets+"/sftp", sftpUpdate(nil, nil), s)
	wantTargetField(t, rec, "sftp.password")
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/sftp", sftpUpdate(ptr(sftpPassword), nil), s), http.StatusAccepted)
	if got := getTarget(t, h, s, "sftp"); !got.SecretsSet.Password || got.SecretsSet.PrivateKey || got.Sftp == nil || got.Sftp.Port == nil || *got.Sftp.Port != 22 || got.Retention != 7 {
		t.Errorf("sftp target %+v", got)
	}
	key := sshKey(t)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/sftp", sftpUpdate(nil, ptr(key)), s), http.StatusAccepted)
	if data := systemSecret(t, backupTargetSecretName("sftp")).Data; len(data[target.PasswordKey]) != 0 || string(data[target.PrivateKeyKey]) != key {
		t.Errorf("sftp secret keys %v", slices.Sorted(maps.Keys(data)))
	}

	// Another type drops the secrets.
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/sftp", nfsUpdate(), s), http.StatusAccepted)
	if !secretGone(t, backupTargetSecretName("sftp")) || targetSpec(t, "sftp").CredentialsRef != "" {
		t.Error("the SFTP secrets outlived the change to NFS")
	}
	if got := getTarget(t, h, s, "sftp"); got.Nfs == nil || got.Nfs.Version == nil || *got.Nfs.Version != "4.1" {
		t.Errorf("nfs target %+v", got)
	}

	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/"+backupTargetName, nil, s), http.StatusAccepted)
	if !secretGone(t, secretName) || targetSpec(t, backupTargetName) != nil {
		t.Error("the removed target or its secret is left")
	}
	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/"+backupTargetName, nil, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodGet, pathTargets+"/"+backupTargetName, nil, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/sftp", nil, s), http.StatusAccepted)
	if getAppliance(t).Spec.Backup != nil {
		t.Error("no targets left, but spec.backup stays")
	}

	var actions []string
	for _, e := range h.entries() {
		if strings.HasPrefix(e.Action, "backup-") {
			actions = append(actions, e.Action+" "+e.Result)
			if e.Action != string(AuditEntryActionBackupTargetRemove) && e.Result == "success" && !slices.Equal(e.Paths, []string{pathBackupTargets}) {
				t.Errorf("entry %+v without the changed path", e)
			}
		}
	}
	if len(actions) != 13 {
		t.Errorf("audit %v", actions)
	}
}

func wantTargetField(t *testing.T, rec *httptest.ResponseRecorder, field string) {
	t.Helper()
	wantStatus(t, rec, http.StatusBadRequest)
	var e Error
	decode(t, rec, &e)
	if deref(e.Field) != field {
		t.Errorf("field %q (%s), want %q", deref(e.Field), e.Message, field)
	}
}

func TestStoredSecretsStayWithTheirServer(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	put := func(u BackupTargetUpdate) *httptest.ResponseRecorder {
		return h.request(http.MethodPut, pathTargets+"/"+backupTargetName, u, s)
	}
	ca := newTestCA(t)
	u := s3Update(ptr(s3AccessKey), ptr(s3SecretKey))
	u.S3.Ca = ptr(string(ca.pem))
	wantStatus(t, put(u), http.StatusAccepted)

	// Another endpoint needs the keys again; the CA bundle, which is not sent anywhere, stays.
	moved := s3Update(nil, nil)
	moved.S3.Endpoint = "https://s3.other.example"
	wantTargetField(t, put(moved), "s3.accessKey")
	moved.S3.AccessKey = ptr("other-access")
	wantTargetField(t, put(moved), "s3.secretKey")
	moved.S3.SecretKey = ptr("other-secret")
	wantStatus(t, put(moved), http.StatusAccepted)
	if data := systemSecret(t, backupTargetSecretName(backupTargetName)).Data; string(data[target.AccessKeyKey]) != "other-access" ||
		string(data[target.SecretKeyKey]) != "other-secret" || !bytes.Equal(data[target.CAKey], ca.pem) {
		t.Errorf("secret %v", data)
	}
	// The same endpoint keeps them, whatever else changes.
	same := s3Update(nil, nil)
	same.S3.Endpoint, same.S3.Bucket = "https://s3.other.example", "other-bucket"
	wantStatus(t, put(same), http.StatusAccepted)

	// Another type needs its own secrets.
	wantTargetField(t, put(sftpUpdate(nil, nil)), "sftp.password")
	wantStatus(t, put(sftpUpdate(ptr("pw"), nil)), http.StatusAccepted)
	for name, change := range map[string]func(*BackupTargetUpdate){
		"host": func(u *BackupTargetUpdate) { u.Sftp.Host = "sftp.other.example" },
		"port": func(u *BackupTargetUpdate) { u.Sftp.Port = ptr(2222) },
		"user": func(u *BackupTargetUpdate) { u.Sftp.User = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			u := sftpUpdate(nil, nil)
			change(&u)
			wantTargetField(t, put(u), "sftp.password")
		})
	}
	u = sftpUpdate(nil, nil)
	u.Sftp.Path = "/elsewhere"
	wantStatus(t, put(u), http.StatusAccepted)
}

func TestBackupTargetRejects(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	key := ptr("k")
	cases := map[string]struct {
		name  string
		body  func() BackupTargetUpdate
		field string
	}{
		"bad name":         {"Off_Site", func() BackupTargetUpdate { return nfsUpdate() }, "name"},
		"no zone":          {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.TimeZone = ""; return u }, fieldTimeZone},
		"unknown zone":     {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.TimeZone = "Mars/Base"; return u }, fieldTimeZone},
		"local zone":       {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.TimeZone = "Local"; return u }, fieldTimeZone},
		"six fields":       {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Schedule = ptr("0 0 2 * * *"); return u }, "schedule"},
		"descriptor":       {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Schedule = ptr("@daily"); return u }, "schedule"},
		"zone in schedule": {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Schedule = ptr("TZ=UTC 0 2 * * *"); return u }, "schedule"},
		"retention":        {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Retention = ptr(366); return u }, "retention"},
		"type":             {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Type = "ftp"; return u }, "type"},
		"no s3":            {"t", func() BackupTargetUpdate { u := s3Update(key, key); u.S3 = nil; return u }, "s3.endpoint"},
		"s3 endpoint path": {"t", func() BackupTargetUpdate {
			u := s3Update(key, key)
			u.S3.Endpoint = "https://s3.example.com/x"
			return u
		}, "s3.endpoint"},
		"s3 endpoint fragment": {"t", func() BackupTargetUpdate {
			u := s3Update(key, key)
			u.S3.Endpoint = "https://s3.example.com#x"
			return u
		}, "s3.endpoint"},
		"s3 bucket":        {"t", func() BackupTargetUpdate { u := s3Update(key, key); u.S3.Bucket = " "; return u }, "s3.bucket"},
		"s3 ca":            {"t", func() BackupTargetUpdate { u := s3Update(key, key); u.S3.Ca = ptr("not pem"); return u }, "s3.ca"},
		"s3 no access key": {"t", func() BackupTargetUpdate { return s3Update(nil, key) }, "s3.accessKey"},
		"s3 no secret key": {"t", func() BackupTargetUpdate { return s3Update(key, nil) }, "s3.secretKey"},
		"s3 long key":      {"t", func() BackupTargetUpdate { return s3Update(ptr(strings.Repeat("k", maxSecretLength+1)), key) }, "s3.accessKey"},
		"no sftp":          {"t", func() BackupTargetUpdate { u := sftpUpdate(key, nil); u.Sftp = nil; return u }, "sftp.host"},
		"sftp port":        {"t", func() BackupTargetUpdate { u := sftpUpdate(key, nil); u.Sftp.Port = ptr(0); return u }, "sftp.port"},
		"sftp path":        {"t", func() BackupTargetUpdate { u := sftpUpdate(key, nil); u.Sftp.Path = ""; return u }, "sftp.path"},
		"sftp user":        {"t", func() BackupTargetUpdate { u := sftpUpdate(key, nil); u.Sftp.User = "a\nb"; return u }, "sftp.user"},
		"sftp host key":    {"t", func() BackupTargetUpdate { u := sftpUpdate(key, nil); u.Sftp.HostKey = "SHA256:"; return u }, "sftp.hostKey"},
		"sftp both":        {"t", func() BackupTargetUpdate { return sftpUpdate(key, key) }, "sftp.privateKey"},
		"sftp bad key":     {"t", func() BackupTargetUpdate { return sftpUpdate(nil, ptr("not a key")) }, "sftp.privateKey"},
		"no nfs":           {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Nfs = nil; return u }, "nfs.server"},
		"nfs export":       {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Nfs.Export = "export"; return u }, "nfs.export"},
		"nfs version": {"t", func() BackupTargetUpdate {
			u := nfsUpdate()
			v := NFSSettingsVersion("2")
			u.Nfs.Version = &v
			return u
		}, "nfs.version"},
		"nfs server scheme": {"t", func() BackupTargetUpdate { u := nfsUpdate(); u.Nfs.Server = "nfs://x"; return u }, "nfs.server"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			wantTargetField(t, h.request(http.MethodPut, pathTargets+"/"+c.name, c.body(), s), c.field)
		})
	}
	wantTargetField(t, h.request(http.MethodPut, pathTargets+"/t", "null", s), "type")
	if getAppliance(t).Spec.Backup != nil {
		t.Error("a refused target was saved")
	}
}

func TestBackupTargetLimits(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/t", nfsUpdate(), s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodGet, pathTargets+"/t", nil, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/t", nil, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodGet, pathBackups, nil, s), http.StatusConflict)
	createAppliance(t, v1alpha1.SetupComplete)
	for i := range maxBackupTargets {
		wantStatus(t, h.request(http.MethodPut, fmt.Sprintf("%s/t%d", pathTargets, i), nfsUpdate(), s), http.StatusAccepted)
	}
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/one-more", s3Update(ptr(s3AccessKey), ptr(s3SecretKey)), s), http.StatusConflict)
	if !secretGone(t, backupTargetSecretName("one-more")) {
		t.Error("a refused target left its secrets behind")
	}
	// Replacing one of them is still possible; a body with a private key and a CA bundle fits.
	big := sftpUpdate(nil, ptr(sshKey(t)))
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/t0", big, s), http.StatusAccepted)
	padded := s3Update(ptr("a"), ptr("b"))
	padded.S3.Ca = ptr(string(newTestCA(t).pem) + strings.Repeat(" ", 2*maxBody))
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/t1", padded, s), http.StatusAccepted)
}

// writeBackup writes a backup file of size random bytes taken at at into dir.
func writeBackup(t *testing.T, dir string, at time.Time, size int) (string, []byte) {
	t.Helper()
	name := backup.FileName(backupHost, at, backupTestVer)
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return name, data
}

func TestDownloadStreams(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	_, dir := withRunner(t, h)
	name, data := writeBackup(t, dir, time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC), 32<<20)

	// A server whose write timeout would cut the download off long before a slow client has read it.
	srv := httptest.NewUnstartedServer(h.handler)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+pathBackups+"/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/octet-stream" ||
		res.Header.Get("Content-Length") != fmt.Sprint(len(data)) || res.Header.Get("Content-Disposition") != `attachment; filename=`+name {
		t.Fatalf("answer %d %v", res.StatusCode, res.Header)
	}
	head := make([]byte, 1<<20)
	if _, err := io.ReadFull(res.Body, head); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	rest, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("the download broke off: %v", err)
	}
	if got := append(head, rest...); !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, want the %d of the file", len(got), len(data))
	}

	entries := h.entries()
	if e := entries[len(entries)-1]; e.Action != string(AuditEntryActionBackupDownload) || e.User != testUser || e.Details["name"] != name || e.Result != "success" {
		t.Errorf("audit entry %+v", e)
	}
}

func TestSlowHostKeyScanAnswers(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	f, _ := withRunner(t, h)
	f.fingerprint, f.scanTime = sftpHostKey, 400*time.Millisecond

	// A server whose write timeout ends before the scan does, as reaching an SSH server through a proxy can.
	srv := httptest.NewUnstartedServer(h.handler)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+pathHostKey, strings.NewReader(`{"host":"sftp.example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.CSRFHeader, s.csrf)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("the answer broke off: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var key HostKey
	if err := json.NewDecoder(res.Body).Decode(&key); err != nil || res.StatusCode != http.StatusOK || key.Fingerprint != sftpHostKey {
		t.Errorf("answer %d %+v %v", res.StatusCode, key, err)
	}
}

func TestDownloadOnlyListedBackups(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	wantStatus(t, h.request(http.MethodGet, pathBackups+"/x"+backup.FileSuffix, nil, s), http.StatusServiceUnavailable)
	_, dir := withRunner(t, h)
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret"+backup.FileSuffix), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing" + backup.FileSuffix, "..%2Fsecret" + backup.FileSuffix, "%2Fetc%2Fpasswd"} {
		wantStatus(t, h.request(http.MethodGet, pathBackups+"/"+name, nil, s), http.StatusNotFound)
	}
	n := len(h.entries())
	wantStatus(t, h.request(http.MethodGet, pathBackups+"/x"+backup.FileSuffix, nil, nil), http.StatusUnauthorized)
	if len(h.entries()) != n {
		t.Error("a refused download was audited")
	}
}

func TestPostBackups(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	wantStatus(t, h.request(http.MethodPost, pathBackups, nil, s), http.StatusServiceUnavailable)
	f, _ := withRunner(t, h)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/a", nfsUpdate(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/b", nfsUpdate(), s), http.StatusAccepted)
	before := len(h.entries())

	wantStatus(t, h.request(http.MethodPost, pathBackups, nil, s), http.StatusAccepted)
	if want := []string{backup.RunManual, "1", "a", "b"}; len(f.started) != 1 || !slices.Equal(f.started[0], want) {
		t.Errorf("started %v, want %v", f.started, want)
	}
	if n := len(h.entries()); n != before {
		t.Error("a started backup was audited by the API; the runner audits it when it ends")
	}

	for err, code := range map[error]string{
		backup.ErrNoKit:             "no_recovery_kit",
		backup.ErrUpgradeInProgress: "upgrade_in_progress",
		fmt.Errorf("%w: %s", backup.ErrNoSpace, targetLeak): "no_space",
	} {
		f.startErr = err
		rec := h.request(http.MethodPost, pathBackups, nil, s)
		wantStatus(t, rec, http.StatusConflict)
		var e Error
		decode(t, rec, &e)
		if e.Code != code || strings.Contains(rec.Body.String(), targetLeak) {
			t.Errorf("refusal %s: %s", code, rec.Body)
		}
	}
	if n := len(h.entries()); n != before {
		t.Error("a refusal the runner audits was audited again")
	}

	f.startErr = backup.ErrBusy
	wantStatus(t, h.request(http.MethodPost, pathBackups, nil, s), http.StatusConflict)
	entries := h.entries()
	if e := entries[len(entries)-1]; len(entries) != before+1 || e.Action != string(AuditEntryActionBackup) || e.Result != "failure" || e.User != testUser || e.Details["failed"] != "refused: Busy" {
		t.Errorf("busy entry %+v", e)
	}

	f.startErr = errors.New(targetLeak)
	rec := h.request(http.MethodPost, pathBackups, nil, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String()+h.logs.String(), targetLeak) {
		t.Error("the runner's error text left the manager")
	}

	resetCluster(t)
	wantStatus(t, h.request(http.MethodPost, pathBackups, nil, s), http.StatusConflict)
}

func TestGetBackups(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	rec := h.request(http.MethodGet, pathBackups, nil, s)
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"local":[]`) || !strings.Contains(rec.Body.String(), `"targets":[]`) {
		t.Errorf("empty backups %s", rec.Body)
	}

	_, dir := withRunner(t, h)
	older, _ := writeBackup(t, dir, time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC), 10)
	newer, _ := writeBackup(t, dir, time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC), 20)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/late", nfsUpdate(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/fresh", nfsUpdate(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/new", nfsUpdate(), s), http.StatusAccepted)

	a := getAppliance(t)
	day := func(d int) *metav1.Time { tm := metav1.NewTime(h.now.AddDate(0, 0, -d)); return &tm }
	last := v1alpha1.BackupRecord{Name: newer, Time: *day(1), SizeBytes: 20, Database: v1alpha1.BackupDatabaseNotIncluded}
	a.Status.Backup = &v1alpha1.BackupStatus{Running: true, Last: &last, Targets: []v1alpha1.TargetStatus{
		{Name: "late", LastSuccess: day(3), LastAttempt: day(1), LastError: target.ReasonUnreachable},
		{Name: "fresh", LastSuccess: day(1), LastAttempt: day(1)},
		{Name: "removed", LastSuccess: day(9)},
	}}
	a.Status.LastRestoreTest = day(100)
	a.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionBackupHealthy, Status: metav1.ConditionFalse, Reason: "TargetFailed",
		Message: "late: unreachable", LastTransitionTime: metav1.Now()}}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	rec = h.request(http.MethodGet, pathBackups, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Backups
	decode(t, rec, &got)
	if !got.Running || got.Last == nil || got.Last.Database != NotIncluded || got.Healthy == nil || got.Healthy.Status != False || got.LastRestoreTest == nil {
		t.Errorf("backups %+v", got)
	}
	if len(got.Local) != 2 || got.Local[0].Name != newer || got.Local[0].Database != NotIncluded || got.Local[1].Name != older || got.Local[1].Database != Included {
		t.Errorf("local %+v", got.Local)
	}
	health := map[string]BackupTargetHealth{}
	for _, target := range got.Targets {
		health[target.Name] = target.Health
	}
	if len(health) != 3 || !health["late"].Warning || deref((*string)(health["late"].LastError)) != target.ReasonUnreachable ||
		health["fresh"].Warning || health["new"].Warning || health["new"].LastSuccess != nil {
		t.Errorf("health %+v", health)
	}

	var status Status
	statusRec := h.request(http.MethodGet, "/appliance/api/v1/status", nil, s)
	wantStatus(t, statusRec, http.StatusOK)
	decode(t, statusRec, &status)
	if b := status.Backup; b == nil || !b.Running || b.Last == nil || len(b.Targets) != 3 || !b.Targets[0].Warning || b.LastRestoreTest == nil {
		t.Errorf("status backup %+v", status.Backup)
	}

	// Deleting a target forgets its state.
	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/late", nil, s), http.StatusAccepted)
	for _, st := range getAppliance(t).Status.Backup.Targets {
		if st.Name == "late" {
			t.Error("the state of the removed target stays")
		}
	}
}

func TestBackupTargetTest(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	wantStatus(t, h.request(http.MethodPost, pathTargets+"/a/test", nil, s), http.StatusServiceUnavailable)
	f, _ := withRunner(t, h)
	wantStatus(t, h.request(http.MethodPost, pathTargets+"/a/test", nil, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/a", nfsUpdate(), s), http.StatusAccepted)

	var out BackupTargetTest
	rec := h.request(http.MethodPost, pathTargets+"/a/test", nil, s)
	wantStatus(t, rec, http.StatusOK)
	decode(t, rec, &out)
	if !out.Ok || out.Reason != nil || !slices.Equal(f.tested, []string{"a"}) {
		t.Errorf("test %+v, tested %v", out, f.tested)
	}
	if entries := h.entries(); entries[len(entries)-1].Result != "success" {
		t.Errorf("passed test audited as %+v", entries[len(entries)-1])
	}

	f.testErr = fmt.Errorf("%w: %s", target.ErrAuthFailed, targetLeak)
	rec = h.request(http.MethodPost, pathTargets+"/a/test", nil, s)
	wantStatus(t, rec, http.StatusOK)
	decode(t, rec, &out)
	if out.Ok || out.Reason == nil || *out.Reason != BackupTargetReasonAuthFailed || strings.Contains(rec.Body.String(), targetLeak) {
		t.Errorf("failed test %s", rec.Body)
	}
	entries := h.entries()
	if e := entries[len(entries)-1]; e.Action != string(AuditEntryActionBackupTargetTest) || e.Result != "failure" || e.Details["result"] != target.ReasonAuthFailed || e.Details["name"] != "a" {
		t.Errorf("audit entry %+v", e)
	}

	f.testErr = backup.ErrNotFound
	wantStatus(t, h.request(http.MethodPost, pathTargets+"/a/test", nil, s), http.StatusNotFound)
	resetCluster(t)
	wantStatus(t, h.request(http.MethodPost, pathTargets+"/a/test", nil, s), http.StatusConflict)
}

func TestBackupHostKey(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	body := HostKeyRequest{Host: "sftp.example.com"}
	wantStatus(t, h.request(http.MethodPost, pathHostKey, body, s), http.StatusServiceUnavailable)
	f, _ := withRunner(t, h)
	f.fingerprint = sftpHostKey

	rec := h.request(http.MethodPost, pathHostKey, body, s)
	wantStatus(t, rec, http.StatusOK)
	var key HostKey
	decode(t, rec, &key)
	if key.Fingerprint != sftpHostKey || !slices.Equal(f.scanned, []string{"sftp.example.com:22"}) {
		t.Errorf("host key %+v, scanned %v", key, f.scanned)
	}
	entries := h.entries()
	if e := entries[len(entries)-1]; e.Action != string(AuditEntryActionBackupHostKeyScan) || e.Result != "success" || e.User != testUser ||
		e.Details["host"] != "sftp.example.com" || e.Details["port"] != "22" {
		t.Errorf("audit entry %+v", e)
	}
	// Like every change, a scan needs the session and its CSRF token; without the token it is audited as denied.
	wantStatus(t, h.request(http.MethodPost, pathHostKey, body, nil), http.StatusUnauthorized)
	wantStatus(t, h.request(http.MethodPost, pathHostKey, body, &session{cookie: s.cookie, csrf: "wrong"}), http.StatusForbidden)
	if e := h.entries(); e[len(e)-1].Result != "denied" || len(f.scanned) != 1 {
		t.Errorf("scan without the CSRF token: %+v, scanned %v", e[len(e)-1], f.scanned)
	}

	wantTargetField(t, h.request(http.MethodPost, pathHostKey, HostKeyRequest{Host: "sftp.example.com:22"}, s), "host")
	wantTargetField(t, h.request(http.MethodPost, pathHostKey, HostKeyRequest{Host: "192.0.2.1", Port: ptr(70000)}, s), "port")

	for err, code := range map[error]string{
		fmt.Errorf("%w: %s", target.ErrUnreachable, targetLeak): target.ReasonUnreachable,
		errors.New(targetLeak): target.ReasonFailed,
	} {
		f.scanErr = err
		rec := h.request(http.MethodPost, pathHostKey, HostKeyRequest{Host: "192.0.2.1", Port: ptr(2222)}, s)
		wantStatus(t, rec, http.StatusBadGateway)
		var e Error
		decode(t, rec, &e)
		if e.Code != code || strings.Contains(rec.Body.String()+h.logs.String(), targetLeak) {
			t.Errorf("scan failure %s", rec.Body)
		}
		if entries := h.entries(); entries[len(entries)-1].Result != "failure" || entries[len(entries)-1].Details["port"] != "2222" {
			t.Errorf("failed scan audited as %+v", entries[len(entries)-1])
		}
	}
	resetCluster(t)
	wantStatus(t, h.request(http.MethodPost, pathHostKey, body, s), http.StatusConflict)
}

// backupTargetAnswers sets S3 and SFTP targets with their secrets, keeps them, refuses a change, tests a target
// whose driver repeats a secret in its error and lists the targets, adding the answers to answers. It returns the
// SFTP private key.
func backupTargetAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) string {
	t.Helper()
	f, _ := withRunner(t, h)
	key := sshKey(t)
	for _, req := range []struct {
		name string
		body BackupTargetUpdate
	}{
		{"s3", s3Update(ptr(s3AccessKey), ptr(s3SecretKey))},
		{"s3", s3Update(nil, nil)},
		{"s3", func() BackupTargetUpdate {
			u := s3Update(ptr(s3AccessKey), ptr(s3SecretKey))
			u.TimeZone = "Mars/Base"
			return u
		}()},
		{"sftp", sftpUpdate(ptr(sftpPassword), nil)},
		{"sftp", sftpUpdate(ptr(sftpPassword), ptr(key))},
		{"sftp", sftpUpdate(nil, ptr(key))},
	} {
		answers.Write(h.request(http.MethodPut, pathTargets+"/"+req.name, req.body, s).Body.Bytes())
	}
	f.testErr = fmt.Errorf("%w: 403 %s %s", target.ErrAuthFailed, s3AccessKey, s3SecretKey)
	answers.Write(h.request(http.MethodPost, pathTargets+"/s3/test", nil, s).Body.Bytes())
	for _, path := range []string{pathBackups, pathTargets + "/s3", pathTargets + "/sftp"} {
		rec := h.request(http.MethodGet, path, nil, s)
		wantStatus(t, rec, http.StatusOK)
		answers.Write(rec.Body.Bytes())
	}
	return key
}

// failingUpdateClient refuses every update of the Appliance.
type failingUpdateClient struct{ client.Client }

func (c failingUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*v1alpha1.Appliance); ok {
		return errors.New("etcd unavailable")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestFailedTargetUpdateRestoresTheSecret(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, s3Update(ptr(s3AccessKey), ptr(s3SecretKey)), s), http.StatusAccepted)
	h.srv.deps.Client = failingUpdateClient{testClient}

	// A new type for an existing target: its stored secrets come back.
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/"+backupTargetName, sftpUpdate(ptr(sftpPassword), nil), s), http.StatusInternalServerError)
	data := systemSecret(t, backupTargetSecretName(backupTargetName)).Data
	if string(data[target.AccessKeyKey]) != s3AccessKey || string(data[target.SecretKeyKey]) != s3SecretKey || len(data[target.PasswordKey]) != 0 {
		t.Errorf("secret after the failed update has keys %v", slices.Sorted(maps.Keys(data)))
	}
	if spec := targetSpec(t, backupTargetName); spec == nil || spec.Type != v1alpha1.BackupTypeS3 {
		t.Errorf("spec after the failed update %+v", spec)
	}

	// A new target: its Secret goes again.
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/sftp", sftpUpdate(ptr(sftpPassword), nil), s), http.StatusInternalServerError)
	if !secretGone(t, backupTargetSecretName("sftp")) {
		t.Error("the secrets of a target that was not saved stay")
	}
	if strings.Contains(h.logs.String(), sftpPassword) {
		t.Error("the log holds the password")
	}
}

func TestDeleteATargetWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	wantStatus(t, h.request(http.MethodPut, pathTargets+"/nas", nfsUpdate(), s), http.StatusAccepted)
	if !secretGone(t, backupTargetSecretName("nas")) {
		t.Fatal("an NFS target got a Secret")
	}
	wantStatus(t, h.request(http.MethodDelete, pathTargets+"/nas", nil, s), http.StatusAccepted)
}
