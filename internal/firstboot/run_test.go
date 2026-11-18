package firstboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	testVersion   = "v-test"
	testInstaller = "registry.example/installer:v1"
	testHostName  = "ilm.appliance.internal"
	// testILMVersion and testManager stand for the release defaults and the Appliance Manager manifest.
	testILMVersion = "2.20.0"
	testManager    = "kind: Deployment\nmetadata: {name: appliance-manager}\n"
)

var (
	globalAddr = []netip.Prefix{netip.MustParsePrefix("10.0.2.15/24")}
	linkLocal  = []netip.Prefix{netip.MustParsePrefix("fe80::1/64"), netip.MustParsePrefix("169.254.1.2/16")}
)

// fakeNode is a talos.Node whose answers the test scripts.
type fakeNode struct {
	mu sync.Mutex

	stage        talos.Stage
	stageAfter   talos.Stage // stage reported once a configuration was applied
	bootstrapped bool
	bootCheckErr error
	configured   bool
	configErr    error
	addrs        [][]netip.Prefix // answers of successive Addresses calls; the last one repeats
	addrGate     map[int]chan struct{}
	applyErrs    []error
	bootErrs     []error
	kubeconfig   []byte
	kubeErrs     []error
	broken       []string
	brokenErr    error
	recorded     bool // the reset record, which survives the reset
	recordedErr  error
	recordErr    error
	clearErr     error
	resetErrs    []error
	marked       bool // the ready marker, which survives reboots and resets
	markedErr    error
	markFail     error // fails every MarkReady call while set
	network      talos.Network
	networkErr   error
	handover     []byte // the hand-over file in the machine configuration
	handoverErr  error

	stageCalls, addrCalls, bootstrapCalls, kubeCalls, brokenCalls int
	applied                                                       [][]byte
	resets, clears, marks                                         int
}

func (n *fakeNode) WasReady(context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.marked, n.markedErr
}

func (n *fakeNode) MarkReady(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.markFail != nil {
		return n.markFail
	}
	n.marks++
	n.marked = true
	return nil
}

func (n *fakeNode) BrokenSystemImages(context.Context) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.brokenCalls++
	return slices.Clone(n.broken), n.brokenErr
}

func (n *fakeNode) ResetRecorded(context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.recorded, n.recordedErr
}

func (n *fakeNode) RecordReset(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.recordErr != nil {
		return n.recordErr
	}
	n.recorded = true
	return nil
}

func (n *fakeNode) ClearResetRecord(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.clearErr != nil {
		return n.clearErr
	}
	n.clears++
	n.recorded = false
	return nil
}

func (n *fakeNode) ResetEphemeral(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.recorded {
		return errors.New("reset requested before it was recorded")
	}
	if len(n.resetErrs) > 0 {
		err := n.resetErrs[0]
		n.resetErrs = n.resetErrs[1:]
		return err
	}
	n.resets++
	return nil
}

// errNotUsed answers the Node methods that the first-boot service does not call.
var errNotUsed = errors.New("not used by the first-boot service")

func (*fakeNode) PatchConfig(context.Context, []byte) error                   { return errNotUsed }
func (*fakeNode) RemoveExtensionConfig(context.Context, string, string) error { return errNotUsed }

func (n *fakeNode) ExtensionConfigFile(_ context.Context, name, file string) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if name != machineconfig.HandoverService || file != machineconfig.HandoverPath {
		return nil, nil
	}
	return n.handover, n.handoverErr
}
func (*fakeNode) Versions(context.Context) (string, string, error) { return "", "", errNotUsed }
func (*fakeNode) Reboot(context.Context) error                     { return errNotUsed }
func (*fakeNode) Shutdown(context.Context) error                   { return errNotUsed }

func (n *fakeNode) setMarkFail(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.markFail = err
}

func (n *fakeNode) setBroken(refs ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.broken = refs
}

func (n *fakeNode) Stage(context.Context) (talos.Stage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stageCalls++
	if len(n.applied) > 0 && n.stageAfter != "" {
		return n.stageAfter, nil
	}
	return n.stage, nil
}

func (n *fakeNode) ApplyConfig(_ context.Context, cfg []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.applyErrs) > 0 {
		err := n.applyErrs[0]
		n.applyErrs = n.applyErrs[1:]
		return err
	}
	n.applied = append(n.applied, cfg)
	return nil
}

func (n *fakeNode) Bootstrap(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.bootstrapCalls++
	if len(n.bootErrs) > 0 {
		err := n.bootErrs[0]
		n.bootErrs = n.bootErrs[1:]
		return err
	}
	n.bootstrapped = true
	return nil
}

func (n *fakeNode) IsBootstrapped(context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.bootstrapped, n.bootCheckErr
}

func (n *fakeNode) Configured(context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.configured, n.configErr
}

func (n *fakeNode) Kubeconfig(context.Context) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.kubeCalls++
	if len(n.kubeErrs) > 0 {
		err := n.kubeErrs[0]
		n.kubeErrs = n.kubeErrs[1:]
		return nil, err
	}
	return n.kubeconfig, nil
}

func (n *fakeNode) Addresses(context.Context) ([]netip.Prefix, error) {
	n.mu.Lock()
	n.addrCalls++
	call := n.addrCalls
	gate := n.addrGate[call]
	var addrs []netip.Prefix
	if len(n.addrs) > 0 {
		addrs = n.addrs[min(call, len(n.addrs))-1]
	}
	n.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return addrs, nil
}

func (n *fakeNode) Network(context.Context, netip.Addr) (talos.Network, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.network, n.networkErr
}

func (n *fakeNode) snapshot() fakeNode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return fakeNode{
		stageCalls: n.stageCalls, addrCalls: n.addrCalls, bootstrapCalls: n.bootstrapCalls,
		kubeCalls: n.kubeCalls, applied: slices.Clone(n.applied), brokenCalls: n.brokenCalls,
		resets: n.resets, clears: n.clears, recorded: n.recorded, marks: n.marks, marked: n.marked,
	}
}

// fakeCluster is a Cluster that records what was applied and created. Create keeps an existing Appliance as it is,
// like the API server does; the Appliance it returns carries the status the test sets.
type fakeCluster struct {
	mu sync.Mutex

	applied   [][]byte
	created   [][]byte
	appliance *v1alpha1.Appliance // the spec as created or as it existed before
	status    v1alpha1.ApplianceStatus
	secrets   []*corev1.Secret // the Secrets created
	applyErrs []error
	createErr error
	readErr   error
	reads     int
}

// newReadyCluster returns a cluster whose Appliance reports the add-ons and the platform ready.
func newReadyCluster() *fakeCluster {
	c := &fakeCluster{}
	c.setConditions(
		metav1.Condition{Type: v1alpha1.ConditionAddonsReady, Status: metav1.ConditionTrue},
		metav1.Condition{Type: v1alpha1.ConditionPlatformReady, Status: metav1.ConditionTrue})
	return c
}

func (c *fakeCluster) Apply(_ context.Context, docs []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applyErrs) > 0 {
		err := c.applyErrs[0]
		c.applyErrs = c.applyErrs[1:]
		return err
	}
	c.applied = append(c.applied, docs)
	return nil
}

func (c *fakeCluster) Create(_ context.Context, docs []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.createErr != nil {
		return c.createErr
	}
	if s := (&corev1.Secret{}); json.Unmarshal(docs, s) == nil && s.Kind == "Secret" {
		c.secrets = append(c.secrets, s)
		return nil
	}
	c.created = append(c.created, docs)
	if c.appliance != nil {
		return nil
	}
	a := &v1alpha1.Appliance{}
	if err := json.Unmarshal(docs, a); err != nil {
		return err
	}
	c.appliance = a
	return nil
}

func (c *fakeCluster) Appliance(context.Context) (*v1alpha1.Appliance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	switch {
	case c.readErr != nil:
		return nil, c.readErr
	case c.appliance == nil:
		return nil, apierrors.NewNotFound(applianceResource.GroupResource(), v1alpha1.ApplianceName)
	}
	a := c.appliance.DeepCopy()
	c.status.DeepCopyInto(&a.Status)
	return a, nil
}

func (c *fakeCluster) createdSecrets() []*corev1.Secret {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.secrets)
}

func (c *fakeCluster) setConditions(conditions ...metav1.Condition) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Conditions = conditions
}

func (c *fakeCluster) snapshot() (applied, created [][]byte, reads int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.applied), slices.Clone(c.created), c.reads
}

// syncBuffer is a bytes.Buffer safe for concurrent writers and readers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeWizard answers wizard part 1 at once with in, or blocks until ctx is done when block is set.
type fakeWizard struct {
	mu    sync.Mutex
	in    machineconfig.Input
	block bool
	ips   []netip.Addr
}

var wizardLines = []string{"ILM appliance v-test setup", "Open https://10.0.2.15:8443/setup/"}

func (w *fakeWizard) Run(ctx context.Context, ip netip.Addr, show func([]string)) (machineconfig.Input, error) {
	w.mu.Lock()
	w.ips = append(w.ips, ip)
	block := w.block
	w.mu.Unlock()
	show(wizardLines)
	if block {
		<-ctx.Done()
		return machineconfig.Input{}, ctx.Err()
	}
	return w.in, nil
}

// wizardAnswers is what the fake wizard answers: a host name and a name server, keeping DHCP.
func wizardAnswers() machineconfig.Input {
	return machineconfig.Input{HostName: "ilm.example.com", DNS: []netip.Addr{netip.MustParseAddr("10.0.2.3")}, Handover: []byte(`{"version":1}`)}
}

type harness struct {
	wizard  *fakeWizard
	node    *fakeNode
	cluster *fakeCluster
	deps    Deps
	store   *StatusStore
	log     *syncBuffer
	console *syncBuffer
	inputs  []machineconfig.Input
	inMu    sync.Mutex
	cancel  context.CancelFunc
	done    chan error
}

// newHarness runs the service against node and cluster; a nil cluster has no Appliance yet.
func newHarness(node *fakeNode, cluster *fakeCluster) *harness {
	if cluster == nil {
		cluster = &fakeCluster{}
	}
	h := &harness{node: node, cluster: cluster, store: NewStatusStore(), log: &syncBuffer{}, console: &syncBuffer{},
		wizard: &fakeWizard{in: wizardAnswers()}}
	h.deps = Deps{
		Node:   node,
		Wizard: h.wizard,
		NewCluster: func(kc []byte) (Cluster, error) {
			if !bytes.Equal(kc, node.kubeconfig) {
				return nil, errors.New("unexpected kubeconfig")
			}
			return cluster, nil
		},
		Manager: []byte(testManager),
		Generate: func(in machineconfig.Input) ([]byte, error) {
			h.inMu.Lock()
			defer h.inMu.Unlock()
			h.inputs = append(h.inputs, in)
			return []byte("generated-config"), nil
		},
		Console:         h.console,
		Log:             slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Retry:           time.Millisecond,
		InstallerImage:  testInstaller,
		HostName:        testHostName,
		ILMVersion:      testILMVersion,
		ClusterName:     "ilm",
		Version:         testVersion,
		ConsoleInterval: 5 * time.Millisecond,
	}
	return h
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- Run(ctx, h.deps, h.store) }()
	t.Cleanup(func() { h.stop(t) })
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run returned %v after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func (h *harness) running() bool {
	select {
	case err := <-h.done:
		h.done <- err
		return false
	default:
		return true
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) waitState(t *testing.T, want State) {
	t.Helper()
	eventually(t, "state "+string(want), func() bool { return h.store.Get().State == want })
}

func TestRunConfiguresFromMaintenance(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "apply and later passes", func() bool {
		s := node.snapshot()
		return len(s.applied) == 1 && s.stageCalls >= 5
	})
	h.stop(t)

	s := node.snapshot()
	if len(s.applied) != 1 || string(s.applied[0]) != "generated-config" {
		t.Fatalf("applied %q, want the generated configuration once", s.applied)
	}
	want := wizardAnswers()
	want.ClusterName, want.NodeIP, want.InstallerImage = "ilm", "10.0.2.15", testInstaller
	if len(h.inputs) != 1 || !reflect.DeepEqual(h.inputs[0], want) {
		t.Errorf("Generate inputs %+v, want [%+v]", h.inputs, want)
	}
	if ips := h.wizard.ips; len(ips) != 1 || ips[0] != netip.MustParseAddr("10.0.2.15") {
		t.Errorf("wizard ran for %v, want once for 10.0.2.15", ips)
	}
	if got := h.store.Get().State; got != StateInstalling {
		t.Errorf("state %q, want %q", got, StateInstalling)
	}
}

func TestRunWaitsForAddress(t *testing.T) {
	gate := make(chan struct{})
	node := &fakeNode{
		stage:    talos.StageMaintenance,
		addrs:    [][]netip.Prefix{linkLocal, linkLocal, globalAddr},
		addrGate: map[int]chan struct{}{3: gate},
	}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "third Addresses call", func() bool { return node.snapshot().addrCalls == 3 })
	if s := node.snapshot(); len(s.applied) != 0 {
		t.Fatalf("applied a configuration before a global address: %q", s.applied)
	}
	if got := h.store.Get().State; got != StateWaitingForAddress {
		t.Fatalf("state %q, want %q", got, StateWaitingForAddress)
	}
	if strings.Contains(h.console.String(), "169.254") || strings.Contains(h.console.String(), "fe80") {
		t.Errorf("console shows a link-local address: %q", h.console.String())
	}
	close(gate)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	if h.inputs[0].NodeIP != "10.0.2.15" {
		t.Errorf("NodeIP %q, want 10.0.2.15", h.inputs[0].NodeIP)
	}
}

func TestRunResumesFromEveryStage(t *testing.T) {
	tests := []struct {
		name          string
		node          *fakeNode
		handedOver    bool // the Appliance exists from an earlier run
		wantState     State
		wantBootstrap int
		wantHandover  bool
	}{
		{
			name:          "running, not bootstrapped",
			node:          &fakeNode{stage: talos.StageRunning},
			wantState:     StateReady,
			wantBootstrap: 1,
			wantHandover:  true,
		},
		{
			name:          "running, bootstrap check fails",
			node:          &fakeNode{stage: talos.StageRunning, bootCheckErr: errors.New("etcd unavailable")},
			wantState:     StateReady,
			wantBootstrap: 1,
			wantHandover:  true,
		},
		{
			name:         "running, bootstrapped, handed over before",
			node:         &fakeNode{stage: talos.StageRunning, bootstrapped: true},
			handedOver:   true,
			wantState:    StateReady,
			wantHandover: true,
		},
		{
			name:      "maintenance, already configured",
			node:      &fakeNode{stage: talos.StageMaintenance, configured: true},
			wantState: StateInstalling,
		},
		{
			name: "maintenance, configuration check fails",
			node: &fakeNode{stage: talos.StageMaintenance, configErr: errors.New("machined unavailable")},
		},
		{
			name:      "installing",
			node:      &fakeNode{stage: talos.StageInstalling},
			wantState: StateInstalling,
		},
		{
			name:      "booting, not configured",
			node:      &fakeNode{stage: talos.StageBooting},
			wantState: StateInstalling,
		},
		{
			name:          "booting, configured, not bootstrapped",
			node:          &fakeNode{stage: talos.StageBooting, configured: true},
			wantState:     StateReady,
			wantBootstrap: 1,
			wantHandover:  true,
		},
		{
			name:      "booting, configuration check fails",
			node:      &fakeNode{stage: talos.StageBooting, configErr: errors.New("machined unavailable")},
			wantState: StateInstalling,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.node.addrs = [][]netip.Prefix{globalAddr}
			tt.node.kubeconfig = []byte("kubeconfig")
			cluster := newReadyCluster()
			if tt.handedOver {
				cluster.appliance = existingAppliance()
			}
			h := newHarness(tt.node, cluster)
			h.start(t)

			if tt.wantState == StateReady {
				h.waitState(t, StateReady)
			} else {
				eventually(t, "several passes", func() bool { return tt.node.snapshot().stageCalls >= 5 })
			}
			h.stop(t)

			s := tt.node.snapshot()
			if len(s.applied) != 0 {
				t.Errorf("ApplyConfig called %d times, want 0", len(s.applied))
			}
			if s.bootstrapCalls != tt.wantBootstrap {
				t.Errorf("Bootstrap called %d times, want %d", s.bootstrapCalls, tt.wantBootstrap)
			}
			applied, created, _ := cluster.snapshot()
			if handedOver := len(applied) == 1 && len(created) == 1; handedOver != tt.wantHandover {
				t.Errorf("applied %d and created %d times, want the hand-over %v", len(applied), len(created), tt.wantHandover)
			}
			if tt.handedOver && cluster.appliance.Spec.Node.HostName != existingHostName {
				t.Errorf("the existing Appliance was changed: %+v", cluster.appliance.Spec)
			}
			if !tt.wantHandover && s.kubeCalls != 0 {
				t.Errorf("Kubeconfig called %d times, want 0", s.kubeCalls)
			}
			if got := h.store.Get().State; got != tt.wantState {
				t.Errorf("state %q, want %q", got, tt.wantState)
			}
			if tt.node.configErr != nil && !strings.Contains(h.store.Get().Message, "machined unavailable") {
				t.Errorf("message %q does not name the failed check", h.store.Get().Message)
			}
			if len(h.inputs) != 0 {
				t.Errorf("Generate called %d times, want 0", len(h.inputs))
			}
		})
	}
}

const existingHostName = "pki.example.com"

// existingAppliance is an Appliance the user configured before the service (re)started.
func existingAppliance() *v1alpha1.Appliance {
	return &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec:       v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: existingHostName, Address: "192.0.2.10/24"}},
	}
}

func TestRunHandsOverAfterBootstrap(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{}
	h := newHarness(node, cluster)
	h.start(t)

	h.waitState(t, StateAddons)
	eventually(t, "the status read again", func() bool { _, _, reads := cluster.snapshot(); return reads >= 3 })
	h.stop(t)

	applied, created, _ := cluster.snapshot()
	if len(applied) != 1 || string(applied[0]) != testManager {
		t.Errorf("applied %q, want the manager manifest once", applied)
	}
	if len(created) != 1 {
		t.Fatalf("created %d times, want the Appliance once", len(created))
	}
	var doc map[string]any
	if err := json.Unmarshal(created[0], &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["status"]; ok {
		t.Errorf("the created Appliance carries a status: %s", created[0])
	}
	a := cluster.appliance
	want := v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: testHostName},
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
		Platform: &v1alpha1.PlatformSpec{ILMVersion: testILMVersion, HostName: testHostName},
	}
	if a.APIVersion != v1alpha1.GroupVersion.String() || a.Kind != "Appliance" || a.Name != v1alpha1.ApplianceName {
		t.Errorf("created %s %s %q, want the Appliance named %q", a.APIVersion, a.Kind, a.Name, v1alpha1.ApplianceName)
	}
	if !equality.Semantic.DeepEqual(a.Spec, want) {
		t.Errorf("spec %+v, want %+v", a.Spec, want)
	}
	if st := h.store.Get(); st.Step != "" || st.Message != "waiting for the Appliance Manager" {
		t.Errorf("status %+v, want addons waiting for the manager", st)
	}
	if !strings.Contains(h.log.String(), "state=handover") {
		t.Errorf("log does not record the hand-over:\n%s", h.log.String())
	}
}

func TestRunDoesNotOverwriteExistingAppliance(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newReadyCluster()
	cluster.appliance = existingAppliance()
	h := newHarness(node, cluster)
	h.start(t)

	h.waitState(t, StateReady)
	h.stop(t)
	if got := cluster.appliance; !equality.Semantic.DeepEqual(got, existingAppliance()) {
		t.Errorf("Appliance changed to %+v", got)
	}
	if applied, _, _ := cluster.snapshot(); len(applied) != 1 || string(applied[0]) != testManager {
		t.Errorf("applied %q, want only the manager manifest", applied)
	}
}

func TestRunRetriesFailedHandover(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newReadyCluster()
	cluster.applyErrs = []error{errors.New("kind Appliance is not served")}
	h := newHarness(node, cluster)
	h.deps.Retry = 20 * time.Millisecond
	h.start(t)

	eventually(t, "the error in the status", func() bool {
		st := h.store.Get()
		return st.State == StateHandover && strings.Contains(st.Message, "kind Appliance is not served")
	})
	h.waitState(t, StateReady)
	if applied, created, _ := cluster.snapshot(); len(applied) != 1 || len(created) != 1 {
		t.Errorf("applied %d and created %d times, want once each after the retry", len(applied), len(created))
	}
}

func TestRunMirrorsApplianceStatus(t *testing.T) {
	cond := func(kind string, status metav1.ConditionStatus, message string) metav1.Condition {
		return metav1.Condition{Type: kind, Status: status, Message: message}
	}
	addonsProgressing := cond(v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, "cilium: waiting for DaemonSet kube-system/cilium")
	addonsReady := cond(v1alpha1.ConditionAddonsReady, metav1.ConditionTrue, "all add-ons are ready")
	platformProgressing := cond(v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, "the Platform reports no Available condition yet")
	platformReady := cond(v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, "the Platform is available")

	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := &fakeCluster{}
	h := newHarness(node, cluster)
	h.start(t)

	steps := []struct {
		conditions  []metav1.Condition
		wantState   State
		wantMessage string
	}{
		{nil, StateAddons, "waiting for the Appliance Manager"},
		{[]metav1.Condition{cond(v1alpha1.ConditionAddonsReady, metav1.ConditionFalse, "")}, StateAddons, "waiting for the Appliance Manager"},
		{[]metav1.Condition{addonsProgressing}, StateAddons, addonsProgressing.Message},
		{[]metav1.Condition{addonsReady}, StatePlatform, "waiting for the ILM Platform"},
		{[]metav1.Condition{addonsReady, platformProgressing}, StatePlatform, platformProgressing.Message},
		{[]metav1.Condition{addonsReady, platformReady}, StateReady, "ILM Platform is ready"},
		{[]metav1.Condition{addonsReady, platformProgressing}, StatePlatform, platformProgressing.Message},
		{[]metav1.Condition{addonsReady, platformReady}, StateReady, "ILM Platform is ready"},
	}
	for _, step := range steps {
		cluster.setConditions(step.conditions...)
		eventually(t, fmt.Sprintf("%s: %s", step.wantState, step.wantMessage), func() bool {
			st := h.store.Get()
			return st.State == step.wantState && st.Step == "" && st.Message == step.wantMessage
		})
	}
	if s := node.snapshot(); s.marks != 1 || !s.marked {
		t.Errorf("%d marks, marked %v: want the ready marker written once", s.marks, s.marked)
	}
}

func TestRunReportsAFailedApplianceRead(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newReadyCluster()
	cluster.readErr = errors.New("connection refused")
	h := newHarness(node, cluster)
	h.start(t)

	eventually(t, "the error in the status", func() bool { return h.store.Get().Message == "connection refused" })
	if st := h.store.Get(); st.State != StateHandover {
		t.Errorf("state %q, want %q until the Appliance is read", st.State, StateHandover)
	}
	cluster.mu.Lock()
	cluster.readErr = nil
	cluster.mu.Unlock()
	h.waitState(t, StateReady)
}

func TestRunAsksAgainWhenTheNodeRefusesTheConfiguration(t *testing.T) {
	node := &fakeNode{
		stage: talos.StageMaintenance, stageAfter: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("kubeconfig"),
		applyErrs: []error{
			status.Error(codes.Unavailable, "connection reset"),
			status.Error(codes.InvalidArgument, "invalid configuration"),
		},
	}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	h.wizard.mu.Lock()
	defer h.wizard.mu.Unlock()
	// A lost answer retries the same answers; a refused configuration asks the administrator again.
	if len(h.wizard.ips) != 2 {
		t.Errorf("wizard ran %d times, want 2", len(h.wizard.ips))
	}
}

func TestRunNeverLogsSecrets(t *testing.T) {
	const secret, kubeSecret = "SECRET-MARKER", "KUBE-MARKER"
	node := &fakeNode{
		stage:      talos.StageMaintenance,
		stageAfter: talos.StageRunning,
		addrs:      [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("users: [" + kubeSecret + "]"),
		applyErrs:  []error{fmt.Errorf("apply configuration: invalid document %q", secret)},
		kubeErrs:   []error{fmt.Errorf("fetch kubeconfig: truncated %q", kubeSecret)},
	}
	h := newHarness(node, newReadyCluster())
	generated := false
	h.deps.Generate = func(machineconfig.Input) ([]byte, error) {
		if !generated {
			generated = true
			return nil, fmt.Errorf("generate: cannot encode the hand-over %q", secret)
		}
		return []byte("ca: " + secret), nil
	}
	newCluster := h.deps.NewCluster
	failed := false
	h.deps.NewCluster = func(kc []byte) (Cluster, error) {
		if !failed {
			failed = true
			return nil, fmt.Errorf("load kubeconfig: cannot parse %q", kc)
		}
		return newCluster(kc)
	}
	h.start(t)

	h.waitState(t, StateReady)
	wantLine := "ILM appliance v-test: https://ilm.appliance.internal (node 10.0.2.15), state ready\n"
	eventually(t, "console line in state ready", func() bool { return strings.Contains(h.console.String(), wantLine) })
	h.stop(t)

	for name, out := range map[string]string{"log": h.log.String(), "console": h.console.String(), "status": fmt.Sprint(h.store.Get())} {
		for _, marker := range []string{secret, kubeSecret} {
			if strings.Contains(out, marker) {
				t.Errorf("%s contains %s:\n%s", name, marker, out)
			}
		}
	}
	if !strings.Contains(h.log.String(), "state=ready") {
		t.Errorf("log does not record the state changes:\n%s", h.log.String())
	}
}

func TestRunRetriesFailedBootstrap(t *testing.T) {
	gate := make(chan struct{})
	node := &fakeNode{
		stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		bootErrs: []error{errors.New("etcd not ready"), errors.New("etcd not ready")},
		addrGate: map[int]chan struct{}{3: gate},
	}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	eventually(t, "two failed bootstraps", func() bool { return node.snapshot().addrCalls == 3 })
	if st := h.store.Get(); st.State != StateBootstrapping || !strings.Contains(st.Message, "etcd not ready") {
		t.Errorf("status %+v, want bootstrapping with the error", st)
	}
	close(gate)
	h.waitState(t, StateReady)
	if got := node.snapshot().bootstrapCalls; got != 3 {
		t.Errorf("Bootstrap called %d times, want 3", got)
	}
}

func TestRunRejectsIncompleteDeps(t *testing.T) {
	for name, clear := range map[string]func(*Deps){
		"installer image": func(d *Deps) { d.InstallerImage = "" },
		"manager":         func(d *Deps) { d.Manager = nil },
		"ILM version":     func(d *Deps) { d.ILMVersion = "" },
		"cluster":         func(d *Deps) { d.NewCluster = nil },
	} {
		h := newHarness(&fakeNode{}, nil)
		clear(&h.deps)
		if err := Run(context.Background(), h.deps, h.store); err == nil {
			t.Errorf("Run accepted Deps without the %s", name)
		}
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 10 * time.Second},
		{3, 40 * time.Second},
		{4, time.Minute},
		{100, time.Minute},
	}
	for _, tt := range tests {
		if got := backoff(5*time.Second, tt.failures); got != tt.want {
			t.Errorf("backoff(5s, %d) = %v, want %v", tt.failures, got, tt.want)
		}
	}
}

func TestRunLogsAlreadyBootstrappedOnce(t *testing.T) {
	node := &fakeNode{
		stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		bootErrs: []error{talos.ErrAlreadyBootstrapped},
	}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	logged := h.log.String()
	if !strings.Contains(logged, `msg="etcd already bootstrapped"`) || strings.Contains(logged, `msg="etcd bootstrapped"`) {
		t.Errorf("log should say etcd was already bootstrapped, and only that:\n%s", logged)
	}
}

const (
	etcdImage    = "registry.k8s.io/etcd:v3.7.3"
	kubeletImage = "ghcr.io/siderolabs/kubelet:v1.37.1"
	resetMessage = "resetting node storage to recover broken system images"
)

func TestRunResetsTheNodeForBrokenSystemImages(t *testing.T) {
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage, kubeletImage}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "reset", func() bool { return node.snapshot().resets == 1 })
	checks := node.snapshot().brokenCalls
	time.Sleep(20 * time.Millisecond)

	s := node.snapshot()
	if !s.recorded || s.resets != 1 {
		t.Fatalf("recorded %v with %d resets, want the record and one reset", s.recorded, s.resets)
	}
	if s.bootstrapCalls != 0 || s.brokenCalls != checks {
		t.Errorf("bootstrap calls %d, checks %d -> %d: want the node left alone while it resets", s.bootstrapCalls, checks, s.brokenCalls)
	}
	if !h.running() {
		t.Fatal("Run returned while the node resets")
	}
	if msg := h.store.Get().Message; msg != resetMessage {
		t.Errorf("message %q, want %q", msg, resetMessage)
	}
	if !strings.Contains(h.log.String(), resetMessage) {
		t.Errorf("log does not record the reset:\n%s", h.log.String())
	}
}

func TestRunChecksImagesWhileMirroring(t *testing.T) {
	cluster := &fakeCluster{}
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, addrs: [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, cluster)
	h.start(t)

	h.waitState(t, StateAddons)
	node.setBroken(kubeletImage)
	eventually(t, "reset", func() bool { return node.snapshot().resets == 1 })
}

func TestRunLeavesHealthyImagesAlone(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	s := node.snapshot()
	if s.brokenCalls == 0 || s.resets != 0 {
		t.Fatalf("checks %d, resets %d: want checks only", s.brokenCalls, s.resets)
	}
}

func TestRunIgnoresFailedImageCheck(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		brokenErr: errors.New("machined unavailable")}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
}

func TestRunDoesNotResetTwice(t *testing.T) {
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage}, recorded: true}
	h := newHarness(node, nil)
	h.start(t)

	want := "system image " + etcdImage + " fails to execute after a reset"
	eventually(t, "message", func() bool { return h.store.Get().Message == want })
	time.Sleep(20 * time.Millisecond)
	s := node.snapshot()
	if s.resets != 0 || s.bootstrapCalls != 0 || !h.running() {
		t.Fatalf("%d resets, %d bootstrap calls: want neither and the service up", s.resets, s.bootstrapCalls)
	}
	if s.brokenCalls < 2 {
		t.Errorf("checks %d, want the service to keep checking", s.brokenCalls)
	}
}

func TestRunDoesNotResetWithoutTheRecord(t *testing.T) {
	tests := map[string]*fakeNode{
		"read fails":  {recordedErr: errors.New("META unreadable")},
		"write fails": {recordErr: errors.New("META write failed")},
	}
	for name, node := range tests {
		t.Run(name, func(t *testing.T) {
			node.stage, node.configured, node.addrs = talos.StageBooting, true, [][]netip.Prefix{globalAddr}
			node.broken = []string{etcdImage}
			h := newHarness(node, nil)
			h.start(t)

			eventually(t, "META error in the status", func() bool { return strings.Contains(h.store.Get().Message, "META") })
			time.Sleep(20 * time.Millisecond)
			if s := node.snapshot(); s.resets != 0 || s.bootstrapCalls != 0 {
				t.Fatalf("%d resets, %d bootstrap calls: want neither", s.resets, s.bootstrapCalls)
			}
		})
	}
}

func TestRunReportsARejectedReset(t *testing.T) {
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage}, resetErrs: []error{status.Error(codes.FailedPrecondition, "reset refused")}}
	h := newHarness(node, nil)
	h.deps.Retry = 10 * time.Millisecond
	h.start(t)

	want := "could not reset node storage: " + codes.FailedPrecondition.String()
	eventually(t, "reset error in the status", func() bool { return h.store.Get().Message == want })
	eventually(t, "reset on the next pass", func() bool { return node.snapshot().resets == 1 })
	if s := node.snapshot(); !s.recorded || s.clears != 1 || !h.running() {
		t.Fatalf("recorded %v, %d clears: want the record cleared after the failure, written again and the service up",
			s.recorded, s.clears)
	}
	if !strings.Contains(h.log.String(), "reset refused") {
		t.Errorf("log does not record the reset error:\n%s", h.log.String())
	}
}

func TestRunTreatsAnUnconfirmedResetAsRequested(t *testing.T) {
	old := resetTimeout
	resetTimeout = 30 * time.Millisecond
	t.Cleanup(func() { resetTimeout = old })
	for _, code := range []codes.Code{codes.DeadlineExceeded, codes.Unavailable, codes.Canceled, codes.Unknown} {
		t.Run(code.String(), func(t *testing.T) {
			node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
				broken: []string{etcdImage}, resetErrs: []error{status.Error(code, "connection lost")}}
			h := newHarness(node, nil)
			h.start(t)

			want := "requested node storage reset did not happen, system image " + etcdImage + " fails to execute"
			eventually(t, "message", func() bool { return h.store.Get().Message == want })
			time.Sleep(20 * time.Millisecond)
			if s := node.snapshot(); s.resets != 0 || !s.recorded || s.clears != 0 || s.bootstrapCalls != 0 {
				t.Fatalf("%d resets, recorded %v, %d clears, %d bootstrap calls: want the record kept and no second reset",
					s.resets, s.recorded, s.clears, s.bootstrapCalls)
			}
			if !strings.Contains(h.log.String(), "connection lost") {
				t.Errorf("log does not record the reset error:\n%s", h.log.String())
			}
		})
	}
}

func TestRunDoesNotResetOnceReady(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	checks := node.snapshot().brokenCalls
	node.setBroken(etcdImage)
	time.Sleep(50 * time.Millisecond)
	if s := node.snapshot(); s.resets != 0 || s.brokenCalls != checks || s.recorded {
		t.Fatalf("%d resets, checks %d -> %d, recorded %v: want nothing after ready", s.resets, checks, s.brokenCalls, s.recorded)
	}
}

func TestRunClearsTheResetRecordWhenReady(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		recorded: true}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	if s := node.snapshot(); s.clears != 1 || s.recorded {
		t.Fatalf("%d clears, recorded %v: want the record cleared once", s.clears, s.recorded)
	}
}

func TestRunReadyDespiteAFailedClear(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		clearErr: errors.New("machined unavailable")}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	eventually(t, "warning", func() bool { return strings.Contains(h.log.String(), "machined unavailable") })
}

func TestRunChecksAgainWhenTheResetDoesNotHappen(t *testing.T) {
	old := resetTimeout
	resetTimeout = 30 * time.Millisecond
	t.Cleanup(func() { resetTimeout = old })
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "reset", func() bool { return node.snapshot().resets == 1 })
	want := "requested node storage reset did not happen, system image " + etcdImage + " fails to execute"
	eventually(t, "message", func() bool { return h.store.Get().Message == want })
	if s := node.snapshot(); s.resets != 1 {
		t.Fatalf("%d resets, want one", s.resets)
	}
	if !strings.Contains(h.log.String(), "reset did not happen") {
		t.Errorf("log does not record the missing reset:\n%s", h.log.String())
	}
}

func TestRunNeverResetsANodeThatWasReady(t *testing.T) {
	tests := map[string]*fakeNode{
		"marker set":        {marked: true},
		"marker unreadable": {markedErr: errors.New("META unreadable")},
	}
	for name, node := range tests {
		t.Run(name, func(t *testing.T) {
			node.stage, node.configured, node.addrs = talos.StageBooting, true, [][]netip.Prefix{globalAddr}
			node.broken = []string{etcdImage}
			h := newHarness(node, nil)
			h.start(t)

			eventually(t, "the normal path", func() bool { return node.snapshot().bootstrapCalls > 0 })
			time.Sleep(20 * time.Millisecond)
			if s := node.snapshot(); s.resets != 0 || s.recorded {
				t.Fatalf("%d resets, recorded %v: want neither", s.resets, s.recorded)
			}
			if got := strings.Count(h.log.String(), "not resetting node storage"); got != 1 {
				t.Errorf("%d warnings, want one:\n%s", got, h.log.String())
			}
		})
	}
}

func TestRunMarksTheNodeReady(t *testing.T) {
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		recorded: true}
	h := newHarness(node, newReadyCluster())
	h.start(t)

	h.waitState(t, StateReady)
	if s := node.snapshot(); s.marks != 1 || !s.marked || s.recorded {
		t.Fatalf("%d marks, marked %v, recorded %v: want the marker written once and the record cleared",
			s.marks, s.marked, s.recorded)
	}
}

func TestRunIsNotReadyUntilTheMarkerIsWritten(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprintf("reset recorded %v", recorded), func(t *testing.T) {
			node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr},
				kubeconfig: []byte("kubeconfig"), recorded: recorded, markFail: errors.New("META full")}
			h := newHarness(node, newReadyCluster())
			h.start(t)

			want := "recording the ready marker: META full"
			eventually(t, "marker error in the status", func() bool { return h.store.Get().Message == want })
			node.setBroken(etcdImage)
			time.Sleep(20 * time.Millisecond)
			if st := h.store.Get(); st.State != StatePlatform || st.Step != "" {
				t.Fatalf("state %s step %q, want the platform step while the marker is missing", st.State, st.Step)
			}
			if s := node.snapshot(); s.resets != 0 || s.recorded != recorded || s.clears != 0 {
				t.Fatalf("%d resets, recorded %v, %d clears: want no reset and the record untouched",
					s.resets, s.recorded, s.clears)
			}

			node.setMarkFail(nil)
			h.waitState(t, StateReady)
			if s := node.snapshot(); s.marks != 1 || s.recorded || s.resets != 0 {
				t.Fatalf("%d marks, recorded %v, %d resets: want the marker written, no record and no reset",
					s.marks, s.recorded, s.resets)
			}
			if !strings.Contains(h.log.String(), "META full") {
				t.Errorf("log does not record the failed marker:\n%s", h.log.String())
			}
		})
	}
}
