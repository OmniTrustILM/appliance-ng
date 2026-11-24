package appliance

import (
	"slices"
	"testing"

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
