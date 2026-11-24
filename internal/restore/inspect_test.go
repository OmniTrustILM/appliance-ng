package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// usersSecretYAML returns the Secret appliance-users holding the named accounts, as a backup stores it.
func usersSecretYAML(t *testing.T, names ...string) string {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	store := auth.NewSecretUserStore(c, "appliance-system")
	for _, n := range names {
		if err := store.Create(context.Background(), auth.User{Name: n, PasswordHash: "hash", TOTPSeed: "SEED", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "appliance-system", Name: auth.UsersSecret}, secret); err != nil {
		t.Fatal(err)
	}
	secret.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
	out, err := yaml.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

type fixture struct {
	dir      string
	key      *age.X25519Identity
	file     string
	manifest *release.Manifest
}

func writeBackup(t *testing.T, key *age.X25519Identity, ilm string, accounts ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := backup.NewWriter(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{
		"appliance.yaml": "kind: Appliance\n",
		"database.sql":   "select 1;\n",
	}
	if len(accounts) > 0 {
		members["secrets/appliance-system/appliance-users.yaml"] = usersSecretYAML(t, accounts...)
	}
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := w.Add(n, int64(len(members[n])), strings.NewReader(members[n])); err != nil {
			t.Fatal(err)
		}
	}
	m := backup.Manifest{
		Appliance: "1.2.3", ILM: ilm, Operator: "1.1.0", Components: []string{"ilm", "keycloak"}, Database: "managed",
		DatabaseIncluded: true, HostName: "node", ILMHostName: "ilm.example.com", Time: time.Date(2026, 11, 23, 1, 2, 3, 0, time.UTC),
	}
	if err := w.Close(m); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFixture(t *testing.T, data func(key *age.X25519Identity) []byte) fixture {
	t.Helper()
	key, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "node.ilmbackup")
	if err := os.WriteFile(file, data(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture{dir: dir, key: key, file: file, manifest: &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.17"}}}
}

// rawArchive returns an encrypted tar of one member, named name with content body.
func rawArchive(t *testing.T, key *age.X25519Identity, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := age.Encrypt(&buf, key.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(enc)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInspectRejects(t *testing.T) {
	foreign, err := recovery.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		backup func(t *testing.T, key *age.X25519Identity) []byte
		want   error
	}{
		{"foreign kit", func(t *testing.T, _ *age.X25519Identity) []byte { return writeBackup(t, foreign, "2.17") }, ErrForeignKit},
		{"tampered", func(t *testing.T, k *age.X25519Identity) []byte {
			b := writeBackup(t, k, "2.17")
			b[len(b)/2] ^= 0xff
			return b
		}, ErrTampered},
		{"truncated", func(t *testing.T, k *age.X25519Identity) []byte { return rawArchive(t, k, "database.sql", "select 1;") }, ErrTruncated},
		{"format too new", func(t *testing.T, k *age.X25519Identity) []byte {
			return rawArchive(t, k, "manifest.json", `{"format":2}`)
		}, ErrFormatTooNew},
		{"other ILM version", func(t *testing.T, k *age.X25519Identity) []byte { return writeBackup(t, k, "2.16") }, ErrVersionUnavailable{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, func(k *age.X25519Identity) []byte { return tt.backup(t, k) })
			before, _ := os.ReadFile(f.file)
			_, err := Inspect(context.Background(), f.file, f.key, f.manifest, nil)
			var want ErrVersionUnavailable
			if errors.As(tt.want, &want) {
				var got ErrVersionUnavailable
				if !errors.As(err, &got) || got.Have != "2.17" || got.Want != "2.16" {
					t.Fatalf("err = %v, want ErrVersionUnavailable{Have: 2.17, Want: 2.16}", err)
				}
				if want := "the backup holds ILM 2.16, but this appliance has ILM 2.17"; err.Error() != want {
					t.Errorf("message %q, want %q", err, want)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), f.key.String()) {
				t.Errorf("error %q contains the backup key", err)
			}
			if after, _ := os.ReadFile(f.file); !bytes.Equal(before, after) {
				t.Error("the backup file changed")
			}
		})
	}
}

func TestInspectSummary(t *testing.T) {
	f := newFixture(t, func(k *age.X25519Identity) []byte { return writeBackup(t, k, "2.17", "zoe", "admin", "bob") })
	before, _ := os.ReadFile(f.file)
	s, err := Inspect(context.Background(), f.file, f.key, f.manifest, []string{"admin", "carol"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admin", "bob", "zoe"}; !slices.Equal(s.Accounts, want) {
		t.Errorf("Accounts = %v, want %v", s.Accounts, want)
	}
	if want := []string{"admin"}; !slices.Equal(s.Clashes, want) {
		t.Errorf("Clashes = %v, want %v", s.Clashes, want)
	}
	if want := []string{"ilm", "keycloak"}; !slices.Equal(s.Components, want) {
		t.Errorf("Components = %v, want %v", s.Components, want)
	}
	if s.Manifest.HostName != "node" || s.Manifest.ILM != "2.17" {
		t.Errorf("Manifest = %+v", s.Manifest)
	}
	if after, _ := os.ReadFile(f.file); !bytes.Equal(before, after) {
		t.Error("the backup file changed")
	}
}

func TestInspectWithoutAccounts(t *testing.T) {
	f := newFixture(t, func(k *age.X25519Identity) []byte { return writeBackup(t, k, "2.17") })
	s, err := Inspect(context.Background(), f.file, f.key, f.manifest, []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts) != 0 || len(s.Clashes) != 0 {
		t.Errorf("Accounts %v, Clashes %v, want none", s.Accounts, s.Clashes)
	}
}

func TestInspectMissingFile(t *testing.T) {
	f := newFixture(t, func(k *age.X25519Identity) []byte { return writeBackup(t, k, "2.17") })
	if _, err := Inspect(context.Background(), filepath.Join(f.dir, "none"), f.key, f.manifest, nil); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestInspectCanceled(t *testing.T) {
	f := newFixture(t, func(k *age.X25519Identity) []byte { return writeBackup(t, k, "2.17") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, f.file, f.key, f.manifest, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
