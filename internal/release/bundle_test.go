package release

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	operatorImage = "hub.omnitrustregistry.com/ilm/operator:1.1.0"
	cnpgImage     = "ghcr.io/cloudnative-pg/cloudnative-pg:1.29.1"
	privateImage  = "hub.omnitrustregistry.com/ilm-private/secret:1.0.0"
	publicImage   = "hub.omnitrustregistry.com/ilm/public:1.0.0"
)

// bomImages are the images of the operator's bill of materials for ILM 2.20.0 that the appliance deploys.
var bomImages = []string{
	"hub.omnitrustregistry.com/ilm/core:2.20.0",
	"hub.omnitrustregistry.com/ilm/auth:1.8.0",
	"hub.omnitrustregistry.com/ilm/auth-opa-policies:1.4.1",
	"hub.omnitrustregistry.com/ilm/curl:8.16.0",
	"hub.omnitrustregistry.com/ilm/frontend-administrator:2.20.0",
	"hub.omnitrustregistry.com/ilm/kong:3.9.1",
	"hub.omnitrustregistry.com/ilm/keycloak-theme:0.1.4",
	"hub.omnitrustregistry.com/ilm/opa:1.10.0-static",
	"hub.omnitrustregistry.com/ilm/scheduler:1.2.0",
	"ghcr.io/cloudnative-pg/postgresql:18",
	"docker.io/library/rabbitmq:4.3.1-management",
}

func addonWorkload(image string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - image: " + image + "\n"
}

// validManifest is a manifest that passes every check; tests break one thing at a time.
func validManifest() (*Manifest, fstest.MapFS) {
	m := &Manifest{
		Version:       "0.1.0",
		Compatibility: Compatibility{Operator: ">=1.1.0 <2.0.0", MinAppliance: "0.1.0"},
		Platform:      PlatformDefaults{ILMVersion: "2.20.0"},
		Addons: []AddonFile{
			{Name: "ilm-operator", File: "operator.yaml", Version: "v1.1.0", SHA256: hash(addonWorkload(operatorImage))},
			{Name: "cloudnative-pg", File: "cnpg.yaml", Version: "1.29.1", SHA256: hash(addonWorkload(cnpgImage))},
		},
		Images: []Image{
			{Ref: operatorImage, Digest: "sha256:1", Bundle: BundlePlatform},
			{Ref: cnpgImage, Digest: "sha256:2", Bundle: BundleSystem},
			{Ref: "registry.k8s.io/pause:3.10.2", Digest: "sha256:3", Bundle: BundleSystem},
			{Ref: publicImage, Digest: "sha256:4", Bundle: BundlePlatform},
			{Ref: privateImage, Digest: "sha256:5", Bundle: BundlePlatform},
		},
		Connectors: []Connector{
			{Name: "public", Image: publicImage, Digest: "sha256:4"},
			{Name: "secret", Image: privateImage, Digest: "sha256:5", Private: true},
		},
	}
	for i, ref := range bomImages {
		m.Images = append(m.Images, Image{Ref: ref, Digest: "sha256:b" + string(rune('a'+i)), Bundle: BundlePlatform})
	}
	return m, fstest.MapFS{
		"operator.yaml": {Data: []byte(addonWorkload(operatorImage))},
		"cnpg.yaml":     {Data: []byte(addonWorkload(cnpgImage))},
	}
}

func assertOneError(t *testing.T, errs []error, want ...string) {
	t.Helper()
	all := joinErrors(errs)
	if len(errs) != 1 {
		t.Fatalf("want one error, got %v", errs)
	}
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("no error naming %q in %s", w, all)
		}
	}
}

func TestValidManifestPasses(t *testing.T) {
	m, files := validManifest()
	if errs := Validate(m, files); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestEveryImageHasBundle(t *testing.T) {
	m, files := validManifest()
	m.Images[2].Bundle = ""
	m.Images[3].Bundle = "other"

	errs := Validate(m, files)

	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
	all := joinErrors(errs)
	for _, want := range []string{"registry.k8s.io/pause:3.10.2", "has no bundle", publicImage, `unknown bundle "other"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no error naming %q in %s", want, all)
		}
	}
}

func TestPlatformImagesCoverBOM(t *testing.T) {
	m, files := validManifest()
	removed := bomImages[0]
	m.Images = append(m.Images[:5], m.Images[6:]...)

	errs := Validate(m, files)

	assertOneError(t, errs, removed, "bill of materials")
}

func TestPlatformImagesCoverBOMTagMismatch(t *testing.T) {
	m, files := validManifest()
	m.Platform.ILMVersion = "2.19.0"

	if errs := Validate(m, files); len(errs) == 0 {
		t.Fatal("want errors for images that are not the 2.19.0 bill of materials")
	}
	m.Platform.ILMVersion = "0.0.1"
	assertOneError(t, Validate(m, files), "0.0.1", "bill of materials")
}

func TestBOMImagesArePlatform(t *testing.T) {
	m, files := validManifest()
	m.Images[5].Bundle = BundleSystem

	assertOneError(t, Validate(m, files), bomImages[0], "platform")
}

func TestBOMImageListedWithDigestIsMatched(t *testing.T) {
	m, files := validManifest()
	m.Images[5].Ref = bomImages[0] + "@sha256:1234"

	if errs := Validate(m, files); len(errs) != 0 {
		t.Fatalf("want no errors for a digest-qualified bill of materials image, got %v", errs)
	}
	m.Images[5].Bundle = BundleSystem
	assertOneError(t, Validate(m, files), bomImages[0], "platform")
}

func TestConnectorAndOperatorImagesArePlatform(t *testing.T) {
	m, files := validManifest()
	m.Images[0].Bundle = BundleSystem
	m.Images[3].Bundle = BundleSystem
	m.Images[4].Bundle = BundleSystem

	errs := Validate(m, files)

	if len(errs) != 3 {
		t.Fatalf("want 3 errors, got %v", errs)
	}
	all := joinErrors(errs)
	for _, want := range []string{operatorImage, publicImage, privateImage} {
		if !strings.Contains(all, want) {
			t.Errorf("no error naming %q in %s", want, all)
		}
	}
}

func TestAddonImagesAreSystem(t *testing.T) {
	m, files := validManifest()
	m.Images[1].Bundle = BundlePlatform

	assertOneError(t, Validate(m, files), cnpgImage, "system")
}

func TestVersionWithoutV(t *testing.T) {
	for version, valid := range map[string]bool{"0.1.0": true, "12.0.3": true, "v0.1.0": false, "0.1": false, "": false, "1.0.0-rc1": false} {
		m, files := validManifest()
		m.Version = version

		errs := Validate(m, files)

		if valid && len(errs) != 0 {
			t.Errorf("version %q: want no errors, got %v", version, errs)
		}
		if !valid && (len(errs) != 1 || !strings.Contains(errs[0].Error(), "version")) {
			t.Errorf("version %q: want one version error, got %v", version, errs)
		}
	}
}

func TestOperatorInRange(t *testing.T) {
	tests := []struct {
		name, version, operator, wantErr string
	}{
		{"inside", "v1.9.9", ">=1.1.0 <2.0.0", ""},
		{"lower bound", "v1.1.0", ">=1.1.0 <2.0.0", ""},
		{"below", "v1.0.9", ">=1.1.0 <2.0.0", "does not satisfy"},
		{"upper bound", "v2.0.0", ">=1.1.0 <2.0.0", "does not satisfy"},
		{"unset", "v1.1.0", "", "compatibility.operator"},
		{"unparsable", "v1.1.0", "not a range", "compatibility.operator"},
		{"bad add-on version", "latest", ">=1.1.0 <2.0.0", "ilm-operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, files := validManifest()
			m.Addons[0].Version = tt.version
			m.Compatibility.Operator = tt.operator

			errs := Validate(m, files)

			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got %v", errs)
				}
				return
			}
			assertOneError(t, errs, tt.wantErr)
		})
	}
}

func TestOperatorAddonRequired(t *testing.T) {
	m, files := validManifest()
	m.Addons = m.Addons[1:]
	m.Images = m.Images[1:]

	errs := Validate(m, files)

	if !strings.Contains(joinErrors(errs), "ilm-operator") {
		t.Fatalf("want an error naming the ilm-operator add-on, got %v", errs)
	}
}

func TestMinApplianceIsSemver(t *testing.T) {
	m, files := validManifest()
	m.Compatibility.MinAppliance = "soon"

	assertOneError(t, Validate(m, files), "minAppliance")
}

func TestBundleImagesSkipPrivate(t *testing.T) {
	m, _ := validManifest()
	m.Images = append(m.Images, Image{Ref: "quay.io/x/amd-only:1", Digest: "sha256:6", Bundle: BundlePlatform, Arches: []string{"amd64"}})

	var platform []string
	for _, img := range m.BundleImages(BundlePlatform, "arm64") {
		platform = append(platform, img.Ref)
	}

	if strings.Contains(strings.Join(platform, " "), "ilm-private") || strings.Contains(strings.Join(platform, " "), "amd-only") {
		t.Fatalf("platform arm64 images = %v, want neither private nor amd64-only images", platform)
	}
	if len(platform) != 2+len(bomImages) {
		t.Fatalf("platform arm64 images = %v", platform)
	}
	if got := len(m.BundleImages(BundlePlatform, "amd64")); got != len(platform)+1 {
		t.Errorf("platform amd64 has %d images, want %d", got, len(platform)+1)
	}
}

func TestBundleImagesPartitionSystemAndPlatform(t *testing.T) {
	m, _ := validManifest()

	system := m.BundleImages(BundleSystem, "amd64")

	want := []Image{m.Images[1], m.Images[2]}
	if !reflect.DeepEqual(system, want) {
		t.Fatalf("system images = %v, want %v", system, want)
	}
	if got := m.BundleImages("other", "amd64"); len(got) != 0 {
		t.Errorf("unknown bundle has images %v", got)
	}
}

func TestBOMImageRefsNamesMissingComponent(t *testing.T) {
	saved := applianceComponents
	t.Cleanup(func() { applianceComponents = saved })
	applianceComponents = append([]string{"renamed-component"}, saved...)

	_, err := BOMImageRefs("2.20.0")

	if err == nil || !strings.Contains(err.Error(), "renamed-component") {
		t.Fatalf("err = %v, want one naming renamed-component", err)
	}
}
