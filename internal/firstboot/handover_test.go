package firstboot

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	testPlaceholder = "appliance.invalid/manager:release"
	testManagerRef  = "registry.example/manager:v1"
)

func TestManagerManifestSetsTheImage(t *testing.T) {
	fsys := fstest.MapFS{managerFile: {Data: []byte("image: " + testPlaceholder + "\n")}}
	got, err := ManagerManifest(fsys, testPlaceholder, testManagerRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "image: "+testManagerRef+"\n" {
		t.Errorf("manifest %q, want the placeholder replaced by %s", got, testManagerRef)
	}
}

func TestManagerManifestRejects(t *testing.T) {
	withPlaceholder := fstest.MapFS{managerFile: {Data: []byte("image: " + testPlaceholder + "\n")}}
	tests := map[string]struct {
		fsys             fstest.MapFS
		placeholder, ref string
		want             string
	}{
		"empty reference":     {withPlaceholder, testPlaceholder, "", "manager image not set at build time"},
		"empty placeholder":   {withPlaceholder, "", testManagerRef, "no manager image placeholder"},
		"missing placeholder": {fstest.MapFS{managerFile: {Data: []byte("image: other\n")}}, testPlaceholder, testManagerRef, "does not name"},
		"missing manifest":    {fstest.MapFS{}, testPlaceholder, testManagerRef, managerFile},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ManagerManifest(tt.fsys, tt.placeholder, tt.ref)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestLocalClusterReadsTheAppliance(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Appliance",
		"metadata":   map[string]any{"name": v1alpha1.ApplianceName},
		"spec":       map[string]any{"node": map[string]any{"hostName": testHostName}},
		"status": map[string]any{"conditions": []any{map[string]any{
			"type": v1alpha1.ConditionAddonsReady, "status": "True", "reason": "Ready", "message": "all add-ons are ready",
			"lastTransitionTime": "2026-11-17T10:00:00Z",
		}}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{applianceResource: "ApplianceList"}, obj)
	c := &localCluster{appliances: dyn.Resource(applianceResource)}

	a, err := c.Appliance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Spec.Node.HostName != testHostName || len(a.Status.Conditions) != 1 || a.Status.Conditions[0].Message != "all add-ons are ready" {
		t.Errorf("read %+v, want the stored Appliance", a)
	}
}

func TestLocalClusterReportsAMissingAppliance(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{applianceResource: "ApplianceList"})
	c := &localCluster{appliances: dyn.Resource(applianceResource)}
	if _, err := c.Appliance(context.Background()); err == nil || !strings.Contains(err.Error(), "read the Appliance") {
		t.Errorf("error %v, want a failed read", err)
	}
}

func TestNewLocalCluster(t *testing.T) {
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: ilm
  cluster:
    server: https://192.0.2.10:6443
    insecure-skip-tls-verify: true
users:
- name: admin
  user:
    token: test-token
contexts:
- name: admin
  context:
    cluster: ilm
    user: admin
current-context: admin
`
	if _, err := NewLocalCluster([]byte(kubeconfig)); err != nil {
		t.Errorf("valid kubeconfig rejected: %v", err)
	}
	if _, err := NewLocalCluster([]byte("not a kubeconfig")); err == nil {
		t.Error("invalid kubeconfig accepted")
	}
}
