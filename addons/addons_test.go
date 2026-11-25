package addons

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

var conditionOf = map[string]string{
	"CustomResourceDefinition": "Established",
	"Deployment":               "Available",
	"DaemonSet":                "Rollout",
	"Node":                     "Ready",
	"Platform":                 "Available",
}

func loadSteps(t *testing.T) []kube.Step {
	t.Helper()
	steps, err := kube.LoadSteps(FS)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func TestOrderSteps(t *testing.T) {
	want := []string{"cilium", "storage", "cert-manager", "cnpg", "rabbitmq", "keycloak", "traefik", "ilm-operator"}
	var got []string
	for _, step := range loadSteps(t) {
		got = append(got, step.Name)
		if len(step.Wait) == 0 {
			t.Errorf("step %s has no wait", step.Name)
		}
		for _, w := range step.Wait {
			if cond, ok := conditionOf[w.Kind]; !ok || cond != w.For {
				t.Errorf("step %s: wait for %s %s/%s: %q is not allowed", step.Name, w.Kind, w.Namespace, w.Name, w.For)
			}
			if w.Name == "" && w.Kind != "Node" {
				t.Errorf("step %s: wait for %s has no name", step.Name, w.Kind)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestOrderCoversEveryFile(t *testing.T) {
	m, err := release.Load("../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]bool{}
	for _, addon := range m.Addons {
		pinned[addon.File] = true
	}

	applied := map[string]int{}
	for _, step := range loadSteps(t) {
		for _, file := range step.Files {
			applied[file]++
			if _, err := fs.Stat(FS, file); err != nil {
				t.Errorf("step %s: %v", step.Name, err)
			}
			if !pinned[file] {
				t.Errorf("step %s: %s is not pinned in release.yaml", step.Name, file)
			}
		}
	}

	files, err := fs.Glob(FS, "*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		want := 1
		// The first-boot service applies manager.yaml; no step does.
		if file == "order.yaml" || file == "platform.yaml" || file == "manager.yaml" {
			want = 0
		}
		if applied[file] != want {
			t.Errorf("%s is applied %d times, want %d", file, applied[file], want)
		}
	}
}

func TestPlatformMatchesRelease(t *testing.T) {
	m, err := release.Load("../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(FS, "platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var platform struct {
		Spec struct {
			Version string `json:"version"`
			Common  struct {
				HostName string `json:"hostName"`
			} `json:"common"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &platform); err != nil {
		t.Fatal(err)
	}
	if platform.Spec.Version != m.Platform.ILMVersion {
		t.Errorf("platform.yaml version = %q, release.yaml platform.ilmVersion = %q", platform.Spec.Version, m.Platform.ILMVersion)
	}
	if platform.Spec.Common.HostName != m.Platform.HostName {
		t.Errorf("platform.yaml hostName = %q, release.yaml platform.hostName = %q", platform.Spec.Common.HostName, m.Platform.HostName)
	}
}

func TestManagerManifest(t *testing.T) {
	m, err := release.Load("../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(FS, "manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd, err := os.ReadFile("../config/crd/appliance.otilm.com_appliances.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, crd) {
		t.Error("manager.yaml does not end with the generated Appliance CRD; run make manifests")
	}

	var images []string
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var obj struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatal(err)
		}
		for _, c := range obj.Spec.Template.Spec.Containers {
			images = append(images, c.Image)
		}
	}
	if len(images) != 1 || images[0] != m.ManagerImage {
		t.Errorf("manager.yaml images = %v, want only the placeholder %q", images, m.ManagerImage)
	}
}

func TestChartsMatchRelease(t *testing.T) {
	m, err := release.Load("../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Charts) == 0 {
		t.Fatal("release.yaml pins no chart")
	}
	for _, c := range m.Charts {
		data, err := fs.ReadFile(FS, c.File)
		if err != nil {
			t.Fatalf("chart %s: %v", c.Name, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != c.SHA256 {
			t.Errorf("chart %s (%s): sha256 is %s, release.yaml pins %s", c.Name, c.File, got, c.SHA256)
		}
	}
}

// docs decodes every document of the add-on file.
func docs(t *testing.T, file string) []map[string]any {
	t.Helper()
	data, err := fs.ReadFile(FS, file)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			t.Fatal(err)
		}
		if obj != nil {
			out = append(out, obj)
		}
	}
}

// TestTraefikRedirectsHTTPWithTheLowestPriority keeps port 80 open to more specific routes, such as the ACME HTTP-01
// solver's, and redirects everything else to https.
func TestTraefikRedirectsHTTPWithTheLowestPriority(t *testing.T) {
	type route struct {
		Match       string `json:"match"`
		Priority    int    `json:"priority"`
		Middlewares []struct {
			Name string `json:"name"`
		} `json:"middlewares"`
		Services []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"services"`
	}
	var object struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			EntryPoints    []string `json:"entryPoints"`
			Routes         []route  `json:"routes"`
			RedirectScheme *struct {
				Scheme    string `json:"scheme"`
				Port      string `json:"port"`
				Permanent bool   `json:"permanent"`
			} `json:"redirectScheme"`
			Template struct {
				Spec struct {
					Containers []struct {
						Args []string `json:"args"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	redirects := map[string]bool{}
	var routes []route
	for _, doc := range docs(t, "traefik.yaml") {
		data, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		object.Spec.RedirectScheme, object.Spec.Routes, object.Spec.Template.Spec.Containers = nil, nil, nil
		if err := yaml.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		for _, c := range object.Spec.Template.Spec.Containers {
			for _, arg := range c.Args {
				if strings.Contains(arg, "redirections") {
					t.Errorf("Traefik argument %s redirects the whole entry point", arg)
				}
			}
		}
		if object.Metadata.Namespace != "traefik" {
			continue
		}
		if r := object.Spec.RedirectScheme; object.Kind == "Middleware" && r != nil && r.Scheme == "https" && r.Port == "443" && r.Permanent {
			redirects[object.Metadata.Name] = true
		}
		if object.Kind == "IngressRoute" && slices.Equal(object.Spec.EntryPoints, []string{"web"}) {
			routes = append(routes, object.Spec.Routes...)
		}
	}
	found := slices.ContainsFunc(routes, func(r route) bool {
		return r.Match == "PathPrefix(`/`)" && r.Priority == 1 && len(r.Middlewares) == 1 && redirects[r.Middlewares[0].Name] &&
			len(r.Services) == 1 && r.Services[0].Name == "noop@internal" && r.Services[0].Kind == "TraefikService"
	})
	if !found {
		t.Errorf("no priority 1 route on the web entry point redirects to https on port 443; routes %+v", routes)
	}
}
