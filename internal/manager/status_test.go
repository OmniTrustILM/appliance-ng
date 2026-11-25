package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

func wantVersions(t *testing.T, want v1alpha1.Versions) {
	t.Helper()
	if got := getAppliance(t).Status.Versions; got != want {
		t.Errorf("versions = %+v, want %+v", got, want)
	}
}

func TestStatusVersions(t *testing.T) {
	cleanCertificates(t)
	spec := importedSpec()
	spec.Platform = platformSpec()
	createAppliance(t, spec)
	r, _ := newReconciler(testClient, &fakeCluster{})
	r.Version = "1.2.3"
	runReconcile(t, r)
	wantVersions(t, v1alpha1.Versions{Appliance: "1.2.3", Operator: "v1.1.0"})

	p := getPlatform(t)
	p.Status.ObservedVersion = "2.20.0"
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	wantVersions(t, v1alpha1.Versions{Appliance: "1.2.3", Operator: "v1.1.0", ILM: "2.20.0"})

	createNode(t, testBootID)
	node := &fakeNodeConfig{docs: firstBootDocuments(t, importedSpec()), talosVersion: "v1.14.5", kubernetesVersion: "v1.37.1"}
	mustReconcileNode(t, newNodeReconciler(node))
	all := v1alpha1.Versions{Appliance: "1.2.3", Operator: "v1.1.0", ILM: "2.20.0", Talos: "v1.14.5", Kubernetes: "v1.37.1"}
	wantVersions(t, all)
	if got := getAppliance(t).Status.NodeAddress; got != "192.0.2.10" {
		t.Errorf("node address %q, want 192.0.2.10", got)
	}

	// Talos unreachable or failing: the last known versions stay and no error text reaches the status.
	unreachable := &NodeReconciler{Client: testClient, Recorder: events.NewFakeRecorder(10), Talos: func(context.Context) (talos.Node, error) { return nil, errors.New("dial tcp: refused") }}
	mustReconcileNode(t, unreachable)
	wantVersions(t, all)
	node.versionsErr = errors.New("rpc error: deadline exceeded")
	mustReconcileNode(t, newNodeReconciler(node))
	wantVersions(t, all)

	// A Platform that reports no version yet keeps the last one, and the Appliance reconciler keeps the node's.
	p = getPlatform(t)
	p.Status.ObservedVersion = ""
	if err := testClient.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	wantVersions(t, all)
	for _, c := range getAppliance(t).Status.Conditions {
		if strings.Contains(c.Message, "refused") || strings.Contains(c.Message, "deadline") {
			t.Errorf("condition %s carries error text: %q", c.Type, c.Message)
		}
	}
}

// setConnectorPhase sets the phase the operator reports for the connector name, for its current generation.
func setConnectorPhase(t *testing.T, name string, phase otilm.ConnectorPhase) {
	t.Helper()
	c := &otilm.Connector{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: name}, c); err != nil {
		t.Fatal(err)
	}
	c.Status.Phase, c.Status.ObservedGeneration = phase, c.Generation
	c.Status.Conditions = []metav1.Condition{{Type: "Degraded", Status: metav1.ConditionTrue, Reason: "Test", Message: "secret password rejected", LastTransitionTime: metav1.Now()}}
	if err := testClient.Status().Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

// setConnectorRegistration sets the registration the operator reports for the connector name.
func setConnectorRegistration(t *testing.T, name string, status otilm.RegistrationStatusValue) {
	t.Helper()
	c := &otilm.Connector{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: name}, c); err != nil {
		t.Fatal(err)
	}
	c.Status.Registration = &otilm.RegistrationStatus{Status: status}
	if err := testClient.Status().Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func wantConnectorsReady(t *testing.T, status metav1.ConditionStatus, reason string, names ...string) {
	t.Helper()
	c := condition(t, getAppliance(t), v1alpha1.ConditionConnectorsReady)
	if c.Status != status || c.Reason != reason {
		t.Errorf("ConnectorsReady = %s/%s (%q), want %s/%s", c.Status, c.Reason, c.Message, status, reason)
	}
	for _, name := range names {
		if !strings.Contains(c.Message, name) {
			t.Errorf("ConnectorsReady message %q does not name %s", c.Message, name)
		}
	}
	if strings.Contains(c.Message, "password") {
		t.Errorf("ConnectorsReady message %q carries the operator's message", c.Message)
	}
}

func TestConnectorsReady(t *testing.T) {
	const crypto, compliance = "software-cryptography-provider", "x509-compliance-provider"
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec(crypto, compliance)})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonProgressing, crypto, compliance)

	setConnectorPhase(t, crypto, otilm.ConnectorPhaseRunning)
	setConnectorRegistration(t, crypto, otilm.RegistrationStatusConnected)
	setConnectorPhase(t, compliance, otilm.ConnectorPhaseFailed)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonFailed, compliance)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionConnectorsReady); strings.Contains(c.Message, crypto) {
		t.Errorf("ConnectorsReady message %q names the running connector", c.Message)
	}

	setConnectorPhase(t, compliance, otilm.ConnectorPhaseRunning)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonProgressing, "waiting for ILM to register "+compliance)

	editSpec(t, func(s *v1alpha1.ApplianceSpec) { s.Platform = platformSpec() })
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionTrue, reasonReady)
}

func TestConnectorsReadyV1WaitsForILM(t *testing.T) {
	const compliance = "x509-compliance-provider"
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec(compliance)})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	c := &otilm.Connector{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: compliance}, c); err != nil {
		t.Fatal(err)
	}
	if c.Spec.Registration != nil {
		t.Fatalf("v1 connector has registration %+v", c.Spec.Registration)
	}

	setConnectorPhase(t, compliance, otilm.ConnectorPhaseRunning)
	res := runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonProgressing, "waiting for ILM to register "+compliance)
	if res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v while ILM has not registered the connector", res.RequeueAfter, addonsRequeue)
	}
	if got := getAppliance(t).Status.Connectors; len(got) != 1 || got[0].State != connectorStateWaiting {
		t.Errorf("connector states = %+v, want the v1 connector waiting", got)
	}
}

func TestConnectorsReadyV2NeedsRegistration(t *testing.T) {
	const crypto = "software-cryptography-provider"
	createAppliance(t, v1alpha1.ApplianceSpec{Platform: platformSpec(crypto)})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	setConnectorPhase(t, crypto, otilm.ConnectorPhaseRunning)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonProgressing, crypto)

	setConnectorRegistration(t, crypto, otilm.RegistrationStatusOffline)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonFailed, crypto, "ILM cannot reach the connector")
	if got := getAppliance(t).Status.Connectors; len(got) != 1 || got[0] != (v1alpha1.ConnectorState{Name: crypto, State: "failed", Problem: "ILM cannot reach the connector"}) {
		t.Errorf("status connectors = %+v", got)
	}

	setConnectorRegistration(t, crypto, otilm.RegistrationStatusWaitingForApproval)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionTrue, reasonReady)

	setConnectorRegistration(t, crypto, otilm.RegistrationStatusFailed)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionFalse, reasonFailed, crypto)

	setConnectorRegistration(t, crypto, otilm.RegistrationStatusConnected)
	runReconcile(t, r)
	wantConnectorsReady(t, metav1.ConditionTrue, reasonReady)
}

func TestConnectorReady(t *testing.T) {
	running := otilm.Connector{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: otilm.ConnectorStatus{Phase: otilm.ConnectorPhaseRunning, ObservedGeneration: 1}}
	if connectorReady(&running) {
		t.Error("a connector whose generation the operator has not observed is ready")
	}
	running.Status.ObservedGeneration = 2
	registered := running.DeepCopy()
	registered.Spec.Registration = &otilm.RegistrationSpec{}
	if connectorReady(registered) {
		t.Error("a running connector that is not registered yet is ready")
	}
	if !connectorReady(&running) {
		t.Error("a running connector of the observed generation is not ready")
	}
}

func TestStatusDataVolume(t *testing.T) {
	cleanCertificates(t)
	spec := importedSpec()
	spec.Platform = platformSpec()
	createAppliance(t, spec)
	r, _ := newReconciler(testClient, &fakeCluster{})

	r.DataDir = t.TempDir()
	runReconcile(t, r)
	volume := getAppliance(t).Status.DataVolume
	if volume == nil || volume.SizeBytes <= 0 || volume.FreeBytes < 0 || volume.FreeBytes > volume.SizeBytes {
		t.Errorf("data volume = %+v, want its size and free space", volume)
	}

	// A mount that cannot be read leaves the volume unset and does not fail the reconcile.
	r.DataDir = r.DataDir + "/missing"
	runReconcile(t, r)
	if volume := getAppliance(t).Status.DataVolume; volume != nil {
		t.Errorf("data volume = %+v, want unset", volume)
	}
}

func TestVolumeStatusReportsCoarseSteps(t *testing.T) {
	const size = 100 << 30
	step := uint64(size / 100)
	base := 40*step + 5
	got := volumeStatus(size, base)
	if got.SizeBytes != size || uint64(got.FreeBytes) != 40*step {
		t.Fatalf("volume = %+v, want %d free", got, 40*step)
	}
	// Free space moving within one step leaves the status as it was.
	if again := volumeStatus(size, base+step/2-5); *again != *got {
		t.Errorf("volume = %+v, want %+v", again, got)
	}
	if again := volumeStatus(size, 40*step); *again != *got {
		t.Errorf("volume = %+v, want %+v", again, got)
	}
	// Crossing a step changes it.
	if next := volumeStatus(size, base+step); uint64(next.FreeBytes) != 41*step {
		t.Errorf("volume = %+v, want %d free", next, 41*step)
	}
	if lower := volumeStatus(size, 40*step-1); uint64(lower.FreeBytes) != 39*step {
		t.Errorf("volume = %+v, want %d free", lower, 39*step)
	}
}

func TestVolumeStatusOfSmallAndOddVolumes(t *testing.T) {
	// A small volume steps by 1 MiB, and free space never exceeds the size.
	if got := volumeStatus(10<<20, 5<<20+100); got.FreeBytes != 5<<20 {
		t.Errorf("volume = %+v, want 5 MiB free", got)
	}
	if got := volumeStatus(10<<20, 20<<20); got.FreeBytes > got.SizeBytes {
		t.Errorf("volume = %+v, want free within the size", got)
	}
}

func TestVolumeStatusJudgesLowSpaceOnTheExactFreeSpace(t *testing.T) {
	// A size that is no multiple of 100, so that the rounded free space of 20.5% falls below a fifth.
	const size = 100<<30 + 99
	for _, tc := range []struct {
		name string
		free uint64
		low  bool
	}{
		{"20.5% free", size * 205 / 1000, false},
		{"just below a fifth free", size / 5, true},
		{"19.9% free", size * 199 / 1000, true},
	} {
		got := volumeStatus(size, tc.free)
		if got.LowSpace != tc.low {
			t.Errorf("%s: low space = %v, want %v (%+v)", tc.name, got.LowSpace, tc.low, got)
		}
	}
	if got := volumeStatus(size, size*205/1000); got.FreeBytes*5 >= got.SizeBytes {
		t.Errorf("the rounded free space %d is not below a fifth of %d; the test no longer covers the rounding", got.FreeBytes, got.SizeBytes)
	}
}
