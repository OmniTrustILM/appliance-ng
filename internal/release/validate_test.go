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
