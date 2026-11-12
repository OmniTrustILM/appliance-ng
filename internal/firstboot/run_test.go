package firstboot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	testVersion   = "v-test"
	testInstaller = "registry.example/installer:v1"
	testHostName  = "ilm.appliance.internal"
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
	markErrs     []error

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
	if len(n.markErrs) > 0 {
		err := n.markErrs[0]
		n.markErrs = n.markErrs[1:]
		return err
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

func (n *fakeNode) snapshot() fakeNode {
	n.mu.Lock()
	defer n.mu.Unlock()
	return fakeNode{
		stageCalls: n.stageCalls, addrCalls: n.addrCalls, bootstrapCalls: n.bootstrapCalls,
		kubeCalls: n.kubeCalls, applied: slices.Clone(n.applied), brokenCalls: n.brokenCalls,
		resets: n.resets, clears: n.clears, recorded: n.recorded, marks: n.marks, marked: n.marked,
	}
}

// fakeCluster is a kube.Cluster where a wait passes once the step that owns it was applied,
// unless the wait is listed in stuck.
type fakeCluster struct {
	mu sync.Mutex

	steps   []kube.Step
	applied []string        // step names in apply order
	created []string        // step names in create-only order
	done    map[string]bool // steps whose objects exist
	stuck   map[string]string
	labels  map[string]map[string]string

	readyCalls map[string]int
}

func newFakeCluster(steps []kube.Step, alreadyApplied ...string) *fakeCluster {
	c := &fakeCluster{steps: steps, done: map[string]bool{}, stuck: map[string]string{},
		labels: map[string]map[string]string{}, readyCalls: map[string]int{}}
	for _, s := range alreadyApplied {
		c.done[s] = true
	}
	return c
}

func (c *fakeCluster) Apply(_ context.Context, docs []byte) error {
	return c.record(&c.applied, docs)
}

func (c *fakeCluster) Create(_ context.Context, docs []byte) error {
	return c.record(&c.created, docs)
}

func (c *fakeCluster) record(into *[]string, docs []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	step := strings.TrimSpace(string(docs))
	step = step[:strings.IndexByte(step, '-')]
	if len(*into) == 0 || (*into)[len(*into)-1] != step {
		*into = append(*into, step)
	}
	c.done[step] = true
	return nil
}

func (c *fakeCluster) LabelNamespace(_ context.Context, ns string, labels map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.labels[ns] = labels
	return nil
}

func (c *fakeCluster) Ready(_ context.Context, w kube.WaitFor) (bool, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readyCalls[w.Name]++
	if reason, ok := c.stuck[w.Name]; ok {
		return false, reason, nil
	}
	if !c.done[w.Name] {
		return false, "not found", nil
	}
	return true, "", nil
}

func (c *fakeCluster) appliedSteps() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.applied)
}

func (c *fakeCluster) createdSteps() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.created)
}

func (c *fakeCluster) calls(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyCalls[name]
}

// testSteps returns steps named after names, each with two files and one wait named after the step.
// File contents start with the step name so the fake cluster knows which step it applies.
func testSteps(names ...string) ([]kube.Step, fs.FS) {
	fsys := fstest.MapFS{}
	var steps []kube.Step
	for _, name := range names {
		files := []string{name + "-1.yaml", name + "-2.yaml"}
		for _, f := range files {
			fsys[f] = &fstest.MapFile{Data: []byte(strings.TrimSuffix(f, ".yaml") + "\n")}
		}
		steps = append(steps, kube.Step{
			Name:            name,
			Files:           files,
			NamespaceLabels: map[string]map[string]string{name: {"pod-security.kubernetes.io/enforce": "privileged"}},
			Wait:            []kube.WaitFor{{Kind: "Deployment", Namespace: name, Name: name, For: "Available"}},
		})
	}
	return steps, fsys
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

type harness struct {
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

func newHarness(node *fakeNode, cluster *fakeCluster, steps []kube.Step, fsys fs.FS) *harness {
	h := &harness{node: node, cluster: cluster, store: NewStatusStore(), log: &syncBuffer{}, console: &syncBuffer{}}
	if fsys == nil {
		fsys = fstest.MapFS{}
	}
	h.deps = Deps{
		Node: node,
		NewCluster: func(kc []byte) (kube.Cluster, error) {
			if !bytes.Equal(kc, node.kubeconfig) {
				return nil, errors.New("unexpected kubeconfig")
			}
			return cluster, nil
		},
		Steps:  steps,
		Addons: fsys,
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
	h := newHarness(node, nil, nil, nil)
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
	want := machineconfig.Input{ClusterName: "ilm", NodeIP: "10.0.2.15", InstallerImage: testInstaller, HostName: testHostName}
	if len(h.inputs) != 1 || h.inputs[0] != want {
		t.Errorf("Generate inputs %+v, want [%+v]", h.inputs, want)
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
	h := newHarness(node, nil, nil, nil)
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
	steps, fsys := testSteps("cilium", "storage", "keycloak", "platform")
	allSteps := []string{"cilium", "storage", "keycloak", "platform"}

	tests := []struct {
		name           string
		node           *fakeNode
		alreadyApplied []string
		wantState      State
		wantBootstrap  int
		wantSteps      []string
	}{
		{
			name:          "running, not bootstrapped",
			node:          &fakeNode{stage: talos.StageRunning},
			wantState:     StateReady,
			wantBootstrap: 1,
			wantSteps:     allSteps,
		},
		{
			name:          "running, bootstrap check fails",
			node:          &fakeNode{stage: talos.StageRunning, bootCheckErr: errors.New("etcd unavailable")},
			wantState:     StateReady,
			wantBootstrap: 1,
			wantSteps:     allSteps,
		},
		{
			name:           "running, bootstrapped, steps half-applied",
			node:           &fakeNode{stage: talos.StageRunning, bootstrapped: true},
			alreadyApplied: []string{"cilium", "storage"},
			wantState:      StateReady,
			wantSteps:      allSteps,
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
			wantSteps:     allSteps,
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
			cluster := newFakeCluster(steps, tt.alreadyApplied...)
			h := newHarness(tt.node, cluster, steps, fsys)
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
			if got := cluster.appliedSteps(); !slices.Equal(got, tt.wantSteps) {
				t.Errorf("applied steps %v, want %v", got, tt.wantSteps)
			}
			if tt.wantSteps == nil && s.kubeCalls != 0 {
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

func TestRunReportsStuckStep(t *testing.T) {
	steps, fsys := testSteps("cilium", "keycloak", "platform")
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newFakeCluster(steps)
	cluster.stuck["keycloak"] = "ImagePullBackOff"
	h := newHarness(node, cluster, steps, fsys)
	h.start(t)

	eventually(t, "5 retries of keycloak", func() bool { return cluster.calls("keycloak") >= 6 })
	if !h.running() {
		t.Fatal("Run returned while a step was stuck")
	}
	st := h.store.Get()
	if st.State != StateAddons || st.Step != "keycloak" {
		t.Errorf("status %q/%q, want %q/keycloak", st.State, st.Step, StateAddons)
	}
	if !strings.Contains(st.Message, "ImagePullBackOff") {
		t.Errorf("message %q does not name the reason", st.Message)
	}
	if cluster.calls("platform") != 0 {
		t.Error("the next step started while keycloak was stuck")
	}
}

func TestRunReportsStuckPlatform(t *testing.T) {
	steps, fsys := testSteps("cilium", "platform")
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newFakeCluster(steps)
	cluster.stuck["platform"] = "Progressing"
	h := newHarness(node, cluster, steps, fsys)
	h.start(t)

	eventually(t, "platform polled", func() bool { return cluster.calls("platform") >= 3 })
	if st := h.store.Get(); st.State != StatePlatform || st.Step != "platform" || !strings.Contains(st.Message, "Progressing") {
		t.Errorf("status %+v, want platform/platform naming the reason", st)
	}
}

func TestRunNeverLogsSecrets(t *testing.T) {
	const secret, kubeSecret = "SECRET-MARKER", "KUBE-MARKER"
	steps, fsys := testSteps("cilium", "platform")
	node := &fakeNode{
		stage:      talos.StageMaintenance,
		stageAfter: talos.StageRunning,
		addrs:      [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("users: [" + kubeSecret + "]"),
		applyErrs:  []error{fmt.Errorf("apply configuration: invalid document %q", secret)},
		kubeErrs:   []error{fmt.Errorf("fetch kubeconfig: truncated %q", kubeSecret)},
	}
	cluster := newFakeCluster(steps)
	h := newHarness(node, cluster, steps, fsys)
	h.deps.Generate = func(machineconfig.Input) ([]byte, error) { return []byte("ca: " + secret), nil }
	newCluster := h.deps.NewCluster
	failed := false
	h.deps.NewCluster = func(kc []byte) (kube.Cluster, error) {
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
	steps, fsys := testSteps("platform")
	node := &fakeNode{
		stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		bootErrs: []error{errors.New("etcd not ready"), errors.New("etcd not ready")},
		addrGate: map[int]chan struct{}{3: gate},
	}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
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
	h := newHarness(&fakeNode{}, nil, nil, nil)
	h.deps.InstallerImage = ""
	if err := Run(context.Background(), h.deps, h.store); err == nil {
		t.Error("Run accepted an empty installer image")
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
	steps, fsys := testSteps("platform")
	node := &fakeNode{
		stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		bootErrs: []error{talos.ErrAlreadyBootstrapped},
	}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	logged := h.log.String()
	if !strings.Contains(logged, `msg="etcd already bootstrapped"`) || strings.Contains(logged, `msg="etcd bootstrapped"`) {
		t.Errorf("log should say etcd was already bootstrapped, and only that:\n%s", logged)
	}
}

func TestRunCreatesCreateOnlyStepsInsteadOfApplying(t *testing.T) {
	steps, fsys := testSteps("cilium", "platform")
	steps[1].CreateOnly = true
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, kubeconfig: []byte("kubeconfig")}
	cluster := newFakeCluster(steps)
	h := newHarness(node, cluster, steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	h.stop(t)
	if got := cluster.appliedSteps(); !slices.Equal(got, []string{"cilium"}) {
		t.Errorf("applied %v, want [cilium]", got)
	}
	if got := cluster.createdSteps(); !slices.Equal(got, []string{"platform"}) {
		t.Errorf("created %v, want [platform]", got)
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
	h := newHarness(node, nil, nil, nil)
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

func TestRunChecksImagesWhileApplyingAddons(t *testing.T) {
	steps, fsys := testSteps("cilium")
	cluster := newFakeCluster(steps)
	cluster.stuck["cilium"] = "pods not ready"
	node := &fakeNode{stage: talos.StageRunning, bootstrapped: true, addrs: [][]netip.Prefix{globalAddr},
		kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, cluster, steps, fsys)
	h.start(t)

	eventually(t, "waiting for cilium", func() bool { return cluster.calls("cilium") > 0 })
	node.setBroken(kubeletImage)
	eventually(t, "reset", func() bool { return node.snapshot().resets == 1 })
}

func TestRunLeavesHealthyImagesAlone(t *testing.T) {
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	s := node.snapshot()
	if s.brokenCalls == 0 || s.resets != 0 {
		t.Fatalf("checks %d, resets %d: want checks only", s.brokenCalls, s.resets)
	}
}

func TestRunIgnoresFailedImageCheck(t *testing.T) {
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		brokenErr: errors.New("machined unavailable")}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
}

func TestRunDoesNotResetTwice(t *testing.T) {
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage}, recorded: true}
	h := newHarness(node, nil, nil, nil)
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
			h := newHarness(node, nil, nil, nil)
			h.start(t)

			eventually(t, "META error in the status", func() bool { return strings.Contains(h.store.Get().Message, "META") })
			time.Sleep(20 * time.Millisecond)
			if s := node.snapshot(); s.resets != 0 || s.bootstrapCalls != 0 {
				t.Fatalf("%d resets, %d bootstrap calls: want neither", s.resets, s.bootstrapCalls)
			}
		})
	}
}

func TestRunReportsAFailedReset(t *testing.T) {
	node := &fakeNode{stage: talos.StageBooting, configured: true, addrs: [][]netip.Prefix{globalAddr},
		broken: []string{etcdImage}, resetErrs: []error{errors.New("reset refused")}}
	h := newHarness(node, nil, nil, nil)
	h.deps.Retry = 10 * time.Millisecond
	h.start(t)

	want := "could not reset node storage: " + codes.Unknown.String()
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

func TestRunDoesNotResetOnceReady(t *testing.T) {
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig")}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
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
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		recorded: true}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	if s := node.snapshot(); s.clears != 1 || s.recorded {
		t.Fatalf("%d clears, recorded %v: want the record cleared once", s.clears, s.recorded)
	}
}

func TestRunReadyDespiteAFailedClear(t *testing.T) {
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		clearErr: errors.New("machined unavailable")}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
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
	h := newHarness(node, nil, nil, nil)
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
			h := newHarness(node, nil, nil, nil)
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
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		recorded: true}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	if s := node.snapshot(); s.marks != 1 || !s.marked || s.recorded {
		t.Fatalf("%d marks, marked %v, recorded %v: want the marker written once and the record cleared",
			s.marks, s.marked, s.recorded)
	}
}

func TestRunRetriesTheReadyMarker(t *testing.T) {
	steps, fsys := testSteps("platform")
	node := &fakeNode{stage: talos.StageRunning, addrs: [][]netip.Prefix{globalAddr}, kubeconfig: []byte("kubeconfig"),
		recorded: true, markErrs: []error{errors.New("META full"), errors.New("META full")}}
	h := newHarness(node, newFakeCluster(steps), steps, fsys)
	h.start(t)

	h.waitState(t, StateReady)
	eventually(t, "marker", func() bool { return node.snapshot().marked })
	if s := node.snapshot(); s.marks != 1 || s.recorded || s.clears != 1 {
		t.Fatalf("%d marks, recorded %v, %d clears: want the marker written and then the record cleared",
			s.marks, s.recorded, s.clears)
	}
	if !strings.Contains(h.log.String(), "META full") {
		t.Errorf("log does not record the failed marker:\n%s", h.log.String())
	}
	if st := h.store.Get().State; st != StateReady {
		t.Errorf("state %s, want ready", st)
	}
}
