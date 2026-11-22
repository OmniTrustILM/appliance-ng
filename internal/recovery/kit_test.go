package recovery

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"filippo.io/age"
)

const testPassphrase = "a recovery passphrase"

func testKit(t *testing.T) Kit {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return Kit{
		BackupKey:   key,
		Kubeconfig:  []byte("kind: Config\n"),
		Talosconfig: []byte("context: appliance\n"),
		Identity:    Identity{HostName: "appliance.example.com", Address: "192.0.2.10", ApplianceVersion: "v0.3.0", ILMHostName: "ilm.example.com"},
	}
}

func TestKitRoundTrip(t *testing.T) {
	k := testKit(t)
	data, err := Build(k, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("AGE-SECRET-KEY-")) || bytes.Contains(data, k.Kubeconfig) {
		t.Fatal("the kit is not encrypted")
	}
	got, err := Open(data, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if got.BackupKey.String() != k.BackupKey.String() || !bytes.Equal(got.Kubeconfig, k.Kubeconfig) ||
		!bytes.Equal(got.Talosconfig, k.Talosconfig) || got.Identity != k.Identity {
		t.Errorf("opened %+v, want %+v", got, k)
	}

	// The age CLI decrypts it with the passphrase alone: an age file with a scrypt stanza holding a tar.
	id, err := age.NewScryptIdentity(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	tr := tar.NewReader(plain)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Mode&0o077 != 0 {
			t.Errorf("%s is readable by others: %o", h.Name, h.Mode)
		}
		body, _ := io.ReadAll(tr)
		names[h.Name] = string(body)
	}
	for _, name := range []string{"backup.key", "kubeconfig", "talosconfig", "appliance.json", "README.txt"} {
		if _, ok := names[name]; !ok {
			t.Errorf("the kit has no %s", name)
		}
	}
	if !strings.Contains(names["README.txt"], "age -d") {
		t.Error("the README does not say how to decrypt the kit")
	}
	if !strings.Contains(names["backup.key"], k.BackupKey.String()) {
		t.Error("backup.key does not hold the key")
	}
}

func TestKitWrongPassphrase(t *testing.T) {
	data, err := Build(testKit(t), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(data, "another passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("Open with a wrong passphrase: %v", err)
	}
}

func TestOpenRejectsWhatIsNotAKit(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":   nil,
		"garbage": []byte("not an age file"),
		"no tar":  encrypt(t, []byte("plain text")),
		"no key":  encrypt(t, tarOf(t, map[string]string{"kubeconfig": "x", "talosconfig": "y", "appliance.json": "{}"})),
		"bad key": encrypt(t, tarOf(t, map[string]string{"backup.key": "AGE-SECRET-KEY-1BAD", "kubeconfig": "x", "talosconfig": "y", "appliance.json": "{}"})),
		"bad identity": encrypt(t, tarOf(t, map[string]string{
			"backup.key": testKitKey(t), "kubeconfig": "x", "talosconfig": "y", "appliance.json": "[",
		})),
	} {
		if _, err := Open(data, testPassphrase); !errors.Is(err, ErrInvalidKit) {
			t.Errorf("%s: %v, want ErrInvalidKit", name, err)
		}
	}
}

func TestOpenRejectsAFileForAKey(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(tarOf(t, map[string]string{"backup.key": key.String()})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(buf.Bytes(), testPassphrase); !errors.Is(err, ErrInvalidKit) {
		t.Errorf("Open of a file for a key: %v, want ErrInvalidKit", err)
	}
}

func TestBuildRejectsAKitWithoutKey(t *testing.T) {
	k := testKit(t)
	k.BackupKey = nil
	if _, err := Build(k, testPassphrase); err == nil {
		t.Error("built a kit without a key")
	}
}

func TestErrorsQuoteNoSecret(t *testing.T) {
	key := testKitKey(t)
	data := encrypt(t, tarOf(t, map[string]string{"backup.key": key + "x", "kubeconfig": "x", "talosconfig": "y", "appliance.json": "{}"}))
	_, err := Open(data, testPassphrase)
	if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), testPassphrase) {
		t.Errorf("error %v", err)
	}
}

func testKitKey(t *testing.T) string {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key.String()
}

func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encrypt(t *testing.T, plain []byte) []byte {
	t.Helper()
	r, err := age.NewScryptRecipient(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	r.SetWorkFactor(10)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
