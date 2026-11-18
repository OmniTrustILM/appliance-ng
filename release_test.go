package appliance

import (
	"testing"

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
