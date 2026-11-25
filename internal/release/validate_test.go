package release

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const workload = `apiVersion: apps/v1
kind: Deployment
metadata: { name: app }
spec:
  template:
    spec:
      containers:
        - name: app
          image: a/x:1
          args: ["--helper-image=b/y:2"]
---
apiVersion: v1
kind: ConfigMap
metadata: { name: cfg }
data:
  config.yaml: |
    image: busybox
    url: https://example.com/path:80
`

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestValidateFindsUnlistedImage(t *testing.T) {
	m := &Manifest{
		ImplicitImages: []string{"busybox"},
		Addons:         []AddonFile{{Name: "app", File: "app.yaml", SHA256: hash(workload)}},
	}

	errs := contentErrors(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}})

	if len(errs) != 3 {
		t.Fatalf("want 3 errors, got %v", errs)
	}
	all := joinErrors(errs)
	for _, want := range []string{"a/x:1", "docker.io/library/busybox:latest", "b/y:2"} {
		if !strings.Contains(all, want) {
			t.Errorf("no error naming %s in %s", want, all)
		}
	}
}

func TestValidateAcceptsListedImages(t *testing.T) {
	m := &Manifest{
		ImplicitImages: []string{"busybox"},
		Addons:         []AddonFile{{Name: "app", File: "app.yaml", SHA256: hash(workload)}},
		Images: []Image{
			{Ref: "docker.io/a/x:1", Digest: "sha256:1"},
			{Ref: "b/y:2", Digest: "sha256:2"},
			{Ref: "busybox", Digest: "sha256:3"},
			{Ref: "unused/z:9", Digest: "sha256:4"},
		},
	}

	if errs := contentErrors(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}}); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidateChecksAddonHash(t *testing.T) {
	m := &Manifest{Addons: []AddonFile{{Name: "app", File: "app.yaml", SHA256: "deadbeef"}}}

	errs := contentErrors(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}})

	if !strings.Contains(joinErrors(errs), "app.yaml") || !strings.Contains(joinErrors(errs), "sha256") {
		t.Fatalf("want hash error, got %v", errs)
	}
}

func TestValidateReportsMissingAndBrokenAddonFiles(t *testing.T) {
	m := &Manifest{Addons: []AddonFile{
		{Name: "gone", File: "gone.yaml", SHA256: "x"},
		{Name: "bad", File: "bad.yaml", SHA256: hash("a: [")},
	}}

	errs := contentErrors(m, fstest.MapFS{"bad.yaml": {Data: []byte("a: [")}})

	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}

func TestValidatePassesWithoutAddons(t *testing.T) {
	if errs := contentErrors(&Manifest{}, nil); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, err := range errs {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "\n")
}

func TestAddonImageNames(t *testing.T) {
	m := &Manifest{
		ImplicitImages: []string{"busybox"},
		Addons:         []AddonFile{{Name: "app", File: "app.yaml", SHA256: hash(workload)}},
		Images: []Image{
			{Ref: "a/x:1", Digest: "sha256:1"},
			{Ref: "b/y:2", Digest: "sha256:2"},
			{Ref: "docker.io/library/busybox:latest", Digest: "sha256:3"},
			{Ref: "c/unused:3", Digest: "sha256:4"},
		},
	}

	got, err := AddonImageNames(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"a/x", "b/y", "docker.io/library/busybox"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestAddonImageNamesReportsBadAddon(t *testing.T) {
	m := &Manifest{Addons: []AddonFile{{Name: "app", File: "app.yaml", SHA256: "bad"}}}

	if _, err := AddonImageNames(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}}); err == nil {
		t.Error("want an error for a hash mismatch")
	}
}

func TestValidateConnectorImagesListed(t *testing.T) {
	m := &Manifest{
		Images: []Image{{Ref: "registry.example/ilm/listed:1", Digest: "sha256:1"}},
		Connectors: []Connector{
			{Name: "listed", Image: "registry.example/ilm/listed:1", Digest: "sha256:1"},
			{Name: "missing", Image: "registry.example/ilm/missing:1", Digest: "sha256:2"},
			{Name: "stale", Image: "registry.example/ilm/listed:1", Digest: "sha256:3"},
		},
	}

	errs := contentErrors(m, nil)

	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
	all := joinErrors(errs)
	for _, want := range []string{"connector missing", "registry.example/ilm/missing:1", "connector stale", "sha256:3"} {
		if !strings.Contains(all, want) {
			t.Errorf("no error naming %s in %s", want, all)
		}
	}
}

func TestValidateConnectorFields(t *testing.T) {
	tests := []struct {
		name      string
		connector Connector
		wantErr   string
	}{
		{"jdbc", Connector{Database: DatabaseJDBC, Schema: "s"}, ""},
		{"env", Connector{Database: DatabaseEnv, Schema: "s"}, ""},
		{"no database", Connector{Settings: []string{SettingProxy, SettingTrust, SettingMail}}, ""},
		{"unknown database", Connector{Database: "mysql"}, `unknown database "mysql"`},
		{"schema without database", Connector{Schema: "s"}, "schema s set without a database"},
		{"unknown setting", Connector{Settings: []string{"proxy", "smtp"}}, `unknown setting "smtp"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.connector
			c.Name, c.Image, c.Digest = "conn", "registry.example/ilm/conn:1", "sha256:1"
			m := &Manifest{Images: []Image{{Ref: c.Image, Digest: c.Digest}}, Connectors: []Connector{c}}

			errs := contentErrors(m, nil)

			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got %v", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), "connector conn") || !strings.Contains(errs[0].Error(), tt.wantErr) {
				t.Fatalf("want one error naming conn and %s, got %v", tt.wantErr, errs)
			}
		})
	}
}

const managerWorkload = `apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - image: appliance.invalid/manager:release
`

func TestValidateAcceptsManagerImage(t *testing.T) {
	m := &Manifest{
		ManagerImage: "appliance.invalid/manager:release",
		Addons:       []AddonFile{{Name: "manager", File: "manager.yaml", SHA256: hash(managerWorkload)}},
	}
	if errs := contentErrors(m, fstest.MapFS{"manager.yaml": {Data: []byte(managerWorkload)}}); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidateReportsUnusedManagerImage(t *testing.T) {
	m := &Manifest{ManagerImage: "appliance.invalid/manager:other"}
	errs := contentErrors(m, fstest.MapFS{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "manager image appliance.invalid/manager:other is used by no add-on") {
		t.Fatalf("errors = %v, want the unused manager image", errs)
	}
}

func TestValidateChecksTheChartPins(t *testing.T) {
	m := &Manifest{Charts: []Chart{{Name: "repo", File: "charts/repo-1.tgz", SHA256: hash("chart")}}}
	if errs := contentErrors(m, fstest.MapFS{"charts/repo-1.tgz": {Data: []byte("chart")}}); len(errs) != 0 {
		t.Fatalf("pinned chart refused: %v", errs)
	}
	errs := contentErrors(m, fstest.MapFS{"charts/repo-1.tgz": {Data: []byte("other")}})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "charts/repo-1.tgz") {
		t.Fatalf("errors %v, want the changed chart", errs)
	}
}

// contentErrors is Validate without the bundle and compatibility checks.
func contentErrors(m *Manifest, addons fs.FS) []error {
	errs, _ := validateContent(m, addons)
	return errs
}

func TestImagesIn(t *testing.T) {
	data := []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - image: hub.omnitrustregistry.com/ilm/operator:1.1.0\n")

	got, err := ImagesIn(data)

	if err != nil || len(got) != 1 || got[0] != "hub.omnitrustregistry.com/ilm/operator:1.1.0" {
		t.Fatalf("ImagesIn = %v, %v", got, err)
	}
}

const testArchDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestValidateArchDigests(t *testing.T) {
	const pinned = "@sha256:1234"
	cases := []struct {
		name   string
		change func(img *Image)
		want   string
	}{
		{name: "pinned ref with every arch", change: func(img *Image) {
			img.Ref += pinned
			img.ArchDigests = map[string]string{"amd64": testArchDigest, "arm64": testArchDigest}
		}},
		{name: "pinned ref limited to an arch", change: func(img *Image) {
			img.Ref += pinned
			img.Arches = []string{"arm64"}
			img.ArchDigests = map[string]string{"arm64": testArchDigest}
		}},
		{name: "pinned ref without per-arch digests", change: func(img *Image) {
			img.Ref += pinned
		}, want: "no archDigests entry for amd64"},
		{name: "pinned ref lacking an arch", change: func(img *Image) {
			img.Ref += pinned
			img.ArchDigests = map[string]string{"amd64": testArchDigest}
		}, want: "no archDigests entry for arm64"},
		{name: "malformed per-arch digest", change: func(img *Image) {
			img.Ref += pinned
			img.ArchDigests = map[string]string{"amd64": testArchDigest, "arm64": "sha256:12"}
		}, want: "archDigests arm64"},
		{name: "per-arch digest of an arch the image is not for", change: func(img *Image) {
			img.Ref += pinned
			img.Arches = []string{"amd64"}
			img.ArchDigests = map[string]string{"amd64": testArchDigest, "arm64": testArchDigest}
		}, want: "archDigests arm64"},
		{name: "per-arch digests without a pinned ref", change: func(img *Image) {
			img.ArchDigests = map[string]string{"amd64": testArchDigest, "arm64": testArchDigest}
		}, want: "does not pin a digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, files := validManifest()
			tc.change(&m.Images[5])
			errs := Validate(m, files)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate: %v, want no errors", errs)
				}
				return
			}
			assertOneError(t, errs, m.Images[5].Ref, tc.want)
		})
	}
}
