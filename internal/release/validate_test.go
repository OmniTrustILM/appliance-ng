package release

import (
	"crypto/sha256"
	"encoding/hex"
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

	errs := Validate(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}})

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

	if errs := Validate(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}}); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidateChecksAddonHash(t *testing.T) {
	m := &Manifest{Addons: []AddonFile{{Name: "app", File: "app.yaml", SHA256: "deadbeef"}}}

	errs := Validate(m, fstest.MapFS{"app.yaml": {Data: []byte(workload)}})

	if !strings.Contains(joinErrors(errs), "app.yaml") || !strings.Contains(joinErrors(errs), "sha256") {
		t.Fatalf("want hash error, got %v", errs)
	}
}

func TestValidateReportsMissingAndBrokenAddonFiles(t *testing.T) {
	m := &Manifest{Addons: []AddonFile{
		{Name: "gone", File: "gone.yaml", SHA256: "x"},
		{Name: "bad", File: "bad.yaml", SHA256: hash("a: [")},
	}}

	errs := Validate(m, fstest.MapFS{"bad.yaml": {Data: []byte("a: [")}})

	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
}

func TestValidatePassesWithoutAddons(t *testing.T) {
	if errs := Validate(&Manifest{}, nil); len(errs) != 0 {
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

	errs := Validate(m, nil)

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

			errs := Validate(m, nil)

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
	if errs := Validate(m, fstest.MapFS{"manager.yaml": {Data: []byte(managerWorkload)}}); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
}

func TestValidateReportsUnusedManagerImage(t *testing.T) {
	m := &Manifest{ManagerImage: "appliance.invalid/manager:other"}
	errs := Validate(m, fstest.MapFS{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "manager image appliance.invalid/manager:other is used by no add-on") {
		t.Fatalf("errors = %v, want the unused manager image", errs)
	}
}
