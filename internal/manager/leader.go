package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crmanager "sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/peer"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// The managers of the voting nodes elect their leader with a Lease. A leader that cannot renew steps down after
// RenewDeadline, before any other manager can take the Lease after LeaseDuration.
const (
	LeaseDuration = 15 * time.Second
	RenewDeadline = 10 * time.Second
	RetryPeriod   = 2 * time.Second
)

// ErrNotLeader refuses a write of a manager that does not hold a fresh lease.
var ErrNotLeader = errors.New("this manager does not lead")

// Lease is the manager's leader election lock. It records when this manager last renewed the Lease, so that every
// leader-only write can check that it still leads: a manager that was paused or cut off resumes believing it leads
// until its renew loop gives up, while another manager may already have taken over.
type Lease struct {
	resourcelock.Interface
	now func() time.Time
	// onLost is called once when a manager that led finds it no longer does.
	onLost func()

	mu sync.Mutex
	// renewed is when the last successful renewal was sent; zero while this manager does not hold the Lease.
	renewed time.Time
	// led is set while this manager holds the Lease and cleared when it releases it.
	led  bool
	lost bool
}

// NewLease returns the Lease of the manager on the node node, written with cfg. onLost is called once when the
// manager finds it no longer leads, before its renew loop does.
func NewLease(cfg *rest.Config, node string, onLost func()) (*Lease, error) {
	if node == "" {
		return nil, errors.New("the leader election needs the node name")
	}
	cfg = rest.AddUserAgent(rest.CopyConfig(cfg), "leader-election")
	// A hung request must not use up the renew deadline.
	cfg.Timeout = RenewDeadline / 2
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create the leader election client: %w", err)
	}
	coordination, err := coordinationv1client.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create the leader election client: %w", err)
	}
	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, SystemNamespace, peer.LeaseName, core, coordination,
		resourcelock.ResourceLockConfig{Identity: peer.HolderIdentity(node)})
	if err != nil {
		return nil, fmt.Errorf("create the leader election lock: %w", err)
	}
	return newLease(lock, time.Now, onLost), nil
}

func newLease(lock resourcelock.Interface, now func() time.Time, onLost func()) *Lease {
	return &Lease{Interface: lock, now: now, onLost: onLost}
}

// ElectionOptions returns o with the leader election on lease: the manager runs its leader-only runnables, every
// controller among them, only while it holds the Lease, and releases it when it stops.
func ElectionOptions(o ctrl.Options, lease *Lease) ctrl.Options {
	o.LeaderElection = true
	o.LeaderElectionID = peer.LeaseName
	o.LeaderElectionNamespace = SystemNamespace
	o.LeaderElectionResourceLockInterface = lease
	o.LeaderElectionReleaseOnCancel = true
	o.LeaseDuration = ptr.To(LeaseDuration)
	o.RenewDeadline = ptr.To(RenewDeadline)
	o.RetryPeriod = ptr.To(RetryPeriod)
	return o
}

// Get reads the Lease; once another manager holds it, this one no longer leads.
func (l *Lease) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	record, raw, err := l.Interface.Get(ctx)
	if err == nil && record.HolderIdentity != l.Identity() {
		l.mu.Lock()
		l.renewed = time.Time{}
		l.mu.Unlock()
	}
	return record, raw, err
}

// Create takes the Lease when it does not exist yet.
func (l *Lease) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	sent := l.now()
	err := l.Interface.Create(ctx, record)
	l.written(record, sent, err)
	return err
}

// Update takes, renews or releases the Lease.
func (l *Lease) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	sent := l.now()
	err := l.Interface.Update(ctx, record)
	l.written(record, sent, err)
	return err
}

// written records the outcome of writing record, sent at sent. A failed write changes nothing: the renew deadline
// decides.
func (l *Lease) written(record resourcelock.LeaderElectionRecord, sent time.Time, err error) {
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if record.HolderIdentity == l.Identity() {
		l.renewed, l.led = sent, true
		return
	}
	l.renewed, l.led = time.Time{}, false
}

// Check returns ErrNotLeader unless this manager holds the Lease and renewed it less than RenewDeadline ago. A
// manager that led and fails the check calls onLost.
func (l *Lease) Check() error {
	l.mu.Lock()
	fresh := !l.renewed.IsZero() && l.now().Sub(l.renewed) < RenewDeadline
	lost := !fresh && l.led && !l.lost
	if lost {
		l.lost = true
	}
	l.mu.Unlock()
	if lost && l.onLost != nil {
		l.onLost()
	}
	if !fresh {
		return ErrNotLeader
	}
	return nil
}

// Leading reports whether Check passes.
func (l *Lease) Leading() bool {
	return l.Check() == nil
}

// Watch checks the lease every RetryPeriod until ctx ends, so that a manager that resumes after a pause finds it
// lost the lead even while it writes nothing.
func (l *Lease) Watch(ctx context.Context) error {
	ticker := time.NewTicker(RetryPeriod)
	defer ticker.Stop()
	for {
		_ = l.Check()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Fence returns a copy of cfg whose requests other than reads fail with ErrNotLeader unless Check passes. Every
// Kubernetes client of the leader-only work is made from it; the Lease itself is written past it.
func (l *Lease) Fence(cfg *rest.Config) *rest.Config {
	fenced := rest.CopyConfig(cfg)
	fenced.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if err := l.Check(); err != nil {
					if r.Body != nil {
						_ = r.Body.Close()
					}
					return nil, fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, err)
				}
			}
			return next.RoundTrip(r)
		})
	})
	return fenced
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Talos returns access that fails with ErrNotLeader unless Check passes.
func (l *Lease) Talos(access func(context.Context) (talos.Node, error)) func(context.Context) (talos.Node, error) {
	return func(ctx context.Context) (talos.Node, error) {
		if err := l.Check(); err != nil {
			return nil, err
		}
		return access(ctx)
	}
}

// FencedAudit is the audit log that only the leader writes.
type FencedAudit struct {
	*audit.Log
	Lease *Lease
}

// Write appends e unless Check fails.
func (a FencedAudit) Write(e audit.Entry) error {
	if err := a.Lease.Check(); err != nil {
		return err
	}
	return a.Log.Write(e)
}

// electedRunnable is a runnable that runs on the leader only, or on every manager.
type electedRunnable struct {
	crmanager.RunnableFunc
	leader bool
}

func (r electedRunnable) NeedLeaderElection() bool { return r.leader }

// OnLeader returns fn as a runnable that runs only while the manager leads.
func OnLeader(fn func(context.Context) error) crmanager.Runnable {
	return electedRunnable{RunnableFunc: fn, leader: true}
}

// OnEveryNode returns fn as a runnable that runs on every manager, leading or not.
func OnEveryNode(fn func(context.Context) error) crmanager.Runnable {
	return electedRunnable{RunnableFunc: fn}
}
