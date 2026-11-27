package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// ActiveOperator returns the ILM operator manifest to apply and its version: the active platform bundle's, stored
// under dir, when its operator is newer than the embedded one, else the embedded one. active is nil while no platform
// bundle is installed.
func ActiveOperator(embedded []byte, embeddedVersion string, active *bundle.Metadata, dir string) ([]byte, string, error) {
	if !bundleOperator(embeddedVersion, active) {
		return embedded, embeddedVersion, nil
	}
	path := filepath.Join(dir, release.BundlePlatform, tag(active.Version, active.Arch), bundle.OperatorFile)
	manifest, err := os.ReadFile(path) //nolint:gosec // built from the verified metadata of a loaded bundle
	if err != nil {
		return nil, "", fmt.Errorf("read the ILM operator of platform bundle %s: %w", active.Version, err)
	}
	return manifest, active.Operator, nil
}

// bundleOperator tells whether the add-on pass applies the ILM operator of the platform bundle md: md is installed
// and its operator is newer than the embedded one.
func bundleOperator(embedded string, md *bundle.Metadata) bool {
	return md != nil && older(embedded, md.Operator)
}

// appliedOperator is the ILM operator version the add-on pass applies with the platform bundle md recorded, nil while
// none is: md's when it is newer than the embedded one, else the embedded one.
func appliedOperator(embedded string, md *bundle.Metadata) string {
	if !bundleOperator(embedded, md) {
		return embedded
	}
	return md.Operator
}

// operatorDowngrade tells whether recording the platform bundle next, nil for the release's own platform, has the
// add-on pass apply an older ILM operator than the newer of running and the one the installed platform bundle
// applies.
func operatorDowngrade(embedded string, installed, next *bundle.Metadata, running string) bool {
	active := appliedOperator(embedded, installed)
	if older(active, running) {
		active = running
	}
	return older(appliedOperator(embedded, next), active)
}

// OperatorDowngrade tells whether recording the loaded platform bundle next, empty for the release m's own platform,
// has the add-on pass apply an older ILM operator than the one the versions v run, as a fresh restore after a failed
// one from a newer bundle would.
func OperatorDowngrade(m *release.Manifest, loaded []v1alpha1.BundleInfo, v v1alpha1.Versions, next string) bool {
	embedded, _ := m.Operator()
	return operatorDowngrade(embedded.Version, loadedPlatform(loaded, v.PlatformBundle), loadedPlatform(loaded, next),
		v.Operator)
}

// loadedPlatform is the operator of the runnable loaded platform bundle version as metadata; nil when version is empty
// or no such bundle is loaded.
func loadedPlatform(loaded []v1alpha1.BundleInfo, version string) *bundle.Metadata {
	for _, b := range loaded {
		if version != "" && runnable(b) && b.Version == version {
			return &bundle.Metadata{Operator: b.Operator}
		}
	}
	return nil
}

// InstalledPlatform returns the metadata of the installed platform bundle version for arch, stored under dir when it
// was loaded; nil when version is empty, as while no platform bundle is installed.
func InstalledPlatform(dir, version, arch string) (*bundle.Metadata, error) {
	return installed(dir, release.BundlePlatform, version, arch)
}

// installed returns the metadata of the loaded bundle of kind and version for arch, stored under dir; nil when version
// is empty.
func installed(dir, kind, version, arch string) (*bundle.Metadata, error) {
	if version == "" {
		return nil, nil
	}
	path := filepath.Join(dir, kind, tag(version, arch), bundle.MetadataFile)
	data, err := os.ReadFile(path) //nolint:gosec // built from the version the status records
	if err != nil {
		return nil, fmt.Errorf("read %s bundle %s: %w", kind, version, err)
	}
	md := &bundle.Metadata{}
	if err := json.Unmarshal(data, md); err != nil {
		return nil, fmt.Errorf("read %s bundle %s: %w", kind, version, err)
	}
	return md, nil
}

// AvailableILM lists the ILM versions the appliance can run: the one m caches, then those of the loaded platform
// bundles, each once. A bundle blocked for another reason than being older than or the same as what runs cannot run
// here and is left out.
func AvailableILM(m *release.Manifest, loaded []v1alpha1.BundleInfo) []string {
	available := []string{m.Platform.ILMVersion}
	for _, b := range loaded {
		if runnable(b) && !slices.Contains(available, b.ILM) {
			available = append(available, b.ILM)
		}
	}
	return available
}

// PlatformBundleFor is the version of the loaded platform bundle that runs ILM version ilm here, as AvailableILM
// lists it; empty when the release m caches ilm or no loaded bundle runs it.
func PlatformBundleFor(m *release.Manifest, loaded []v1alpha1.BundleInfo, ilm string) string {
	if ilm == m.Platform.ILMVersion {
		return ""
	}
	for _, b := range loaded {
		if runnable(b) && b.ILM == ilm {
			return b.Version
		}
	}
	return ""
}

// runnable tells whether b is a platform bundle that can run here: one blocked for another reason than being older
// than or the same as what runs cannot.
func runnable(b v1alpha1.BundleInfo) bool {
	return b.Kind == release.BundlePlatform && (b.Blocked == "" || b.Blocked == OlderThanRunning || b.Blocked == SameVersion)
}

// TagPlatform tags the images of the loaded platform bundle version for arch, stored under dir, in reg under their
// upstream tags, which workloads pull through the registry mirror.
func TagPlatform(ctx context.Context, reg ImageTagger, dir, version, arch string) error {
	md, err := InstalledPlatform(dir, version, arch)
	if err != nil {
		return err
	}
	if md == nil {
		return errors.New("no platform bundle to tag")
	}
	return reg.TagImages(ctx, md.Images)
}
