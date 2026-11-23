package release

import (
	"reflect"
	"testing"
)

func TestCacheListFiltersArchAndAddsBuiltImages(t *testing.T) {
	m := &Manifest{Images: []Image{
		{Ref: "z/last:1", Digest: "sha256:1"},
		{Ref: "a/amd-only:1", Digest: "sha256:2", Arches: []string{"amd64"}},
		{Ref: "a/first:1@sha256:3", Digest: "sha256:3"},
		{Ref: "z/last:1", Digest: "sha256:1"},
	}}

	got := CacheList(m, "arm64", "factory/installer:v1", "factory/manager:v1")
	want := []string{"a/first:1@sha256:3", "factory/installer:v1", "factory/manager:v1", "z/last:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arm64: got %v want %v", got, want)
	}

	got = CacheList(m, "amd64", "", "")
	want = []string{"a/amd-only:1", "a/first:1@sha256:3", "z/last:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("amd64: got %v want %v", got, want)
	}
}

func TestCacheListHasPublicConnectorsOnly(t *testing.T) {
	m := &Manifest{
		Images: []Image{
			{Ref: "registry.example/ilm/public:1", Digest: "sha256:1"},
			{Ref: "registry.example/ilm-private/private:1", Digest: "sha256:2"},
		},
		Connectors: []Connector{
			{Name: "public", Image: "registry.example/ilm/public:1", Digest: "sha256:1"},
			{Name: "private", Image: "registry.example/ilm-private/private:1", Digest: "sha256:2", Private: true},
		},
	}

	got := CacheList(m, "arm64", "")

	want := []string{"registry.example/ilm/public:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestConnectorRegistryHost(t *testing.T) {
	for image, want := range map[string]string{
		"hub.omnitrustregistry.com/ilm-private/x:1.0.0": "hub.omnitrustregistry.com",
		"Registry.Example.com:5000/x:1":                 "registry.example.com:5000",
		"localhost/x:1":                                 "localhost",
		"library/busybox:1":                             "docker.io",
		"busybox":                                       "docker.io",
	} {
		if got := (Connector{Image: image}).RegistryHost(); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestPinnedImage(t *testing.T) {
	m := &Manifest{Images: []Image{
		{Ref: "docker.io/library/busybox:latest", Digest: "sha256:1"},
		{Ref: "ghcr.io/example/tool:2", Digest: "sha256:2"},
	}}
	for name, want := range map[string]string{
		"busybox":                "docker.io/library/busybox:latest@sha256:1",
		"library/busybox:latest": "docker.io/library/busybox:latest@sha256:1",
		"ghcr.io/example/tool:2": "ghcr.io/example/tool:2@sha256:2",
	} {
		if got, err := m.PinnedImage(name); err != nil || got != want {
			t.Errorf("%s: got %q, %v, want %q", name, got, err, want)
		}
	}
	if _, err := m.PinnedImage("postgresql:18"); err == nil {
		t.Error("an image the release does not pin was found")
	}
}
