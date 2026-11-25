package firstboot

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/setup"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
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

func TestApplianceStateIgnoresStaleConditions(t *testing.T) {
	const generation = 3
	cond := func(kind string, observed int64, message string) metav1.Condition {
		return metav1.Condition{Type: kind, Status: metav1.ConditionTrue, ObservedGeneration: observed, Message: message}
	}
	addons := cond(v1alpha1.ConditionAddonsReady, generation, "all add-ons are ready")
	platform := cond(v1alpha1.ConditionPlatformReady, generation, "the Platform is available")
	staleAddons := cond(v1alpha1.ConditionAddonsReady, generation-1, "all add-ons are ready")
	stalePlatform := cond(v1alpha1.ConditionPlatformReady, generation-1, "the Platform is available")

	tests := map[string]struct {
		conditions  []metav1.Condition
		wantState   State
		wantMessage string
	}{
		"current":        {[]metav1.Condition{addons, platform}, StateReady, "ILM Platform is ready"},
		"stale add-ons":  {[]metav1.Condition{staleAddons, platform}, StateAddons, "waiting for the Appliance Manager"},
		"stale platform": {[]metav1.Condition{addons, stalePlatform}, StatePlatform, "waiting for the ILM Platform"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Generation: generation}}
			a.Status.Conditions = tt.conditions
			state, message := applianceState(a)
			if state != tt.wantState || message != tt.wantMessage {
				t.Errorf("state %q %q, want %q %q", state, message, tt.wantState, tt.wantMessage)
			}
		})
	}
}

const handoverContent = `{"version":1,"admin":{"user":"admin","totpSeed":"SEED-MARKER"}}`

func handoverCondition(status metav1.ConditionStatus, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionHandoverComplete, Status: status, Reason: "Test", Message: message}
}

// TestFirstbootRecreatesHandoverSecretWhileFileExists starts first-boot with the hand-over file in the machine
// configuration, as after each restart until the manager removed it: it creates the hand-over Secret until an
// Appliance reports the hand-over complete, and never the default Appliance.
func TestFirstbootRecreatesHandoverSecretWhileFileExists(t *testing.T) {
	tests := []struct {
		name       string
		appliance  bool
		handover   *metav1.Condition
		wantSecret bool
	}{
		{name: "no Appliance yet", wantSecret: true},
		{name: "import under way", appliance: true, handover: &metav1.Condition{Status: metav1.ConditionFalse}, wantSecret: true},
		{name: "Appliance without hand-over", appliance: true, wantSecret: true},
		{name: "hand-over complete", appliance: true, handover: &metav1.Condition{Status: metav1.ConditionTrue}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
			cluster := &fakeCluster{}
			if tt.appliance {
				cluster.appliance = existingAppliance()
			}
			if tt.handover != nil {
				cluster.setConditions(handoverCondition(tt.handover.Status, "importing"))
			}
			node.handover = []byte(handoverContent)
			h := newHarness(node, cluster)
			h.start(t)

			eventually(t, "the Appliance read", func() bool { _, _, reads := cluster.snapshot(); return reads >= 2 })
			h.stop(t)

			_, created, _ := cluster.snapshot()
			if len(created) != 0 {
				t.Errorf("created %q, want no default Appliance on a node with a hand-over file", created)
			}
			secrets := cluster.createdSecrets()
			if !tt.wantSecret {
				if len(secrets) != 0 {
					t.Errorf("created %d Secrets, want none once the hand-over is complete", len(secrets))
				}
				return
			}
			if len(secrets) != 1 {
				t.Fatalf("created %d Secrets, want the hand-over Secret once", len(secrets))
			}
			s := secrets[0]
			if s.Namespace != setup.HandoverSecretNamespace || s.Name != setup.HandoverSecretName ||
				string(s.Data[setup.HandoverSecretKey]) != handoverContent {
				t.Errorf("Secret %s/%s with keys %v, want the hand-over file in %s/%s", s.Namespace, s.Name,
					slices.Collect(maps.Keys(s.Data)), setup.HandoverSecretNamespace, setup.HandoverSecretName)
			}
			for name, out := range map[string]string{"log": h.log.String(), "status": h.store.Get().Message} {
				if strings.Contains(out, "SEED-MARKER") {
					t.Errorf("%s quotes the hand-over file:\n%s", name, out)
				}
			}
		})
	}
}

func TestHandOverAnswersSplitsTheSetupCertificate(t *testing.T) {
	const content = `{"version":1,"admin":{"user":"admin"},"tls":{"certPEM":"CERT-MARKER","keyPEM":"KEY-MARKER"}}`
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{}
	node.handover = []byte(content)
	h := newHarness(node, cluster)
	h.start(t)

	eventually(t, "both Secrets", func() bool { return len(cluster.createdSecrets()) >= 2 })
	h.stop(t)

	secrets := cluster.createdSecrets()
	tlsSecret, answers := secrets[0], secrets[1]
	if tlsSecret.Namespace != setup.HandoverSecretNamespace || tlsSecret.Name != setup.SetupTLSSecretName ||
		tlsSecret.Type != corev1.SecretTypeTLS || string(tlsSecret.Data[corev1.TLSCertKey]) != "CERT-MARKER" ||
		string(tlsSecret.Data[corev1.TLSPrivateKeyKey]) != "KEY-MARKER" {
		t.Errorf("setup certificate Secret %s/%s type %s keys %v", tlsSecret.Namespace, tlsSecret.Name, tlsSecret.Type,
			slices.Collect(maps.Keys(tlsSecret.Data)))
	}
	if answers.Name != setup.HandoverSecretName {
		t.Fatalf("second Secret is %s, want the hand-over Secret", answers.Name)
	}
	if got := string(answers.Data[setup.HandoverSecretKey]); strings.Contains(got, "tls") || strings.Contains(got, "MARKER") ||
		!strings.Contains(got, `"admin"`) {
		t.Errorf("hand-over Secret holds %q, want the answers without the certificate", got)
	}
	if strings.Contains(h.log.String(), "KEY-MARKER") || strings.Contains(h.store.Get().Message, "KEY-MARKER") {
		t.Error("the key is quoted in the log or status")
	}
}

func TestHandOverAnswersWithoutCertificateCreatesNoTLSSecret(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{}
	node.handover = []byte(handoverContent)
	h := newHarness(node, cluster)
	h.start(t)

	eventually(t, "the hand-over Secret", func() bool { return len(cluster.createdSecrets()) >= 1 })
	h.stop(t)

	for _, s := range cluster.createdSecrets() {
		if s.Name == setup.SetupTLSSecretName {
			t.Error("created a setup certificate Secret for a file without one")
		}
	}
}

func TestRunWaitsForTheImportedAppliance(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{}
	node.handover = []byte(handoverContent)
	h := newHarness(node, cluster)
	h.start(t)

	eventually(t, "waiting for the import", func() bool {
		st := h.store.Get()
		return st.State == StateHandover && st.Message == "waiting for the Appliance Manager to import the setup"
	})
	cluster.mu.Lock()
	cluster.appliance = existingAppliance()
	cluster.mu.Unlock()
	cluster.setConditions(handoverCondition(metav1.ConditionFalse, "waiting for access to the Talos API"))
	eventually(t, "the import condition", func() bool {
		st := h.store.Get()
		return st.State == StateHandover && st.Message == "waiting for access to the Talos API"
	})
	cluster.setConditions(handoverCondition(metav1.ConditionTrue, "the setup answers are imported"))
	h.waitState(t, StateAddons)
}

func TestRunReportsAFailedHandoverWithoutDetails(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{createErr: errors.New("create Secret: invalid value SEED-MARKER")}
	node.handover = []byte(handoverContent)
	h := newHarness(node, cluster)
	h.start(t)

	eventually(t, "the failure in the status", func() bool {
		return strings.HasPrefix(h.store.Get().Message, "create the hand-over Secret failed")
	})
	h.stop(t)
	if out := h.log.String() + h.store.Get().Message; strings.Contains(out, "SEED-MARKER") {
		t.Errorf("log or status quotes the error:\n%s", out)
	}
}

func TestRunReportsAnUnreadableHandoverFile(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	// The machine configuration holds the hand-over, so the error of reading it is reported by its code only.
	node.handoverErr = errors.New("read machine configuration: cannot parse SEED-MARKER")
	h := newHarness(node, &fakeCluster{})
	h.start(t)

	eventually(t, "the failure in the status", func() bool {
		return h.store.Get().Message == "read the hand-over file failed (Unknown)"
	})
	h.stop(t)
	if out := h.log.String(); strings.Contains(out, "SEED-MARKER") {
		t.Errorf("log quotes the error:\n%s", out)
	}
}

func TestRunShowsTheApplianceHostName(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig"), addrs: [][]netip.Prefix{globalAddr}}
	cluster := newReadyCluster()
	cluster.appliance = existingAppliance()
	h := newHarness(node, cluster)
	h.start(t)

	want := "https://" + existingHostName + " (node 10.0.2.15)\n"
	eventually(t, "the Appliance host name on the console", func() bool { return strings.Contains(h.console.String(), want) })
}
