package appliance

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/cbom"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func TestReleaseManifestParses(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version == "" || m.ManagerImage == "" {
		t.Errorf("version %q, manager image %q: want both set", m.Version, m.ManagerImage)
	}
}

func TestCacheListHasCBOMImages(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		cached := release.CacheList(m, arch)
		for _, name := range []string{cbom.StoreImage, cbom.RepositoryImage} {
			if !slices.Contains(cached, name) {
				t.Errorf("%s: the image cache lacks %s", arch, name)
			}
			if _, err := m.PinnedImage(name); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	chart, err := m.Chart(cbom.ChartName)
	if err != nil {
		t.Fatal(err)
	}
	if chart.File != "charts/cbom-repository-1.2.0.tgz" || chart.SHA256 == "" {
		t.Errorf("chart %s = %+v, want the vendored 1.2.0 archive with its SHA-256", cbom.ChartName, chart)
	}
}

func TestReleaseManifestIsValid(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Compatibility.Operator != ">=1.1.0 <2.0.0" || m.Compatibility.MinAppliance != "0.1.0" {
		t.Errorf("compatibility = %+v", m.Compatibility)
	}
	for _, err := range release.Validate(m, os.DirFS("addons")) {
		t.Error(err)
	}
}

func TestBundlesCarry3aImages(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	refs := func(kind string) []string {
		var refs []string
		for _, img := range m.BundleImages(kind, "amd64") {
			refs = append(refs, img.Ref)
		}
		return refs
	}
	if !slices.Contains(refs(release.BundleSystem), cbom.StoreImage) {
		t.Errorf("the system bundle lacks %s", cbom.StoreImage)
	}
	if !slices.Contains(refs(release.BundlePlatform), cbom.RepositoryImage) {
		t.Errorf("the platform bundle lacks %s", cbom.RepositoryImage)
	}
}

func TestCiliumPinnedToTheManifestOfTheArch(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		data, err := fs.ReadFile(addons.Pinned(os.DirFS("addons"), m.ArchPins(arch)), "cilium.yaml")
		if err != nil {
			t.Fatal(err)
		}
		images, err := release.ImagesIn(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range m.Images {
			if strings.HasPrefix(img.Ref, "quay.io/cilium/") {
				wantArchManifest(t, arch, img, images)
			}
		}
	}
}

// wantArchManifest checks that images reference img by the digest of its manifest for arch, never by its index.
func wantArchManifest(t *testing.T, arch string, img release.Image, images []string) {
	t.Helper()
	name, _, _ := strings.Cut(img.Ref, "@")
	if want := name + "@" + img.ArchDigests[arch]; !slices.Contains(images, want) {
		t.Errorf("%s: cilium.yaml lacks %s", arch, want)
	}
	if slices.ContainsFunc(images, func(ref string) bool { return strings.HasSuffix(ref, img.Digest) }) {
		t.Errorf("%s: cilium.yaml still references the index %s", arch, img.Digest)
	}
}

func TestMirrorUpstreamsOfTheRelease(t *testing.T) {
	m, err := release.Parse(ReleaseManifest, "release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker.io", "ghcr.io", "hub.omnitrustregistry.com", "quay.io", "registry.k8s.io"}
	if got := m.RegistryHosts(); !slices.Equal(got, want) {
		t.Errorf("RegistryHosts() = %v, want %v", got, want)
	}
}
