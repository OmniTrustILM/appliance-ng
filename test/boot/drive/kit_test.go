package main

import (
	"context"
	"encoding/json"
	"errors"
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
	testPassphrase  = "correct horse battery staple"
	testKubeSecret  = "kube-token-4f1a9c"
	testTalosSecret = "talos-key-77be02"
)

// captureStdout returns what f prints to standard output.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()
	done := make(chan string)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	f()
	_ = w.Close()
	return <-done
}

type fakeCluster struct{ kubeconfigs, talosconfigs, logSecrets []string }

func (f *fakeCluster) listNodes(_ context.Context, kubeconfig []byte) error {
	f.kubeconfigs = append(f.kubeconfigs, string(kubeconfig))
	if len(f.kubeconfigs) > 1 {
		return errUnauthorized // a later use is one after the revocation
	}
	return nil
}

func (f *fakeCluster) talosReady(_ context.Context, talosconfig []byte) error {
	f.talosconfigs = append(f.talosconfigs, string(talosconfig))
	return nil
}

func (f *fakeCluster) checkManager(_ context.Context, _ []byte, secrets ...string) error {
	f.logSecrets = append(f.logSecrets, secrets...)
	return nil
}

// kitConsole serves the recovery kit API: the first POST issues kit, a refusal body is echoed by refuse.
func kitConsole(t *testing.T, kit []byte, recipient, refuse string) *httptest.Server {
	t.Helper()
	issued, confirmed := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /appliance/api/v1/recovery-kit":
			_ = json.NewEncoder(w).Encode(map[string]any{"issued": issued, "confirmed": confirmed, "recipient": recipient})
		case "POST /appliance/api/v1/recovery-kit":
			if refuse != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "invalid", "message": refuse})
				return
			}
			issued = true
			_, _ = w.Write(kit)
		case "POST /appliance/api/v1/recovery-kit/confirm":
			confirmed = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDriveKitNeverPrintsSecrets(t *testing.T) {
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
	passphraseFile := filepath.Join(dir, "kit.pass")
	if err := os.WriteFile(passphraseFile, []byte(testPassphrase+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{out: filepath.Join(dir, "kit.tar.age"), passphraseFile: passphraseFile}
	secrets := []string{testPassphrase, testKubeSecret, testTalosSecret, key.String(), "AGE-SECRET-KEY-"}

	t.Run("issues, confirms and checks the kit", func(t *testing.T) {
		srv := kitConsole(t, kit, key.Recipient().String(), "")
		cl := &fakeCluster{}
		var runErr error
		out := captureStdout(t, func() {
			runErr = newClient(srv.URL, nil, "").issueKit(context.Background(), o, cl)
		})
		if runErr != nil {
			t.Fatalf("issueKit = %v", runErr)
		}
		for _, secret := range secrets {
			if strings.Contains(out, secret) {
				t.Errorf("drive printed a secret:\n%s", out)
			}
		}
		if info, err := os.Stat(o.out); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("kit file = %v, %v, want a file with mode 0600", info, err)
		}
		if len(cl.kubeconfigs) != 1 || !strings.Contains(cl.kubeconfigs[0], testKubeSecret) {
			t.Errorf("checked kubeconfigs = %q, want the kit's", cl.kubeconfigs)
		}
		if len(cl.talosconfigs) != 1 || !strings.Contains(cl.talosconfigs[0], testTalosSecret) {
			t.Errorf("checked talosconfigs = %q, want the kit's", cl.talosconfigs)
		}
		if !strings.Contains(strings.Join(cl.logSecrets, " "), testPassphrase) {
			t.Error("the logs were not searched for the passphrase")
		}
	})

	t.Run("an issue the API refuses leaves no secret in the error", func(t *testing.T) {
		srv := kitConsole(t, kit, "", "bad passphrase "+testPassphrase)
		err := newClient(srv.URL, nil, "").issueKit(context.Background(), options{
			out: filepath.Join(dir, "refused.tar.age"), passphraseFile: passphraseFile,
		}, &fakeCluster{})
		if err == nil {
			t.Fatal("issueKit passed although the API refused")
		}
		if strings.Contains(err.Error(), testPassphrase) {
			t.Errorf("the error shows the passphrase: %v", err)
		}
	})

	t.Run("a kit of another backup key is refused", func(t *testing.T) {
		other, err := recovery.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		srv := kitConsole(t, kit, other.Recipient().String(), "")
		err = newClient(srv.URL, nil, "").issueKit(context.Background(), options{
			out: filepath.Join(dir, "other.tar.age"), passphraseFile: passphraseFile,
		}, &fakeCluster{})
		if err == nil || strings.Contains(err.Error(), testPassphrase) {
			t.Errorf("issueKit with another recipient = %v, want an error without the passphrase", err)
		}
	})
}

func TestDriveAccessNeverPrintsSecrets(t *testing.T) {
	const kubeconfig, talosconfig = "token: " + testKubeSecret, "key: " + testTalosSecret
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /appliance/api/v1/access/kubeconfig":
			_, _ = w.Write([]byte(kubeconfig))
		case "POST /appliance/api/v1/access/talosconfig":
			_, _ = w.Write([]byte(talosconfig))
		case "GET /appliance/api/v1/access":
			_, _ = w.Write([]byte(`{"items":[{"kind":"kubeconfig","name":"kc-abc","role":"view"},` +
				`{"kind":"kubeconfig","name":"break-glass","role":"cluster-admin","breakGlass":true},` +
				`{"kind":"talosconfig","role":"os:reader"}]}`))
		case "DELETE /appliance/api/v1/access/kubeconfig/kc-abc":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cl := &fakeCluster{}
	var err error
	out := captureStdout(t, func() { err = newClient(srv.URL, nil, "").checkAccess(context.Background(), cl) })
	if err != nil {
		t.Fatalf("checkAccess = %v", err)
	}
	if !deleted {
		t.Error("the kubeconfig was not revoked")
	}
	for _, secret := range []string{testKubeSecret, testTalosSecret} {
		if strings.Contains(out, secret) {
			t.Errorf("drive printed a secret:\n%s", out)
		}
	}
	if len(cl.kubeconfigs) != 2 || len(cl.talosconfigs) != 1 {
		t.Errorf("checked %d kubeconfigs and %d talosconfigs, want 2 (before and after the revocation) and 1",
			len(cl.kubeconfigs), len(cl.talosconfigs))
	}
}

func TestDriveAccessFailsWhenARevokedKubeconfigStillWorks(t *testing.T) {
	err := waitRevoked(context.Background(), time.Millisecond, 3*time.Millisecond, func() error { return nil })
	if err == nil {
		t.Error("waitRevoked passed although the kubeconfig still works")
	}
	if err := waitRevoked(context.Background(), time.Millisecond, time.Second, func() error { return errUnauthorized }); err != nil {
		t.Errorf("waitRevoked = %v after the kubeconfig stopped working", err)
	}
	err = waitRevoked(context.Background(), time.Millisecond, 3*time.Millisecond, func() error { return errors.New("connection refused") })
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("waitRevoked = %v, want the last error", err)
	}
}
