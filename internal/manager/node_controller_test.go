package manager

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	testBootID = "boot-1"
	testLink   = "enp0s1"
)

// fakeNodeConfig is a node whose running machine configuration holds the documents docs.
type fakeNodeConfig struct {
	talos.Node
	mu         sync.Mutex
	docs       []configdoc.Document
	replaced   [][]string // IDs of the replaced documents, per call
	removed    [][]string
	replaceErr error
	readErr    error

	talosVersion, kubernetesVersion string
	versionsErr                     error
}

func (f *fakeNodeConfig) Versions(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.talosVersion, f.kubernetesVersion, f.versionsErr
}

func (f *fakeNodeConfig) Documents(_ context.Context, kinds ...string) ([]configdoc.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	var out []configdoc.Document
	for _, doc := range f.docs {
		if slices.Contains(kinds, doc.Kind()) {
			out = append(out, doc.Clone())
		}
	}
	return out, nil
}

func (f *fakeNodeConfig) ReplaceDocuments(_ context.Context, docs []configdoc.Document, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced, f.removed = append(f.replaced, documentIDs(docs)), append(f.removed, remove)
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.docs = slices.DeleteFunc(f.docs, func(d configdoc.Document) bool {
		id := talos.DocumentID(d)
		return slices.Contains(remove, id) || slices.ContainsFunc(docs, func(n configdoc.Document) bool { return talos.DocumentID(n) == id })
	})
	f.docs = append(f.docs, docs...)
	return nil
}

func (f *fakeNodeConfig) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replaced)
}

func (f *fakeNodeConfig) lastCall() (replaced, removed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaced[len(f.replaced)-1], f.removed[len(f.removed)-1]
}

func (f *fakeNodeConfig) document(t *testing.T, id string) configdoc.Document {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, doc := range f.docs {
		if talos.DocumentID(doc) == id {
			return doc
		}
	}
	t.Fatalf("no running document %s", id)
	return nil
}

// firstBootDocuments are the node documents first boot generates for spec, with its static address on testLink.
func firstBootDocuments(t *testing.T, spec v1alpha1.ApplianceSpec) []configdoc.Document {
	t.Helper()
	link := nodeconfig.LinkConfig(testLink, netip.MustParsePrefix(spec.FirstNode().Address), netip.MustParseAddr(spec.FirstNode().Gateway))
	docs, err := nodeconfig.Render(&v1alpha1.Appliance{Spec: spec}, nodeconfig.Node{IP: "192.0.2.10", Link: link}, nodeconfig.Secrets{})
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// createNode creates the Kubernetes node with the boot ID bootID and the internal address of importedSpec.
func createNode(t *testing.T, bootID string) *corev1.Node {
	t.Helper()
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "talos-node"}}
	if err := testClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, node) })
	node.Status.NodeInfo.BootID = bootID
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}
	if err := testClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	return node
}

func setBootID(t *testing.T, node *corev1.Node, bootID string) {
	t.Helper()
	node.Status.NodeInfo.BootID = bootID
	if err := testClient.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
}

// editSpec changes the spec of the Appliance.
func editSpec(t *testing.T, edit func(*v1alpha1.ApplianceSpec)) {
	t.Helper()
	a := getAppliance(t)
	edit(&a.Spec)
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func newNodeReconciler(node talos.Node) *NodeReconciler {
	return &NodeReconciler{Client: testClient, Recorder: events.NewFakeRecorder(10), Talos: func(context.Context) (talos.Node, error) { return node, nil }}
}

func reconcileNode(t *testing.T, r *NodeReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
}

func mustReconcileNode(t *testing.T, r *NodeReconciler) {
	t.Helper()
	if _, err := reconcileNode(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// wantNodeConfigured checks the NodeConfigured condition of the current generation and the required reboot.
func wantNodeConfigured(t *testing.T, status metav1.ConditionStatus, reason string, rebootRequired bool) {
	t.Helper()
	a := getAppliance(t)
	c := condition(t, a, v1alpha1.ConditionNodeConfigured)
	if c.Status != status || c.Reason != reason || c.ObservedGeneration != a.Generation {
		t.Errorf("NodeConfigured %s/%s for generation %d, want %s/%s for %d", c.Status, c.Reason, c.ObservedGeneration, status, reason, a.Generation)
	}
	if a.Status.RebootRequired != rebootRequired {
		t.Errorf("rebootRequired %v, want %v", a.Status.RebootRequired, rebootRequired)
	}
}

func TestNodeReconcilerPatchesOnlyChanged(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	r := newNodeReconciler(node)

	mustReconcileNode(t, r)
	if node.calls() != 0 {
		t.Fatalf("the node runs the settings of the Appliance, yet %v was replaced", node.replaced)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) {
		s.DNS.Servers = []string{"198.51.100.53"}
		s.NTP.Servers = []string{"ntp2.example.com"}
	})
	mustReconcileNode(t, r)
	replaced, removed := node.lastCall()
	if !slices.Equal(replaced, []string{network.ResolverKind, network.TimeSyncKind}) || len(removed) != 0 {
		t.Fatalf("replaced %v and removed %v, want only the name and time servers", replaced, removed)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.FirstNode().Gateway = "192.0.2.254" })
	mustReconcileNode(t, r)
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{network.LinkKind + "/" + testLink}) {
		t.Fatalf("replaced %v, want only the link", replaced)
	}
	link := node.document(t, network.LinkKind+"/"+testLink).(*network.LinkConfigV1Alpha1)
	if link.LinkAddresses[0].AddressAddress.String() != "192.0.2.10/24" || link.LinkRoutes[0].RouteGateway.String() != "192.0.2.254" {
		t.Errorf("link addresses %v routes %v, want the address kept and the new gateway", link.LinkAddresses, link.LinkRoutes)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)

	mustReconcileNode(t, r)
	if node.calls() != 2 {
		t.Errorf("%d changes, want none once the node runs the settings", node.calls())
	}
}

// setRunning replaces or, with doc nil, removes the running document id, as a change past the manager would.
func (f *fakeNodeConfig) setRunning(id string, doc configdoc.Document) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = slices.DeleteFunc(f.docs, func(d configdoc.Document) bool { return talos.DocumentID(d) == id })
	if doc != nil {
		f.docs = append(f.docs, doc)
	}
}

// nextEvent returns the next event of r, or "" when there is none.
func nextEvent(r *NodeReconciler) string {
	select {
	case e := <-r.Recorder.(*events.FakeRecorder).Events:
		return e
	default:
		return ""
	}
}

func TestNodeReconcilerRecordsDriftCorrections(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	r := newNodeReconciler(node)
	mustReconcileNode(t, r)

	// A changed spec is applied, not corrected.
	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.DNS.Servers = []string{"198.51.100.53"} })
	mustReconcileNode(t, r)
	if e := nextEvent(r); e != "" {
		t.Errorf("event %q for a change of the spec", e)
	}

	// Documents changed or removed on the node past the manager are put back.
	node.setRunning(network.ResolverKind, nodeconfig.ResolverConfig([]netip.Addr{netip.MustParseAddr("203.0.113.53")}))
	node.setRunning(network.TimeSyncKind, nil)
	mustReconcileNode(t, r)
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{network.ResolverKind, network.TimeSyncKind}) {
		t.Fatalf("replaced %v, want the name and time servers back", replaced)
	}
	if e := nextEvent(r); e != "Normal DriftCorrected restored the node's ResolverConfig, TimeSyncConfig" {
		t.Errorf("event %q, want DriftCorrected naming the document kinds", e)
	}
	mustReconcileNode(t, r)
	if e := nextEvent(r); e != "" {
		t.Errorf("event %q once the node runs the settings", e)
	}
}

func TestProxyChangeRequiresReboot(t *testing.T) {
	createAppliance(t, importedSpec())
	k8sNode := createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	r := newNodeReconciler(node)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Proxy.HTTP = "http://proxy2.example.com:3128" })
	mustReconcileNode(t, r)
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{runtime.EnvironmentConfigKind}) {
		t.Fatalf("replaced %v, want the environment", replaced)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, true)
	if got := getAppliance(t).Annotations[RebootBootIDAnnotation]; got != testBootID {
		t.Errorf("recorded boot ID %q, want %q", got, testBootID)
	}

	// Still the same boot: the reboot stays required.
	mustReconcileNode(t, r)
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, true)

	setBootID(t, k8sNode, "boot-2")
	mustReconcileNode(t, r)
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, false)
	if _, ok := getAppliance(t).Annotations[RebootBootIDAnnotation]; ok {
		t.Error("the boot ID annotation is kept after the reboot")
	}

	// Removing the proxy removes the environment, which needs a reboot too.
	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Proxy = nil })
	mustReconcileNode(t, r)
	if _, removed := node.lastCall(); !slices.Equal(removed, []string{runtime.EnvironmentConfigKind}) {
		t.Fatalf("removed %v, want the environment", removed)
	}
	wantNodeConfigured(t, metav1.ConditionTrue, reasonApplied, true)
}

func TestNodeReconcilerWaits(t *testing.T) {
	createAppliance(t, importedSpec())
	r := &NodeReconciler{Client: testClient, Recorder: events.NewFakeRecorder(10), Talos: func(context.Context) (talos.Node, error) { return nil, ErrNoTalosAccess }}

	res, err := reconcileNode(t, r)
	if err != nil || res.RequeueAfter != talosAccessRequeue {
		t.Fatalf("reconcile without a node = %+v, %v; want a requeue", res, err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonWaitingForNode, false)

	createNode(t, testBootID)
	res, err = reconcileNode(t, r)
	if err != nil || res.RequeueAfter != talosAccessRequeue {
		t.Fatalf("reconcile without Talos access = %+v, %v; want a requeue", res, err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonWaitingForTalosAccess, false)
}

func TestNodeReconcilerReportsFailuresWithFixedMessages(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	cause := errors.New("rpc error: secret detail")
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec()), replaceErr: cause}
	r := newNodeReconciler(node)
	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.DNS.Servers = []string{"192.0.2.54"} })

	if _, err := reconcileNode(t, r); !errors.Is(err, cause) {
		t.Fatalf("reconcile = %v, want the failure returned for a retry", err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonFailed, false)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionNodeConfigured); strings.Contains(c.Message, "secret") {
		t.Errorf("message %q quotes the error", c.Message)
	}

	node.replaceErr, node.readErr = nil, cause
	if _, err := reconcileNode(t, r); !errors.Is(err, cause) {
		t.Fatalf("reconcile = %v, want the read failure returned", err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonFailed, false)
}

func TestFailedProxyChangeStillRequiresReboot(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	cause := errors.New("connection reset")
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec()), replaceErr: cause}
	r := newNodeReconciler(node)
	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Proxy.HTTP = "http://proxy2.example.com:3128" })

	// The apply may have reached the node before it failed, so the reboot is recorded first and stays.
	if _, err := reconcileNode(t, r); !errors.Is(err, cause) {
		t.Fatalf("reconcile = %v, want the failure returned for a retry", err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonFailed, true)
	if got := getAppliance(t).Annotations[RebootBootIDAnnotation]; got != testBootID {
		t.Errorf("recorded boot ID %q, want %q", got, testBootID)
	}
}

func TestNodeReconcilerRefusesSettingsThatDoNotFit(t *testing.T) {
	createAppliance(t, importedSpec())
	createNode(t, testBootID)
	// The node runs with DHCP, so the gateway of the spec cannot be set.
	docs := slices.DeleteFunc(firstBootDocuments(t, importedSpec()), func(d configdoc.Document) bool { return d.Kind() == network.LinkKind })
	node := &fakeNodeConfig{docs: docs}
	res, err := reconcileNode(t, newNodeReconciler(node))
	if err != nil || res.RequeueAfter != resync {
		t.Fatalf("reconcile = %+v, %v; want no retry before the Appliance changes", res, err)
	}
	wantNodeConfigured(t, metav1.ConditionFalse, reasonInvalid, false)
	if node.calls() != 0 {
		t.Error("settings that do not fit were applied")
	}
}

func TestNodeReconcilerIgnoresAMissingAppliance(t *testing.T) {
	if _, err := reconcileNode(t, newNodeReconciler(&fakeNodeConfig{})); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestNodeIPPrefersTheStaticAddress(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "198.51.100.7"}}}}
	a := &v1alpha1.Appliance{Spec: importedSpec()}
	if got := nodeIP(a, node); got != "192.0.2.10" {
		t.Errorf("nodeIP = %q, want the static address", got)
	}
	a.Spec.FirstNode().Address = ""
	if got := nodeIP(a, node); got != "198.51.100.7" {
		t.Errorf("nodeIP = %q, want the internal address with DHCP", got)
	}
	if got := nodeIP(a, &corev1.Node{}); got != "" {
		t.Errorf("nodeIP = %q, want none", got)
	}
}

// unencodable is a document that fails to encode.
type unencodable struct{}

func (unencodable) Clone() configdoc.Document { return unencodable{} }
func (unencodable) Kind() string              { return "Unencodable" }
func (unencodable) APIVersion() string        { return "v1alpha1" }
func (unencodable) MarshalYAML() (any, error) { return nil, errors.New("no yaml") }

func TestChangesFailsOnAnUnencodableDocument(t *testing.T) {
	good := []configdoc.Document{nodeconfig.HostnameConfig("ilm.example.com")}
	for name, pair := range map[string][2][]configdoc.Document{
		"running":  {{unencodable{}}, good},
		"rendered": {good, {unencodable{}}},
	} {
		if _, _, err := changes(pair[0], pair[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNodeChangedPredicate(t *testing.T) {
	node := func(bootID, ip string) *corev1.Node {
		return &corev1.Node{Status: corev1.NodeStatus{
			NodeInfo:  corev1.NodeSystemInfo{BootID: bootID},
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		}}
	}
	for name, tc := range map[string]struct {
		old, new client.Object
		want     bool
	}{
		"unchanged":       {node("a", "192.0.2.10"), node("a", "192.0.2.10"), false},
		"boot ID changed": {node("a", "192.0.2.10"), node("b", "192.0.2.10"), true},
		"address changed": {node("a", "192.0.2.10"), node("a", "192.0.2.11"), true},
		"not a node":      {&corev1.Secret{}, node("a", "192.0.2.10"), true},
	} {
		if got := (nodeChanged{}).Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
			t.Errorf("%s: Update = %v, want %v", name, got, tc.want)
		}
	}
}

// startNodeManager runs a manager with the node reconciler on node until the test ends.
func startNodeManager(t *testing.T, node talos.Node) {
	t.Helper()
	skipNameValidation := true
	mgr, err := ctrl.NewManager(testConfig, ctrl.Options{
		Scheme:     testClient.Scheme(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Cache:      CacheOptions(),
		Controller: config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &NodeReconciler{Client: mgr.GetClient(), Recorder: mgr.GetEventRecorder(fieldManager), Talos: func(context.Context) (talos.Node, error) { return node, nil }}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
}

// waitForCalls waits until the node had n changes.
func waitForCalls(t *testing.T, node *fakeNodeConfig, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for node.calls() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the manager made %d changes to the node, want %d", node.calls(), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestNodeSetupWithManagerConfiguresTheNode(t *testing.T) {
	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	startNodeManager(t, node)

	spec := importedSpec()
	spec.DNS.Servers = []string{"198.51.100.53"}
	createAppliance(t, spec)
	waitForCalls(t, node, 1)
	if replaced, _ := node.lastCall(); !slices.Equal(replaced, []string{network.ResolverKind}) {
		t.Errorf("replaced %v, want the name servers", replaced)
	}
}

func TestNodeSetupWithManagerFollowsTheSecrets(t *testing.T) {
	createNode(t, testBootID)
	writeSystemSecret(t, testRegistryRef, map[string][]byte{RegistryUsernameKey: []byte("robot"), RegistryPasswordKey: []byte("first")})
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec())}
	startNodeManager(t, node)

	spec := importedSpec()
	spec.Registries = []v1alpha1.RegistrySpec{{Host: testRegistry, CredentialsRef: testRegistryRef}}
	createAppliance(t, spec)
	waitForCalls(t, node, 1)

	// A new password changes the Secret only, not the spec.
	writeSystemSecret(t, testRegistryRef, map[string][]byte{RegistryUsernameKey: []byte("robot"), RegistryPasswordKey: []byte("second")})
	waitForCalls(t, node, 2)
	if got := node.document(t, cri.RegistryAuthConfig+"/"+testRegistry).(*cri.RegistryAuthConfigV1Alpha1).RegistryPassword; got != "second" {
		t.Errorf("registry password %q, want the new one", got)
	}
}
