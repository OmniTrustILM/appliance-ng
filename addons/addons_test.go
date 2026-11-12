package addons

import (
	"io/fs"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestOnlyPlatformStepIsCreateOnly(t *testing.T) {
	for _, step := range loadSteps(t) {
		if want := step.Name == "platform"; step.CreateOnly != want {
			t.Errorf("step %s: createOnly = %v, want %v", step.Name, step.CreateOnly, want)
		}
	}
}

func TestPlatformStorageFitsDataVolume(t *testing.T) {
	data, err := fs.ReadFile(FS, "platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var platform struct {
		Spec struct {
			Database struct {
				Managed struct {
					Storage struct{ Size resource.Quantity } `json:"storage"`
				} `json:"managed"`
			} `json:"database"`
			Messaging struct {
				Managed struct {
					Storage struct{ Size resource.Quantity } `json:"storage"`
				} `json:"managed"`
			} `json:"messaging"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &platform); err != nil {
		t.Fatal(err)
	}
	requested := platform.Spec.Database.Managed.Storage.Size.Value() + platform.Spec.Messaging.Managed.Storage.Size.Value()

	generated, err := machineconfig.Generate(machineconfig.Input{ClusterName: "ilm", NodeIP: "10.0.2.15", InstallerImage: "registry.example/installer:v1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configloader.NewFromBytes(generated)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range cfg.Documents() {
		if vol, ok := doc.(*block.UserVolumeConfigV1Alpha1); ok && vol.MetaName == "data" {
			if capacity := vol.ProvisioningSpec.ProvisioningMaxSize.Value(); requested > int64(capacity) {
				t.Errorf("Platform requests %d bytes of storage, the data volume holds at most %d", requested, capacity)
			}
			return
		}
	}
	t.Fatal("data volume missing from the machine configuration")
}
