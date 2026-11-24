package release

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/OmniTrustILM/operator/pkg/bom"
)

// Bundles an image travels in.
const (
	// BundleSystem holds the operating system, Kubernetes, the add-ons and the manager.
	BundleSystem = "system"
	// BundlePlatform holds the ILM operator, the ILM platform and the connectors.
	BundlePlatform = "platform"
)

// operatorAddon is the add-on that installs the ILM operator; its images belong to the platform bundle.
const operatorAddon = "ilm-operator"

// applianceComponents are the components of the operator's bill of materials that the appliance deploys.
var applianceComponents = []string{
	"core", "auth", "auth-opa-policies", "curl", "fe-administrator", "api-gateway", "keycloak-theme", "opa", "scheduler",
}

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// BundleImages returns the images of bundle kind that arch needs. The platform bundle leaves out the images of
// private connectors, which need registry credentials and are not cached.
func (m *Manifest) BundleImages(kind, arch string) []Image {
	private := map[string]bool{}
	if kind == BundlePlatform {
		private = privateImages(m)
	}
	var images []Image
	for _, img := range m.Images {
		if img.Bundle == kind && !private[normalize(img.Ref)] && img.forArch(arch) {
			images = append(images, img)
		}
	}
	return images
}

func (i Image) forArch(arch string) bool {
	return len(i.Arches) == 0 || slices.Contains(i.Arches, arch)
}

// requirement is the bundle an image must be in and why.
type requirement struct{ bundle, reason string }

// validateBundles checks the release version, the compatibility, and that each image is in the bundle its role
// demands. usage maps each image the add-ons use, normalized, to the names of the add-ons using it.
func validateBundles(m *Manifest, usage map[string][]string) []error {
	errs := validateCompatibility(m)
	errs = append(errs, validateBundleNames(m)...)
	required, reqErrs := requiredBundles(m, usage)
	errs = append(errs, reqErrs...)
	for _, img := range m.Images {
		want, ok := required[tagRef(img.Ref)]
		if ok && img.Bundle != want.bundle && slices.Contains([]string{BundleSystem, BundlePlatform}, img.Bundle) {
			errs = append(errs, fmt.Errorf("image %s is %s, so it belongs in the %s bundle, not %s", img.Ref, want.reason, want.bundle, img.Bundle))
		}
	}
	return errs
}

// validateBundleNames checks that each image names a known bundle.
func validateBundleNames(m *Manifest) []error {
	var errs []error
	for _, img := range m.Images {
		switch img.Bundle {
		case BundleSystem, BundlePlatform:
		case "":
			errs = append(errs, fmt.Errorf("image %s has no bundle", img.Ref))
		default:
			errs = append(errs, fmt.Errorf("image %s: unknown bundle %q", img.Ref, img.Bundle))
		}
	}
	return errs
}

// tagRef is the normalized reference without its digest, so a digest-qualified reference matches the tag-only
// references of the bill of materials.
func tagRef(ref string) string {
	name, _, _ := strings.Cut(normalize(ref), "@")
	return name
}

// requiredBundles maps each normalized image the add-ons, the connectors and the bill of materials use to the bundle
// it must be in, and reports the images of the bill of materials the manifest does not list.
func requiredBundles(m *Manifest, usage map[string][]string) (map[string]requirement, []error) {
	var errs []error
	listed := map[string]bool{}
	for _, img := range m.Images {
		listed[tagRef(img.Ref)] = true
	}
	required := map[string]requirement{}
	for ref, addons := range usage {
		if slices.Contains(addons, operatorAddon) {
			required[tagRef(ref)] = requirement{BundlePlatform, "an image of the " + operatorAddon + " add-on"}
		} else {
			required[tagRef(ref)] = requirement{BundleSystem, "an add-on image"}
		}
	}
	for _, c := range m.Connectors {
		required[tagRef(c.Image)] = requirement{BundlePlatform, "a connector image"}
	}
	bomRefs, err := BOMImageRefs(m.Platform.ILMVersion)
	if err != nil {
		errs = append(errs, err)
	}
	for _, ref := range bomRefs {
		required[tagRef(ref)] = requirement{BundlePlatform, "in the bill of materials of ILM " + m.Platform.ILMVersion}
		if !listed[tagRef(ref)] {
			errs = append(errs, fmt.Errorf("image %s of the bill of materials of ILM %s is not listed in the manifest", ref, m.Platform.ILMVersion))
		}
	}
	return required, errs
}

// validateCompatibility checks the release version and the compatibility against the ILM operator add-on.
func validateCompatibility(m *Manifest) []error {
	var errs []error
	if !releaseVersionPattern.MatchString(m.Version) {
		errs = append(errs, fmt.Errorf("version %q must be X.Y.Z without a v", m.Version))
	}
	if !releaseVersionPattern.MatchString(m.Compatibility.MinAppliance) {
		errs = append(errs, fmt.Errorf("compatibility.minAppliance %q must be X.Y.Z without a v", m.Compatibility.MinAppliance))
	}
	constraint, err := semver.NewConstraint(m.Compatibility.Operator)
	if err != nil {
		return append(errs, fmt.Errorf("compatibility.operator %q is not a version range: %w", m.Compatibility.Operator, err))
	}
	for _, addon := range m.Addons {
		if addon.Name != operatorAddon {
			continue
		}
		version, err := semver.NewVersion(addon.Version)
		if err != nil {
			return append(errs, fmt.Errorf("add-on %s: version %q: %w", addon.Name, addon.Version, err))
		}
		if !constraint.Check(version) {
			errs = append(errs, fmt.Errorf("add-on %s version %s does not satisfy compatibility.operator %q", addon.Name, addon.Version, m.Compatibility.Operator))
		}
		return errs
	}
	return append(errs, fmt.Errorf("the manifest has no %s add-on to check against compatibility.operator", operatorAddon))
}

// BOMImageRefs returns the references of the images the operator's bill of materials names for ilmVersion that the
// appliance deploys: its components and the managed databases and message broker.
func BOMImageRefs(ilmVersion string) ([]string, error) {
	b, ok := bom.BundleFor(ilmVersion)
	if !ok {
		return nil, fmt.Errorf("the operator has no bill of materials for ILM version %q", ilmVersion)
	}
	var refs []string
	for _, name := range applianceComponents {
		img, ok := b.Components[name]
		if !ok {
			return nil, fmt.Errorf("the bill of materials of ILM %s has no component %s", ilmVersion, name)
		}
		repository := img.Repository
		if repository == "" {
			repository = bom.DefaultImageRepository
		}
		refs = append(refs, fmt.Sprintf("%s/%s/%s:%s", bom.DefaultImageRegistry, repository, img.Name, img.Tag))
	}
	refs = append(refs,
		"ghcr.io/cloudnative-pg/postgresql:"+b.CNPGVersion,
		"rabbitmq:"+b.RabbitMQVersion+"-management",
	)
	return refs, nil
}
