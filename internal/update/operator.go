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

// EmbeddedOperatorOlder tells whether the ILM operator that the release m embeds is older than the running version,
// so that the add-on pass would replace it with an older one once no platform bundle is recorded.
func EmbeddedOperatorOlder(m *release.Manifest, running string) bool {
	embedded, ok := m.Operator()
	return ok && older(embedded.Version, running)
}

// InstalledPlatform returns the metadata of the installed platform bundle version for arch, stored under dir when it
// was loaded; nil when version is empty, as while no platform bundle is installed.
func InstalledPlatform(dir, version, arch string) (*bundle.Metadata, error) {
	if version == "" {
		return nil, nil
	}
	path := filepath.Join(dir, release.BundlePlatform, tag(version, arch), bundle.MetadataFile)
	data, err := os.ReadFile(path) //nolint:gosec // built from the version the status records
	if err != nil {
		return nil, fmt.Errorf("read platform bundle %s: %w", version, err)
	}
	md := &bundle.Metadata{}
	if err := json.Unmarshal(data, md); err != nil {
		return nil, fmt.Errorf("read platform bundle %s: %w", version, err)
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
