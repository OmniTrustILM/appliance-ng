package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/peer"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// member is one manager of the election test.
type member struct {
	node      string
	client    client.Client
	cancel    context.CancelFunc
	done      chan error
	leading   atomic.Bool
	everyNode atomic.Bool
}

const reconciledBy = "test.appliance.otilm.com/reconciled-by"

// startMember starts a manager on node with the election options, a ConfigMap reconciler in namespace that marks
// what it reconciles with node, a leader-only and an every-node runnable.
func startMember(t *testing.T, node, namespace string) *member {
	t.Helper()
	lease, err := NewLease(testConfig, node, func() { t.Errorf("%s lost its lease", node) })
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(lease.Fence(testConfig), ElectionOptions(ctrl.Options{
		Scheme:                 testClient.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	}, lease))
	if err != nil {
		t.Fatal(err)
	}
	m := &member{node: node, client: mgr.GetClient(), done: make(chan error, 1)}
	inNamespace := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == namespace })
	err = ctrl.NewControllerManagedBy(mgr).Named("leader-test-"+node).
		For(&corev1.ConfigMap{}, builder.WithPredicates(inNamespace, predicate.Funcs{UpdateFunc: func(event.UpdateEvent) bool { return false }})).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			cm := &corev1.ConfigMap{}
			if err := m.client.Get(ctx, req.NamespacedName, cm); err != nil {
				return reconcile.Result{}, client.IgnoreNotFound(err)
			}
			patch := client.MergeFromWithOptions(cm.DeepCopy(), client.MergeFromWithOptimisticLock{})
			metav1.SetMetaDataAnnotation(&cm.ObjectMeta, reconciledBy, node)
			return reconcile.Result{}, m.client.Patch(ctx, cm, patch)
		}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		flag   *atomic.Bool
		leader bool
	}{{&m.leading, true}, {&m.everyNode, false}} {
		run := func(ctx context.Context) error {
			r.flag.Store(true)
			<-ctx.Done()
			r.flag.Store(false)
			return nil
		}
		add := OnEveryNode(run)
		if r.leader {
			add = OnLeader(run)
		}
		if err := mgr.Add(add); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() { m.done <- mgr.Start(ctx) }()
	t.Cleanup(m.stop)
	return m
}

func (m *member) stop() {
	m.cancel()
	<-m.done
	m.done <- nil
}

func TestOnlyLeaderRunsReconcilers(t *testing.T) {
	ctx := context.Background()
	opts := ElectionOptions(ctrl.Options{}, &Lease{})
	if !opts.LeaderElection || opts.LeaderElectionID != "appliance-manager" || opts.LeaderElectionNamespace != SystemNamespace ||
		*opts.LeaseDuration != 15*time.Second || *opts.RenewDeadline != 10*time.Second || *opts.RetryPeriod != 2*time.Second ||
		!opts.LeaderElectionReleaseOnCancel || opts.LeaderElectionResourceLockInterface == nil {
		t.Fatalf("election options %+v", opts)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "leader-"}}
	if err := testClient.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}

	a := startMember(t, "node-a", ns.Name)
	waitFor(t, "node-a to lead", a.leading.Load)
	b := startMember(t, "node-b", ns.Name)
	waitFor(t, "node-b to run", b.everyNode.Load)
	if leader, err := peer.Leader(ctx, testClient); err != nil || leader != "node-a" {
		t.Fatalf("leader %q, %v", leader, err)
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "work"}}
	if err := testClient.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	marked := func(node string) func() bool {
		return func() bool {
			got := &corev1.ConfigMap{}
			return testClient.Get(ctx, client.ObjectKeyFromObject(cm), got) == nil && got.Annotations[reconciledBy] == node
		}
	}
	waitFor(t, "node-a to reconcile", marked("node-a"))
	// A follower runs no leader-only work and its writes are refused.
	time.Sleep(2 * RetryPeriod)
	if b.leading.Load() || !marked("node-a")() {
		t.Fatal("node-b leads or reconciled as well")
	}
	if err := b.client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: "follower"}}); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a follower's write: %v, want %v", err, ErrNotLeader)
	}

	// The leader steps down on shutdown and releases the Lease: the follower takes over at its next retry.
	a.stop()
	stopped := time.Now()
	waitFor(t, "node-b to take over", b.leading.Load)
	// Released, the Lease is taken at the follower's next retry, well before it would have expired.
	if took := time.Since(stopped); took >= LeaseDuration {
		t.Errorf("node-b took over after %s", took)
	}
	if leader, err := peer.Leader(ctx, testClient); err != nil || leader != "node-b" {
		t.Errorf("leader %q, %v", leader, err)
	}
	waitFor(t, "node-b to reconcile", marked("node-b"))
}

// memoryLock is a resourcelock.Interface on a record in memory; failing makes its writes fail.
type memoryLock struct {
	mu       sync.Mutex
	identity string
	record   resourcelock.LeaderElectionRecord
	failing  bool
}

func (l *memoryLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.record
	return &r, nil, nil
}

func (l *memoryLock) Create(_ context.Context, r resourcelock.LeaderElectionRecord) error {
	return l.Update(context.Background(), r)
}

func (l *memoryLock) Update(_ context.Context, r resourcelock.LeaderElectionRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failing {
		return errors.New("the API server is unreachable")
	}
	l.record = r
	return nil
}

func (l *memoryLock) RecordEvent(string) {}
func (l *memoryLock) Identity() string   { return l.identity }
func (l *memoryLock) Describe() string   { return "memory" }

// testClock is a clock the test moves.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// heldLease returns a lease on a memory lock that this manager acquired, the lock, its clock and how often it was
// lost.
func heldLease(t *testing.T) (*Lease, *memoryLock, *testClock, *atomic.Int32) {
	t.Helper()
	lock := &memoryLock{identity: peer.HolderIdentity("node-a")}
	clock := &testClock{now: time.Now()}
	lost := &atomic.Int32{}
	lease := newLease(lock, clock.Now, func() { lost.Add(1) })
	if err := lease.Check(); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("before the election: %v", err)
	}
	if err := lease.Create(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: lock.identity}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Check(); err != nil {
		t.Fatalf("after the election: %v", err)
	}
	return lease, lock, clock, lost
}

// TestLeaderStopsWritingWhenLeaseLost: a leader resumed after a pause past its lease has every Kubernetes, Talos and
// audit write refused before its renew loop notices. The renew deadline is below the lease, so the new leader starts
// only after the old one's writes stop.
func TestLeaderStopsWritingWhenLeaseLost(t *testing.T) {
	ctx := context.Background()
	if RenewDeadline >= LeaseDuration {
		t.Fatalf("renew deadline %s is not below the lease %s", RenewDeadline, LeaseDuration)
	}
	lease, _, clock, lost := heldLease(t)
	c, err := client.New(lease.Fence(testConfig), client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "leader-fence"}, Data: map[string]string{"step": "1"}}
	if err := c.Create(ctx, cm); err != nil {
		t.Fatalf("the leader's write: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, cm) })
	clock.advance(RenewDeadline - time.Second)
	cm.Data["step"] = "2"
	if err := c.Update(ctx, cm); err != nil {
		t.Fatalf("a write within the renew deadline: %v", err)
	}
	log, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fencedLog := FencedAudit{Log: log, Lease: lease}
	if err := fencedLog.Write(audit.Entry{Action: "backup"}); err != nil {
		t.Fatalf("the leader's audit entry: %v", err)
	}
	dialled := 0
	access := lease.Talos(func(context.Context) (talos.Node, error) { dialled++; return nil, nil })
	if _, err := access(ctx); err != nil || dialled != 1 {
		t.Fatalf("the leader reaches Talos: %v", err)
	}

	// SIGSTOP for 30 s, SIGCONT: the renewal is 30 s old.
	clock.advance(30 * time.Second)
	cm.Data["step"] = "3"
	if err := c.Update(ctx, cm); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a stale leader's write: %v, want %v", err, ErrNotLeader)
	}
	got := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), got); err != nil || got.Data["step"] != "2" {
		t.Errorf("a stale leader reads %v (%v), want step 2 unchanged", got.Data, err)
	}
	if err := fencedLog.Write(audit.Entry{Action: "restore"}); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a stale leader's audit entry: %v", err)
	}
	if entries, _, err := log.Page("", 10); err != nil || len(entries) != 1 {
		t.Errorf("audit log %v, %v, want the leader's entry only", entries, err)
	}
	if _, err := access(ctx); !errors.Is(err, ErrNotLeader) || dialled != 1 {
		t.Errorf("a stale leader reaches Talos: %v, dialled %d", err, dialled)
	}
	if n := lost.Load(); n != 1 {
		t.Errorf("lost %d times, want once", n)
	}

	t.Run("no node name", func(t *testing.T) {
		if _, err := NewLease(testConfig, "", nil); err == nil {
			t.Error("a lease without the node name")
		}
	})
	t.Run("taken over", func(t *testing.T) {
		lease, lock, _, lost := heldLease(t)
		lock.record.HolderIdentity = peer.HolderIdentity("node-b")
		if _, _, err := lease.Get(ctx); err != nil {
			t.Fatal(err)
		}
		if lease.Leading() || lost.Load() != 1 {
			t.Errorf("leading %v after another manager took the lease, lost %d", lease.Leading(), lost.Load())
		}
	})
	t.Run("renewal failing", func(t *testing.T) {
		lease, lock, clock, lost := heldLease(t)
		lock.failing = true
		clock.advance(RenewDeadline / 2)
		if err := lease.Update(ctx, lock.record); err == nil || !lease.Leading() {
			t.Fatalf("a failed renewal ends the lead before the renew deadline: %v", err)
		}
		clock.advance(RenewDeadline / 2)
		if lease.Leading() || lost.Load() != 1 {
			t.Errorf("leading past the renew deadline, lost %d", lost.Load())
		}
	})
	t.Run("released", func(t *testing.T) {
		lease, _, _, lost := heldLease(t)
		if err := lease.Update(ctx, resourcelock.LeaderElectionRecord{}); err != nil {
			t.Fatal(err)
		}
		if lease.Leading() || lost.Load() != 0 {
			t.Errorf("after the release: leading %v, lost %d", lease.Leading(), lost.Load())
		}
	})
	t.Run("watched", func(t *testing.T) {
		lease, _, clock, lost := heldLease(t)
		clock.advance(LeaseDuration)
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := lease.Watch(ctx); err != nil || lost.Load() != 1 {
			t.Errorf("watch: %v, lost %d", err, lost.Load())
		}
	})
}

// recordingNode is a Talos node that counts the calls that reach it.
type recordingNode struct {
	talos.Node
	reboots, upgrades, patches, versions int
}

func (n *recordingNode) Reboot(context.Context) error              { n.reboots++; return nil }
func (n *recordingNode) Upgrade(context.Context, string) error     { n.upgrades++; return nil }
func (n *recordingNode) PatchConfig(context.Context, []byte) error { n.patches++; return nil }
func (n *recordingNode) Versions(context.Context) (string, string, error) {
	n.versions++
	return "v1", "v1", nil
}

// TestHeldTalosNodeRefusesChangesWhenLeaseLost: a leader that acquired a Talos node and was paused past its lease
// has every change through that node refused, while reads still pass.
func TestHeldTalosNodeRefusesChangesWhenLeaseLost(t *testing.T) {
	ctx := context.Background()
	lease, _, clock, _ := heldLease(t)
	fake := &recordingNode{}
	node, err := lease.Talos(func(context.Context) (talos.Node, error) { return fake, nil })(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Reboot(ctx); err != nil || fake.reboots != 1 {
		t.Fatalf("the leader's reboot: %v, %d reboots", err, fake.reboots)
	}

	clock.advance(30 * time.Second)
	if err := node.Reboot(ctx); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a stale leader's reboot: %v, want %v", err, ErrNotLeader)
	}
	if err := node.Upgrade(ctx, "installer"); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a stale leader's upgrade: %v, want %v", err, ErrNotLeader)
	}
	if err := node.PatchConfig(ctx, nil); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a stale leader's patch: %v, want %v", err, ErrNotLeader)
	}
	if fake.reboots != 1 || fake.upgrades != 0 || fake.patches != 0 {
		t.Errorf("Talos got %d reboots, %d upgrades, %d patches; want the leader's reboot only", fake.reboots,
			fake.upgrades, fake.patches)
	}
	if _, _, err := node.Versions(ctx); err != nil || fake.versions != 1 {
		t.Errorf("a stale leader's read: %v, %d reads", err, fake.versions)
	}
}
