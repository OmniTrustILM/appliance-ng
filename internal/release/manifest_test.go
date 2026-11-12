package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRejectsMissingDigest(t *testing.T) {
	path := writeManifest(t, "version: 0.1.0\nimages:\n  - { ref: \"registry.k8s.io/etcd:v3.7.3\" }\n")

	_, err := Load(path)

	if err == nil || !strings.Contains(err.Error(), "registry.k8s.io/etcd:v3.7.3") {
		t.Fatalf("want error naming the ref, got %v", err)
	}
}

func TestLoadRejectsDigestMismatch(t *testing.T) {
	path := writeManifest(t, "images:\n  - { ref: \"a/x:1@sha256:aaa\", digest: \"sha256:bbb\" }\n")

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "a/x:1@sha256:aaa") {
		t.Fatalf("want digest mismatch error, got %v", err)
	}
}

func TestLoadParsesManifest(t *testing.T) {
	path := writeManifest(t, `version: 0.1.0
talos: { version: v1.14.5, kubernetes: v1.37.1, imager: "i@sha256:abc" }
platform: { ilmVersion: 2.20.0, hostName: ilm.appliance.internal }
implicitImages: [busybox]
images:
  - { ref: "a/x:1@sha256:aaa", digest: "sha256:aaa", arches: [amd64] }
`)

	m, err := Load(path)

	if err != nil {
		t.Fatal(err)
	}
	if m.Talos.Kubernetes != "v1.37.1" || m.Platform.HostName != "ilm.appliance.internal" ||
		m.ImplicitImages[0] != "busybox" || m.Images[0].Arches[0] != "amd64" {
		t.Fatalf("unexpected manifest %+v", m)
	}
}

func TestLoadRejectsMissingFileAndUnknownField(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Fatal("want error for missing file")
	}
	if _, err := Load(writeManifest(t, "bogus: 1\n")); err == nil {
		t.Fatal("want error for unknown field")
	}
}
