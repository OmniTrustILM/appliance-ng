package update_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const embeddedOperator = "kind: Deployment\nmetadata: {name: embedded}\n"

// storeOperator writes manifest as the operator of the platform bundle version under dir and returns its metadata.
func storeOperator(t *testing.T, dir, version, operator, manifest string) *bundle.Metadata {
	t.Helper()
	bundleDir := filepath.Join(dir, release.BundlePlatform, version+"-"+testArch)
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, bundle.OperatorFile), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return &bundle.Metadata{Kind: release.BundlePlatform, Version: version, Arch: testArch, Operator: operator}
}

// TestActiveOperatorPrefersNewerBundle applies the active platform bundle's operator when it is newer than the
// embedded one.
func TestActiveOperatorPrefersNewerBundle(t *testing.T) {
	dir := t.TempDir()
	active := storeOperator(t, dir, "1.1.0", "v1.2.0", operatorYAML)
	manifest, version, err := update.ActiveOperator([]byte(embeddedOperator), "v1.1.0", active, dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifest) != operatorYAML || version != "v1.2.0" {
		t.Errorf("ActiveOperator = %q, %q; want the bundle's v1.2.0", manifest, version)
	}
}

// TestActiveOperatorFallsBackToEmbedded applies the embedded operator without an active platform bundle, or when the
// active one's operator is not newer.
func TestActiveOperatorFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	for name, active := range map[string]*bundle.Metadata{
		"no active bundle": nil,
		"same operator":    storeOperator(t, dir, "1.1.0", "v1.1.0", operatorYAML),
		"older operator":   storeOperator(t, dir, "1.2.0", "v1.0.0", operatorYAML),
	} {
		t.Run(name, func(t *testing.T) {
			manifest, version, err := update.ActiveOperator([]byte(embeddedOperator), "v1.1.0", active, dir)
			if err != nil || string(manifest) != embeddedOperator || version != "v1.1.0" {
				t.Errorf("ActiveOperator = %q, %q, %v; want the embedded v1.1.0", manifest, version, err)
			}
		})
	}
}

// TestActiveOperatorNeedsTheStoredManifest fails rather than fall back to an older operator when the active bundle's
// manifest is gone.
func TestActiveOperatorNeedsTheStoredManifest(t *testing.T) {
	active := &bundle.Metadata{Kind: release.BundlePlatform, Version: "1.1.0", Arch: testArch, Operator: "v1.2.0"}
	if _, _, err := update.ActiveOperator([]byte(embeddedOperator), "v1.1.0", active, t.TempDir()); err == nil {
		t.Error("ActiveOperator without the stored manifest: want an error")
	}
}

// TestInstalledPlatformReadsTheLoadedBundle returns the metadata a load stored, none without an installed platform
// bundle, and an error when its files are gone.
func TestInstalledPlatformReadsTheLoadedBundle(t *testing.T) {
	e := newEnv(t)
	if _, err := e.loader().Load(t.Context(), release.BundlePlatform, e.upload(e.build(e.platform("1.1.0", "2.20.0")))); err != nil {
		t.Fatal(err)
	}
	md, err := update.InstalledPlatform(e.dir, "1.1.0", testArch)
	if err != nil || md == nil || md.Version != "1.1.0" || md.Operator != testOperator || md.ILM != "2.20.0" {
		t.Fatalf("InstalledPlatform = %+v, %v", md, err)
	}
	if md, err := update.InstalledPlatform(e.dir, "", testArch); md != nil || err != nil {
		t.Errorf("InstalledPlatform without a version = %+v, %v; want none", md, err)
	}
	if _, err := update.InstalledPlatform(e.dir, "1.2.0", testArch); err == nil {
		t.Error("InstalledPlatform of a bundle that is not stored: want an error")
	}
}

// TestAvailableILM lists the ILM version the release caches, then those of the loaded platform bundles, once each.
func TestAvailableILM(t *testing.T) {
	m := &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.19.0"}}
	loaded := []v1alpha1.BundleInfo{
		{Kind: release.BundleSystem, Version: "1.1.0"},
		{Kind: release.BundlePlatform, Version: "1.1.0", ILM: "2.20.0"},
		{Kind: release.BundlePlatform, Version: "1.2.0", ILM: "2.19.0"},
	}
	if got := update.AvailableILM(m, loaded); !slices.Equal(got, []string{"2.19.0", "2.20.0"}) {
		t.Errorf("AvailableILM = %v", got)
	}
	if got := update.AvailableILM(m, nil); !slices.Equal(got, []string{"2.19.0"}) {
		t.Errorf("AvailableILM without loaded bundles = %v", got)
	}
}

// TestAvailableILMSkipsBlockedBundles leaves out the loaded platform bundles that cannot run on this appliance, and
// keeps those blocked only for being older than or the same as what runs.
func TestAvailableILMSkipsBlockedBundles(t *testing.T) {
	m := &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.20.0"}}
	loaded := []v1alpha1.BundleInfo{
		{Kind: release.BundlePlatform, Version: "1.1.0", ILM: "2.21.0", Blocked: update.NeedsAppliance},
		{Kind: release.BundlePlatform, Version: "1.2.0", ILM: "2.22.0", Blocked: update.OperatorOutOfRange},
		{Kind: release.BundlePlatform, Version: "1.3.0", ILM: "2.23.0", Blocked: update.WrongArch},
		{Kind: release.BundlePlatform, Version: "1.4.0", ILM: "2.24.0", Blocked: update.InvalidMetadata},
		{Kind: release.BundlePlatform, Version: "0.9.0", ILM: "2.19.0", Blocked: update.OlderThanRunning},
		{Kind: release.BundlePlatform, Version: "0.9.1", ILM: "2.18.0", Blocked: update.SameVersion},
	}
	if got := update.AvailableILM(m, loaded); !slices.Equal(got, []string{"2.20.0", "2.19.0", "2.18.0"}) {
		t.Errorf("AvailableILM = %v", got)
	}
}

// TestPlatformBundleFor names the loaded platform bundle that runs an ILM version, none for the release's own.
func TestPlatformBundleFor(t *testing.T) {
	m := &release.Manifest{Platform: release.PlatformDefaults{ILMVersion: "2.19.0"}}
	loaded := []v1alpha1.BundleInfo{
		{Kind: release.BundlePlatform, Version: "1.1.0", ILM: "2.20.0", Blocked: update.WrongArch},
		{Kind: release.BundlePlatform, Version: "1.2.0", ILM: "2.20.0", Blocked: update.OlderThanRunning},
		{Kind: release.BundlePlatform, Version: "1.3.0", ILM: "2.19.0"},
	}
	for ilm, want := range map[string]string{"2.19.0": "", "2.20.0": "1.2.0", "2.21.0": ""} {
		if got := update.PlatformBundleFor(m, loaded, ilm); got != want {
			t.Errorf("PlatformBundleFor(%s) = %q, want %q", ilm, got, want)
		}
	}
}

// TestTagPlatform tags the images of a loaded platform bundle, and fails for one that is not loaded.
func TestTagPlatform(t *testing.T) {
	e := newJobEnv(t, shippedOperator)
	if err := update.TagPlatform(t.Context(), e.tagger, e.dir, toBundle, testArch); err != nil {
		t.Fatal(err)
	}
	if e.tagger.calls() != 1 || len(e.tagger.tagged[0]) != 2 {
		t.Errorf("tagged %v, want the bundle's two images", e.tagger.tagged)
	}
	for _, version := range []string{"", "9.9.9"} {
		if err := update.TagPlatform(t.Context(), e.tagger, e.dir, version, testArch); err == nil {
			t.Errorf("TagPlatform(%q): want an error", version)
		}
	}
}
