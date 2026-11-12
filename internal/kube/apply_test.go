package kube_test

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/OmniTrustILM/appliance-ng/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

var (
	restConfig *rest.Config
	kubeconfig []byte
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		os.Exit(1)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		println("start envtest:", err.Error())
		os.Exit(1)
	}
	restConfig = cfg
	kubeconfig, err = clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"c": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"u": {
			ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"x": {Cluster: "c", AuthInfo: "u"}},
		CurrentContext: "x",
	})
	if err != nil {
		println("write kubeconfig:", err.Error())
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func newCluster(t *testing.T) kube.Cluster {
	t.Helper()
	c, err := kube.NewCluster(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

const widgetCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.test
spec:
  group: example.test
  names: {kind: Widget, listKind: WidgetList, plural: widgets, singular: widget}
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
`

const widgetAndNamespace = widgetCRD + `---
# an empty document follows
---
apiVersion: v1
kind: Namespace
metadata:
  name: widgets
---
apiVersion: example.test/v1
kind: Widget
metadata:
  name: one
  namespace: widgets
spec:
  size: 3
`

func TestApplyCreatesNamespaceThenCR(t *testing.T) {
	c := newCluster(t)
	if err := c.Apply(ctx(t), []byte(widgetAndNamespace)); err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(restConfig)
	gvr := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}
	if _, err := dyn.Resource(gvr).Namespace("widgets").Get(ctx(t), "one", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, _, err := c.Ready(ctx(t), kube.WaitFor{Kind: "CustomResourceDefinition", Name: "widgets.example.test", For: "Established"})
	if err != nil || !ready {
		t.Fatalf("CRD ready = %v, %v", ready, err)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	c := newCluster(t)
	manifest := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: idem
  namespace: default
data:
  key: value
`)
	cms := kubernetes.NewForConfigOrDie(restConfig).CoreV1().ConfigMaps("default")
	if err := c.Apply(ctx(t), manifest); err != nil {
		t.Fatal(err)
	}
	first, err := cms.Get(ctx(t), "idem", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(ctx(t), manifest); err != nil {
		t.Fatal(err)
	}
	second, err := cms.Get(ctx(t), "idem", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion changed: %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestApplyRejectsBadInput(t *testing.T) {
	c := newCluster(t)
	if err := c.Apply(ctx(t), []byte("kind: [unterminated")); err == nil {
		t.Error("malformed YAML: want error")
	}
	if err := c.Apply(ctx(t), []byte("apiVersion: v1\nmetadata:\n  name: x\n")); err == nil {
		t.Error("document without kind: want error")
	}
}

func TestApplyUnknownKindFailsAfterWait(t *testing.T) {
	c := newCluster(t)
	short, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Apply(short, []byte("apiVersion: nothing.test/v1\nkind: Ghost\nmetadata:\n  name: x\n"))
	if err == nil {
		t.Fatal("want error for a kind that never appears")
	}
}

func TestLabelNamespaceCreatesAndMerges(t *testing.T) {
	c := newCluster(t)
	if err := c.LabelNamespace(ctx(t), "labelled", map[string]string{"a": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.LabelNamespace(ctx(t), "labelled", map[string]string{"b": "2"}); err != nil {
		t.Fatal(err)
	}
	ns, err := kubernetes.NewForConfigOrDie(restConfig).CoreV1().Namespaces().Get(ctx(t), "labelled", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Labels["a"] != "1" || ns.Labels["b"] != "2" {
		t.Errorf("labels = %v", ns.Labels)
	}
}

func int32Ptr(i int32) *int32 { return &i }

func createDeployment(t *testing.T, name string) {
	t.Helper()
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.test/c:1"}}},
			},
		},
	}
	if _, err := kubernetes.NewForConfigOrDie(restConfig).AppsV1().Deployments("default").Create(ctx(t), d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestReadyDeploymentAvailable(t *testing.T) {
	c := newCluster(t)
	createDeployment(t, "web")
	w := kube.WaitFor{Kind: "Deployment", Namespace: "default", Name: "web", For: "Available"}

	ready, _, err := c.Ready(ctx(t), w)
	if err != nil || ready {
		t.Fatalf("before status: ready = %v, err = %v", ready, err)
	}

	deployments := kubernetes.NewForConfigOrDie(restConfig).AppsV1().Deployments("default")
	d, err := deployments.Get(ctx(t), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
	if _, err := deployments.UpdateStatus(ctx(t), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, _, err = c.Ready(ctx(t), w)
	if err != nil || !ready {
		t.Fatalf("after status: ready = %v, err = %v", ready, err)
	}
}

func TestReadyMissingObjectIsNotReady(t *testing.T) {
	c := newCluster(t)
	ready, reason, err := c.Ready(ctx(t), kube.WaitFor{Kind: "Deployment", Namespace: "default", Name: "absent", For: "Available"})
	if err != nil || ready || reason == "" {
		t.Fatalf("ready = %v, reason = %q, err = %v", ready, reason, err)
	}
}

func TestReadyDaemonSetRollout(t *testing.T) {
	c := newCluster(t)
	labels := map[string]string{"app": "agent"}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.test/c:1"}}},
			},
		},
	}
	daemonSets := kubernetes.NewForConfigOrDie(restConfig).AppsV1().DaemonSets("default")
	created, err := daemonSets.Create(ctx(t), ds, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := kube.WaitFor{Kind: "DaemonSet", Namespace: "default", Name: "agent", For: "Rollout"}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || ready {
		t.Fatalf("before status: ready = %v, err = %v", ready, err)
	}

	created.Status = appsv1.DaemonSetStatus{
		ObservedGeneration: created.Generation, DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2,
	}
	if created, err = daemonSets.UpdateStatus(ctx(t), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || !ready {
		t.Fatalf("rolled out: ready = %v, err = %v", ready, err)
	}

	created.Status.NumberAvailable = 1
	if _, err := daemonSets.UpdateStatus(ctx(t), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || ready {
		t.Fatalf("partially available: ready = %v, err = %v", ready, err)
	}
}

func TestReadyNode(t *testing.T) {
	c := newCluster(t)
	w := kube.WaitFor{Kind: "Node", For: "Ready"}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || ready {
		t.Fatalf("no nodes: ready = %v, err = %v", ready, err)
	}

	nodes := kubernetes.NewForConfigOrDie(restConfig).CoreV1().Nodes()
	node, err := nodes.Create(ctx(t), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || ready {
		t.Fatalf("node without condition: ready = %v, err = %v", ready, err)
	}

	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if _, err := nodes.UpdateStatus(ctx(t), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ready, _, err := c.Ready(ctx(t), w); err != nil || !ready {
		t.Fatalf("all ready: ready = %v, err = %v", ready, err)
	}
	named := kube.WaitFor{Kind: "Node", Name: "n1", For: "Ready"}
	if ready, _, err := c.Ready(ctx(t), named); err != nil || !ready {
		t.Fatalf("named node: ready = %v, err = %v", ready, err)
	}
}

func TestReadyRejectsUnknownKindAndCondition(t *testing.T) {
	c := newCluster(t)
	for _, w := range []kube.WaitFor{
		{Kind: "StatefulSet", Namespace: "default", Name: "x", For: "Ready"},
		{Kind: "Deployment", Namespace: "default", Name: "x", For: "Ready"},
	} {
		if _, _, err := c.Ready(ctx(t), w); err == nil {
			t.Errorf("%+v: want error", w)
		}
	}
}

const platformCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: platforms.otilm.com
spec:
  group: otilm.com
  names: {kind: Platform, listKind: PlatformList, plural: platforms, singular: platform}
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    subresources: {status: {}}
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
`

func TestReadyPlatformReportsCondition(t *testing.T) {
	c := newCluster(t)
	manifest := platformCRD + `---
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: ilm
  namespace: default
spec: {}
`
	if err := c.Apply(ctx(t), []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	w := kube.WaitFor{Kind: "Platform", Namespace: "default", Name: "ilm", For: "Available"}
	ready, reason, err := c.Ready(ctx(t), w)
	if err != nil || ready {
		t.Fatalf("no status: ready = %v, err = %v", ready, err)
	}
	if reason == "" {
		t.Error("want a reason while no condition is reported")
	}

	setAvailable := func(status, reason string, observedGeneration int64) {
		t.Helper()
		dyn := dynamic.NewForConfigOrDie(restConfig)
		gvr := schema.GroupVersionResource{Group: "otilm.com", Version: "v1alpha1", Resource: "platforms"}
		obj, err := dyn.Resource(gvr).Namespace("default").Get(ctx(t), "ilm", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cond := map[string]any{"type": "Available", "status": status, "reason": reason, "observedGeneration": observedGeneration}
		if err := unstructured.SetNestedSlice(obj.Object, []any{cond}, "status", "conditions"); err != nil {
			t.Fatal(err)
		}
		if _, err := dyn.Resource(gvr).Namespace("default").UpdateStatus(ctx(t), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	setAvailable("False", "WaitingForDatabase", 1)
	ready, reason, err = c.Ready(ctx(t), w)
	if err != nil || ready || reason != "WaitingForDatabase" {
		t.Fatalf("ready = %v, reason = %q, err = %v", ready, reason, err)
	}

	setAvailable("True", "Reconciled", 0)
	ready, reason, err = c.Ready(ctx(t), w)
	if err != nil || ready || reason != "generation not observed" {
		t.Fatalf("stale generation: ready = %v, reason = %q, err = %v", ready, reason, err)
	}

	setAvailable("True", "Reconciled", 1)
	if ready, _, err = c.Ready(ctx(t), w); err != nil || !ready {
		t.Fatalf("ready = %v, err = %v", ready, err)
	}
}

func TestLoadStepsFromEmbeddedAddons(t *testing.T) {
	steps, err := kube.LoadSteps(addons.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 9 {
		t.Fatalf("got %d steps, want 9", len(steps))
	}
	if last := steps[len(steps)-1]; last.Name != "platform" {
		t.Errorf("last step = %q, want platform", last.Name)
	}
	if got := steps[1].NamespaceLabels["local-path-storage"]["pod-security.kubernetes.io/enforce"]; got != "privileged" {
		t.Errorf("storage namespace label = %q", got)
	}
}

func TestCreateAppliesMissingObject(t *testing.T) {
	c := newCluster(t)
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: created\n  namespace: default\ndata:\n  key: value\n")
	if err := c.Create(ctx(t), manifest); err != nil {
		t.Fatal(err)
	}
	cm, err := kubernetes.NewForConfigOrDie(restConfig).CoreV1().ConfigMaps("default").Get(ctx(t), "created", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["key"] != "value" {
		t.Errorf("data = %v", cm.Data)
	}
}

func TestCreateLeavesExistingObjectUntouched(t *testing.T) {
	c := newCluster(t)
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kept\n  namespace: default\ndata:\n  key: default\n")
	if err := c.Create(ctx(t), manifest); err != nil {
		t.Fatal(err)
	}
	cms := kubernetes.NewForConfigOrDie(restConfig).CoreV1().ConfigMaps("default")
	cm, err := cms.Get(ctx(t), "kept", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data["key"] = "changed"
	if _, err := cms.Update(ctx(t), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx(t), manifest); err != nil {
		t.Fatal(err)
	}
	cm, err = cms.Get(ctx(t), "kept", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["key"] != "changed" {
		t.Errorf("key = %q, want the changed value", cm.Data["key"])
	}
}

func TestCreateRejectsBadInput(t *testing.T) {
	c := newCluster(t)
	if err := c.Create(ctx(t), []byte("apiVersion: v1\nmetadata:\n  name: x\n")); err == nil {
		t.Error("document without kind: want error")
	}
}
