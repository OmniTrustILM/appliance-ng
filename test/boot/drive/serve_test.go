package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

const (
	testSFTPPassword = "sftp-password-6d1b7e"
	testS3Secret     = "s3-secret-key-c20f93"
)

func startTestTargets(t *testing.T) (*servedTargets, string) {
	t.Helper()
	dir := t.TempDir()
	served, err := startTargets(targetsConfig{
		sftpListen: "127.0.0.1:0", s3Listen: "127.0.0.1:0", guestHost: "127.0.0.1", dir: dir,
		sftpPassword: testSFTPPassword, s3Secret: testS3Secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(served.Close)
	return served, dir
}

func splitAddr(t *testing.T, addr string) (string, int32) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, int32(n)
}

func roundTrip(t *testing.T, tg target.Target, dir string) {
	t.Helper()
	ctx := context.Background()
	const name = "node-20260101T000000Z-v1.ilmbackup"
	data := bytes.Repeat([]byte("ilm"), 1000)
	if err := tg.Put(ctx, name, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := backupFiles(t, dir); len(got) != 1 || got[0] != name {
		t.Fatalf("files on the host = %v, want [%s]", got, name)
	}
	list, err := tg.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != name || list[0].Size != int64(len(data)) {
		t.Fatalf("List = %v, %v", list, err)
	}
	r, err := tg.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get returned %d bytes, %v; want the %d written", len(got), err, len(data))
	}
	if err := target.Test(ctx, tg); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if err := tg.Delete(ctx, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := backupFiles(t, dir); len(got) != 0 {
		t.Fatalf("files left after Delete: %v", got)
	}
}

func TestServeTargetsRoundTrip(t *testing.T) {
	served, dir := startTestTargets(t)

	t.Run("sftp", func(t *testing.T) {
		host, port := splitAddr(t, served.sftpAddr)
		spec := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, SFTP: &v1alpha1.SFTPTarget{
			Host: host, Port: port, Path: "/", User: sftpUser, HostKey: served.fingerprint}}
		tg, err := target.New(spec, map[string][]byte{target.PasswordKey: []byte(testSFTPPassword)}, target.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, tg, sftpDir(dir))
	})

	t.Run("sftp refuses another password and another host key", func(t *testing.T) {
		host, port := splitAddr(t, served.sftpAddr)
		spec := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, SFTP: &v1alpha1.SFTPTarget{
			Host: host, Port: port, Path: "/", User: sftpUser, HostKey: served.fingerprint}}
		tg, err := target.New(spec, map[string][]byte{target.PasswordKey: []byte("wrong")}, target.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Test(context.Background(), tg); err == nil {
			t.Error("Test passed with a wrong password")
		}
		spec.SFTP.HostKey = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
		tg, err = target.New(spec, map[string][]byte{target.PasswordKey: []byte(testSFTPPassword)}, target.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Test(context.Background(), tg); err == nil {
			t.Error("Test passed with another host key")
		}
	})

	t.Run("s3", func(t *testing.T) {
		spec := v1alpha1.BackupTarget{Name: "s3", Type: v1alpha1.BackupTypeS3, S3: &v1alpha1.S3Target{
			Endpoint: "https://" + served.s3Addr, Bucket: s3Bucket, Prefix: s3Prefix, Region: "us-east-1"}}
		creds := map[string][]byte{target.AccessKeyKey: []byte(s3AccessKey), target.SecretKeyKey: []byte(testS3Secret),
			target.CAKey: readCA(t, dir)}
		tg, err := target.New(spec, creds, target.Deps{})
		if err != nil {
			t.Fatal(err)
		}
		roundTrip(t, tg, s3Files(dir))
	})

	t.Run("s3 refuses another access key", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "https://"+served.s3Addr+"/"+s3Bucket, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=other/20260101/us-east-1/s3/aws4_request")
		res, err := caClient(t, dir).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", res.StatusCode)
		}
	})

	t.Run("s3 serves no plain HTTP and no certificate of another host", func(t *testing.T) {
		res, err := http.Get("http://" + served.s3Addr + "/" + s3Bucket)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Errorf("plain HTTP status = %d, want 400", res.StatusCode)
			}
		}
		client := caClient(t, dir)
		client.Transport.(*http.Transport).TLSClientConfig.ServerName = "other.test"
		if res, err := client.Get("https://" + served.s3Addr + "/" + s3Bucket); err == nil {
			_ = res.Body.Close()
			t.Error("the S3 certificate is valid for another host")
		}
	})
}

func readCA(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(s3CAFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// caClient trusts only the CA serve-targets wrote to dir.
func caClient(t *testing.T, dir string) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(readCA(t, dir)) {
		t.Fatal("the CA file holds no PEM certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
}

func TestCredentialIsReadOrGenerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	generated, err := credential(path)
	if err != nil || generated == "" {
		t.Fatalf("credential = %q, %v", generated, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("file = %v, %v, want mode 0600", info, err)
	}
	again, err := credential(path)
	if err != nil || again != generated {
		t.Errorf("second read = %q, %v, want the generated one", again, err)
	}
	if err := os.WriteFile(path, []byte("given\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := credential(path); err != nil || got != "given" {
		t.Errorf("credential = %q, %v, want given", got, err)
	}
}

func TestServeTargetsPrintsNoSecret(t *testing.T) {
	served, _ := startTestTargets(t)
	out := captureStdout(t, func() { announce(served) })
	for _, s := range []string{testSFTPPassword, testS3Secret} {
		if strings.Contains(out, s) {
			t.Errorf("serve-targets printed a secret:\n%s", out)
		}
	}
	for _, want := range []string{served.sftpAddr, served.s3Addr, served.fingerprint} {
		if !strings.Contains(out, want) {
			t.Errorf("serve-targets printed no %q:\n%s", want, out)
		}
	}
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := backupNames(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func TestSFTPCannotLeaveItsRoot(t *testing.T) {
	served, dir := startTestTargets(t)
	outside := filepath.Join(dir, "outside.ilmbackup")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, served.sftpAddr)
	for name, path := range map[string]string{"parent": "/..", "relative parent": "../", "absolute": dir, "nested parent": "/sub/../.."} {
		t.Run(name, func(t *testing.T) {
			spec := v1alpha1.BackupTarget{Name: "sftp", Type: v1alpha1.BackupTypeSFTP, SFTP: &v1alpha1.SFTPTarget{
				Host: host, Port: port, Path: path, User: sftpUser, HostKey: served.fingerprint}}
			tg, err := target.New(spec, map[string][]byte{target.PasswordKey: []byte(testSFTPPassword)}, target.Deps{})
			if err != nil {
				t.Fatal(err)
			}
			if list, err := tg.List(context.Background()); err == nil && len(list) != 0 {
				t.Errorf("List through %q = %v, want nothing outside the root", path, list)
			}
			if r, err := tg.Get(context.Background(), "outside.ilmbackup"); err == nil {
				_ = r.Close()
				t.Error("Get reached a file outside the root")
			}
		})
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Errorf("the file outside = %q, %v", got, err)
	}
}
