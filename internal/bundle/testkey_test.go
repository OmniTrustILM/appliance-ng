package bundle

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	appliance "github.com/OmniTrustILM/appliance-ng"
)

func TestSigningKeyIsTheTestKeyWhenSet(t *testing.T) {
	key, keyPEM := testKeyPEM(t)
	got, err := signingKey(base64.StdEncoding.EncodeToString(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(got) {
		t.Error("signingKey(test key) is not the test key")
	}
	embedded, err := ParsePublicKey(appliance.CosignPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := signingKey(""); err != nil || !embedded.(*ecdsa.PublicKey).Equal(got) {
		t.Errorf("signingKey(\"\") = %v, want cosign.pub", err)
	}
}

// testKeyPEM is a new ECDSA key and the PEM of its public key.
func testKeyPEM(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestSigningKeyRejectsABadTestKey refuses a test key that is not exactly one PEM public key, so that the base64 of
// every test key starts as the release check looks for.
func TestSigningKeyRejectsABadTestKey(t *testing.T) {
	_, keyPEM := testKeyPEM(t)
	for name, test := range map[string][]byte{
		"not a key":     []byte("key"),
		"preamble":      append([]byte("\n"), keyPEM...),
		"trailing data": append(slices.Clone(keyPEM), "x"...),
		"second block":  append(slices.Clone(keyPEM), keyPEM...),
	} {
		if _, err := signingKey(base64.StdEncoding.EncodeToString(test)); err == nil || !strings.Contains(err.Error(), "bundle test key") {
			t.Errorf("%s: signingKey = %v, want an error naming the test key", name, err)
		}
	}
	if _, err := signingKey("%%%"); err == nil || !strings.Contains(err.Error(), "bundle test key") {
		t.Errorf("not base64: signingKey = %v, want an error naming the test key", err)
	}
	if _, err := signingKey(base64.StdEncoding.EncodeToString(append(slices.Clone(keyPEM), " \n"...))); err != nil {
		t.Errorf("trailing white space: signingKey = %v", err)
	}
}

// workflow is the part of a GitHub workflow the release check reads.
type workflow struct {
	Jobs map[string]struct {
		If    string         `json:"if"`
		Uses  string         `json:"uses"`
		With  map[string]any `json:"with"`
		Steps []struct {
			Name string            `json:"name"`
			If   string            `json:"if"`
			Run  string            `json:"run"`
			Env  map[string]string `json:"env"`
		} `json:"steps"`
	} `json:"jobs"`
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	var w workflow
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/"+name)), &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return w
}

// TestReleaseBuildHasNoTestKey checks that only the long tests build a manager with a bundle test key: the release
// workflow never names one, the images workflow sets one only when asked to, only long-tests.yaml asks, and only
// for the build it runs itself (a release calls it with its own images), and only build/manager.sh links one in.
func TestReleaseBuildHasNoTestKey(t *testing.T) {
	const (
		input = "bundle-test-key"
		env   = "BUNDLE_TEST_KEY"
	)
	release := readRepoFile(t, ".github/workflows/release.yaml")
	for _, word := range []string{input, env, "testKey"} {
		if strings.Contains(release, word) {
			t.Errorf("release.yaml names %s", word)
		}
	}
	entries, err := os.ReadDir("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		for name, job := range readWorkflow(t, e.Name()).Jobs {
			if _, ok := job.With[input]; !ok {
				continue
			}
			if e.Name() != "long-tests.yaml" || !strings.Contains(job.If, "!inputs.caller-images") {
				t.Errorf("%s: job %s asks for a bundle test key outside the long tests' own build", e.Name(), name)
			}
		}
	}
	checked := false
	for name, job := range readWorkflow(t, "images.yaml").Jobs {
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "LS0tLS1CRUdJTiBQVUJMSUMgS0VZ") {
				// grep finds nothing in a file that is missing, which would let the check pass.
				exists := strings.Index(s.Run, `test -s "out/$ARCH/manager/manager"`)
				checked = exists >= 0 && exists < strings.Index(s.Run, "grep")
			}
			mentions := strings.Contains(s.Run, env)
			for k, v := range s.Env {
				mentions = mentions || strings.Contains(k+v, env)
			}
			if mentions && !strings.Contains(s.If, "inputs."+input) {
				t.Errorf("images.yaml: step %q of job %s sets %s without inputs.%s", s.Name, name, env, input)
			}
		}
	}
	if !checked {
		t.Error("images.yaml does not make sure the manager binary is there before it looks for a bundle test key in it")
	}
	scripts, err := filepath.Glob("../../build/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range append(scripts, "../../Makefile", "../../Dockerfile.manager") {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "bundle.testKey") && filepath.Base(path) != "manager.sh" {
			t.Errorf("%s links in a bundle test key", path)
		}
	}
	if manager := readRepoFile(t, "build/manager.sh"); !strings.Contains(manager, `if [[ -n "${`+env+`:-}" ]]`) {
		t.Errorf("build/manager.sh does not link the test key in only when %s is set", env)
	}
}
