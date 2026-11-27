// Package update decides which bundles an appliance can install and carries out updates.
package update

import (
	"github.com/Masterminds/semver/v3"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// Reasons a bundle cannot be installed now; they are shown in the status and the API.
const (
	OlderThanRunning   = "olderThanRunning"
	OperatorOutOfRange = "operatorOutOfRange"
	NeedsAppliance     = "needsAppliance"
	WrongArch          = "wrongArch"
	SameVersion        = "sameVersion"
	InvalidMetadata    = "invalidMetadata"
)

// Current is what the appliance runs. Arch is always known; an empty Arch matches no bundle. The other fields are
// empty while the component is not installed yet, and are then not compared.
type Current struct {
	Appliance     string
	Operator      string
	ILM           string
	OperatorRange string
	Arch          string
	// platformBundle is the version of the platform bundle installed; empty while the appliance's own platform runs.
	platformBundle string
}

// Check reports why md cannot be installed on cur, empty when it can. needs is the appliance version that has to be
// installed first: set with NeedsAppliance, and with OperatorOutOfRange when the platform bundle also needs a newer
// appliance than the running one; it is empty for every other reason. A bundle whose compatibility fields for its kind
// are empty or unparsable is InvalidMetadata. A platform bundle of the running ILM with an older operator is
// OlderThanRunning, since it would downgrade the operator.
func Check(cur Current, md bundle.Metadata) (blocked, needs string) {
	if cur.Arch == "" || md.Arch != cur.Arch {
		return WrongArch, ""
	}
	if !validMetadata(md) {
		return InvalidMetadata, ""
	}
	if md.Kind == release.BundleSystem {
		return checkSystem(cur, md)
	}
	return checkPlatform(cur, md)
}

func checkSystem(cur Current, md bundle.Metadata) (blocked, needs string) {
	if reason := compareVersion(md.Version, cur.Appliance); reason != "" {
		return reason, ""
	}
	if !inRange(md.OperatorRange, cur.Operator) {
		return OperatorOutOfRange, ""
	}
	return "", ""
}

func checkPlatform(cur Current, md bundle.Metadata) (blocked, needs string) {
	if md.ILM == cur.ILM && md.Operator == cur.Operator && cur.ILM != "" {
		return SameVersion, ""
	}
	ilm := compareVersion(md.ILM, cur.ILM)
	if ilm == OlderThanRunning || ilm == SameVersion && compareVersion(md.Operator, cur.Operator) == OlderThanRunning {
		return OlderThanRunning, ""
	}
	needsSystem := ""
	if older(cur.Appliance, md.MinAppliance) {
		needsSystem = md.MinAppliance
	}
	if !inRange(cur.OperatorRange, md.Operator) {
		return OperatorOutOfRange, needsSystem
	}
	if needsSystem != "" {
		return NeedsAppliance, needsSystem
	}
	return "", ""
}

// validMetadata reports whether the fields the bundle's kind states for compatibility are present and parse.
func validMetadata(md bundle.Metadata) bool {
	versions := []string{md.Version, md.ILM, md.Operator, md.MinAppliance}
	if md.Kind == release.BundleSystem {
		versions = versions[:1]
		if _, err := semver.NewConstraint(md.OperatorRange); err != nil || md.OperatorRange == "" {
			return false
		}
	}
	for _, v := range versions {
		if _, err := semver.NewVersion(v); err != nil {
			return false
		}
	}
	return true
}

// compareVersion returns SameVersion or OlderThanRunning when version equals or precedes running.
func compareVersion(version, running string) string {
	v, errV := semver.NewVersion(version)
	r, errR := semver.NewVersion(running)
	if errV != nil || errR != nil {
		return ""
	}
	switch v.Compare(r) {
	case 0:
		return SameVersion
	case -1:
		return OlderThanRunning
	}
	return ""
}

// older reports whether version precedes than.
func older(version, than string) bool {
	v, errV := semver.NewVersion(version)
	t, errT := semver.NewVersion(than)
	return errV == nil && errT == nil && v.LessThan(t)
}

// skipsMinor reports whether version is more than one minor version above running; what does not parse is not
// judged.
func skipsMinor(version, running string) bool {
	v, errV := semver.NewVersion(version)
	r, errR := semver.NewVersion(running)
	if errV != nil || errR != nil {
		return false
	}
	return v.Major() > r.Major() || v.Major() == r.Major() && v.Minor() > r.Minor()+1
}

// inRange reports whether version satisfies the range; an empty or unparsable range or version is not judged.
func inRange(rng, version string) bool {
	c, errC := semver.NewConstraint(rng)
	v, errV := semver.NewVersion(version)
	if errC != nil || errV != nil {
		return true
	}
	return c.Check(v)
}
