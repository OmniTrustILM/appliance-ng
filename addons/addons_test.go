package addons

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
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
	want := []string{"cilium", "storage", "cert-manager", "cnpg", "rabbitmq", "keycloak", "traefik", "ilm-operator", "platform"}
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
		if file != "order.yaml" && applied[file] != 1 {
			t.Errorf("%s is applied %d times, want once", file, applied[file])
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
