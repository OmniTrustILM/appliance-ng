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
		{Ref: "registry.example.com/pinned:3@sha256:3", Digest: "sha256:3"},
		{Ref: "registry.example.com/untagged@sha256:4", Digest: "sha256:4"},
	}}
	for _, tc := range []struct{ name, want string }{
		{"busybox", "docker.io/library/busybox:latest@sha256:1"},
		{"library/busybox:latest", "docker.io/library/busybox:latest@sha256:1"},
		{"busybox@sha256:1", "docker.io/library/busybox:latest@sha256:1"},
		{"ghcr.io/example/tool:2", "ghcr.io/example/tool:2@sha256:2"},
		{"registry.example.com/pinned:3", "registry.example.com/pinned:3@sha256:3"},
		{"registry.example.com/pinned:3@sha256:3", "registry.example.com/pinned:3@sha256:3"},
		{"registry.example.com/untagged", "registry.example.com/untagged:latest@sha256:4"},
	} {
		if got, err := m.PinnedImage(tc.name); err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}
	for _, name := range []string{"postgresql:18", "busybox@sha256:9", "registry.example.com/pinned:3@sha256:9"} {
		if got, err := m.PinnedImage(name); err == nil {
			t.Errorf("%s: got %q, want an error", name, got)
		}
	}
}

func TestMirrorUpstreamsFromRelease(t *testing.T) {
	m := &Manifest{Images: []Image{
		{Ref: "ghcr.io/x/a:1", Digest: "sha256:1"},
		{Ref: "Quay.io/x/b:1@sha256:2", Digest: "sha256:2"},
		{Ref: "registry.example.com:5000/c:1", Digest: "sha256:3"},
		{Ref: "rabbitmq:4", Digest: "sha256:4"},
		{Ref: "rancher/local-path-provisioner:v1", Digest: "sha256:5"},
		{Ref: "index.docker.io/library/busybox:1", Digest: "sha256:6"},
		{Ref: "ghcr.io/x/d:1", Digest: "sha256:7"},
	}}

	want := []string{"docker.io", "ghcr.io", "quay.io", "registry.example.com:5000"}
	if got := m.RegistryHosts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RegistryHosts() = %v, want %v", got, want)
	}
}

func TestArchPins(t *testing.T) {
	const (
		index = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		amd   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		arm   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		one   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	m := &Manifest{Images: []Image{
		{Ref: "quay.io/x/agent:1@" + index, Digest: index, ArchDigests: map[string]string{"amd64": amd, "arm64": arm}},
		{Ref: "quay.io/x/single:1@" + one, Digest: one, ArchDigests: map[string]string{"amd64": one}, Arches: []string{"amd64"}},
		{Ref: "quay.io/x/tagged:1", Digest: "sha256:2"},
	}}

	if got, want := m.ArchPins("arm64"), map[string]string{index: arm}; !reflect.DeepEqual(got, want) {
		t.Errorf("ArchPins(arm64) = %v, want %v", got, want)
	}
	if got, want := m.ArchPins("amd64"), map[string]string{index: amd}; !reflect.DeepEqual(got, want) {
		t.Errorf("ArchPins(amd64) = %v, want %v", got, want)
	}
}

func TestRepository(t *testing.T) {
	for name, want := range map[string]string{
		"busybox":                          "docker.io/library/busybox",
		"rancher/local-path-provisioner":   "docker.io/rancher/local-path-provisioner",
		"docker.io/traefik":                "docker.io/library/traefik",
		"index.docker.io/library/rabbitmq": "docker.io/library/rabbitmq",
		"index.docker.io/versity/gw":       "docker.io/versity/gw",
		"localhost/app":                    "localhost/app",
		"registry.example.com:5000/a/b":    "registry.example.com:5000/a/b",
		"quay.io/cilium/cilium":            "quay.io/cilium/cilium",
	} {
		if got := Repository(name); got != want {
			t.Errorf("Repository(%q) = %q, want %q", name, got, want)
		}
	}
}
