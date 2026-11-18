// Package firstboot turns a freshly booted Talos node into a running ILM appliance: it configures the node,
// bootstraps etcd, hands the setup over to the Appliance Manager and reports the Appliance status.
package firstboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/setup"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// resetTimeout is how long a requested reset may take before the system images are checked again.
var resetTimeout = 10 * time.Minute

const (
	maxBackoff             = time.Minute
	defaultConsoleInterval = 60 * time.Second
)

// Wizard is wizard part 1, which collects the node settings and the administrator in maintenance mode.
type Wizard interface {
	// Run serves the wizard for the node address ip, shows its console lines through show, and returns the machine
	// configuration input once the administrator applied it. It stops serving before it returns.
	Run(ctx context.Context, ip netip.Addr, show func(lines []string)) (machineconfig.Input, error)
}

// Deps is what Run needs. Console and Log must not be nil; ConsoleInterval defaults to defaultConsoleInterval.
// Manager is the Appliance Manager manifest with its image set; HostName and ILMVersion are the release defaults
// of the default Appliance, which a node without a hand-over file gets.
type Deps struct {
	Node            talos.Node
	Wizard          Wizard
	NewCluster      func([]byte) (Cluster, error)
	Manager         []byte
	Generate        func(machineconfig.Input) ([]byte, error)
	Console         io.Writer
	Log             *slog.Logger
	Retry           time.Duration
	InstallerImage  string
	HostName        string
	ILMVersion      string
	ClusterName     string
	Version         string
	ConsoleInterval time.Duration
}

func (d Deps) validate() error {
	switch {
	case d.Node == nil || d.Wizard == nil || d.NewCluster == nil || d.Generate == nil || len(d.Manager) == 0:
		return errors.New("first boot: node, wizard, cluster, generator and manager manifest are required")
	case d.Console == nil || d.Log == nil:
		return errors.New("first boot: console and logger are required")
	case d.Retry <= 0:
		return errors.New("first boot: retry interval must be positive")
	case d.InstallerImage == "" || d.HostName == "" || d.ILMVersion == "" || d.ClusterName == "":
		return errors.New("first boot: installer image, host name, ILM version and cluster name are required")
	}
	return nil
}

// Run drives the node to a running cluster, hands the setup over to the Appliance Manager and then reports the
// Appliance status until ctx is done. Every decision is taken from what the node and the cluster report, so a
// restart at any point resumes where the setup stands.
// It returns nil when ctx is cancelled and an error only for invalid Deps.
func Run(ctx context.Context, d Deps, s *StatusStore) error {
	if err := d.validate(); err != nil {
		return err
	}
	interval := d.ConsoleInterval
	if interval <= 0 {
		interval = defaultConsoleInterval
	}
	go runConsole(ctx, d.Console, d.Log, d.Version, d.HostName, interval, s)

	r := &runner{d: d, s: s}
	r.run(ctx)
	return nil
}

type runner struct {
	d       Deps
	s       *StatusStore
	nodeIP  string
	applied bool // a configuration was applied by this process; never apply a second one
	// answers are the settings of wizard part 1, kept until a configuration generated from them is applied.
	answers *machineconfig.Input
	// resetting is set once a reset to recover system images was requested at resetAt; the reset ends this
	// process. Talos runs the reset after it answered, so a reset that fails leaves the process running.
	resetting bool
	resetAt   time.Time
	// resetMissed is set when a requested reset did not happen within resetTimeout.
	resetMissed bool
	// wasReady is set once META shows, or cannot rule out, that the node has been ready; its storage then holds
	// user data and is never reset.
	wasReady bool
	// marked is set once this process wrote the ready marker.
	marked bool
}

func (r *runner) run(ctx context.Context) {
	if r.retry(ctx, r.nodePass) != nil {
		return
	}
	var cluster Cluster
	err := r.retry(ctx, func(ctx context.Context) (bool, error) {
		var err error
		cluster, err = r.connect(ctx)
		return err == nil, err
	})
	if err != nil {
		return
	}
	if r.retry(ctx, func(ctx context.Context) (bool, error) { return r.handover(ctx, cluster) }) != nil {
		return
	}
	r.mirror(ctx, cluster)
}

// markReady marks the node in META as having been ready, which disables the storage reset for good, and then
// forgets the reset record. The record stays while the marker is missing, so it still bounds the reset.
func (r *runner) markReady(ctx context.Context) error {
	if err := r.d.Node.MarkReady(ctx); err != nil {
		return err
	}
	if err := r.d.Node.ClearResetRecord(ctx); err != nil {
		r.d.Log.Warn("could not clear the record of the node storage reset", "error", err)
	}
	return nil
}

// recordReady writes the ready marker, waiting as retry does between failures, until it succeeds or ctx is done.
// It never checks the system images: the platform is up, so no reset may be requested while the marker is missing.
func (r *runner) recordReady(ctx context.Context) error {
	for failures := 0; ; failures++ {
		err := r.markReady(ctx)
		if err == nil {
			r.d.Log.Info("node marked as ready")
			return nil
		}
		cur := r.s.Get()
		r.set(cur.State, cur.Step, "recording the ready marker: "+err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(r.d.Retry, failures)):
		}
	}
}

// retry calls pass until it reports done, unless broken system images are being recovered. Between calls it waits
// Retry, doubling while the state and step stay the same, up to one minute. An error is recorded in the status
// message and the log; it never ends the loop.
func (r *runner) retry(ctx context.Context, pass func(context.Context) (bool, error)) error {
	failures := 0
	last := r.s.Get()
	for {
		recovering, err := r.recoverImages(ctx)
		done := false
		if !recovering {
			done, err = pass(ctx)
		}
		if done {
			return nil
		}
		cur := r.s.Get()
		if err != nil {
			r.set(cur.State, cur.Step, err.Error())
		}
		if cur.State != last.State || cur.Step != last.Step {
			failures, last = 0, cur
		}
		wait := backoff(r.d.Retry, failures)
		failures++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// backoff returns retry doubled failures times, capped at one minute.
func backoff(retry time.Duration, failures int) time.Duration {
	d := retry
	for range failures {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return min(d, maxBackoff)
}

// set records the status and logs state changes and new messages. Messages never contain configuration or
// kubeconfig bytes: every error that could carry them is replaced by a fixed text before it gets here.
func (r *runner) set(state State, step, message string) {
	moved, changed := r.s.set(state, step, message)
	switch {
	case moved:
		r.d.Log.Info("state changed", "state", string(state), "step", step, "message", message)
	case changed:
		r.d.Log.Info("status", "state", string(state), "step", step, "message", message)
	}
}

// recoverImages resets the node storage when a system service cannot execute the binary of its image, as a power
// cut shortly after the image was unpacked leaves it. Removing the image does not help: containerd keeps the
// unpacked layers while a container uses them and reuses them on the next unpack. The reset wipes EPHEMERAL, which
// holds the images, their layers and the half-built cluster, and keeps the machine configuration, so the setup
// starts over from a configured node. It runs only on a node that has never been ready, as marked in META, when
// there is no user data yet; a marker that cannot be read counts as set.
// The node is reset once until it is ready, as recorded in META before the reset: an image that still fails after
// a reset is broken in the image cache itself, so it is reported instead of resetting again. Without that record
// there is no reset at all. It reports whether a reset is under way or blocked; a failed check is logged and
// leaves the pass to run.
func (r *runner) recoverImages(ctx context.Context) (bool, error) {
	if r.resetting {
		if time.Since(r.resetAt) < resetTimeout {
			return true, nil
		}
		r.resetting, r.resetMissed = false, true
		r.d.Log.Warn("the requested reset did not happen, checking the system images again", "waited", resetTimeout)
	}
	if r.wasReady {
		return false, nil
	}
	broken, err := r.d.Node.BrokenSystemImages(ctx)
	if err != nil {
		r.d.Log.Debug("system image check failed", "error", err)
		return false, nil
	}
	if len(broken) == 0 {
		return false, nil
	}
	if wasReady, err := r.d.Node.WasReady(ctx); err != nil || wasReady {
		r.wasReady = true
		r.d.Log.Warn("system images fail to execute, not resetting node storage: the node may have been ready",
			"images", broken, "error", err)
		return false, nil
	}
	recorded, err := r.d.Node.ResetRecorded(ctx)
	if err != nil {
		return true, err
	}
	switch {
	case recorded && r.resetMissed:
		return true, fmt.Errorf("requested node storage reset did not happen, system image %s fails to execute", broken[0])
	case recorded:
		return true, fmt.Errorf("system image %s fails to execute after a reset", broken[0])
	}
	if err := r.d.Node.RecordReset(ctx); err != nil {
		return true, err
	}
	cur := r.s.Get()
	r.d.Log.Warn("system images fail to execute", "images", broken)
	r.set(cur.State, cur.Step, "resetting node storage to recover broken system images")
	err = r.d.Node.ResetEphemeral(ctx)
	if err != nil && resetRejected(err) {
		// No reset happened, so it is not used up: the next pass may try again.
		r.d.Log.Warn("could not reset node storage", "error", err)
		if err := r.d.Node.ClearResetRecord(ctx); err != nil {
			r.d.Log.Warn("could not clear the record of the node storage reset", "error", err)
		}
		return true, fmt.Errorf("could not reset node storage: %s", status.Code(err))
	}
	if err != nil {
		// machined may have scheduled the reset before the answer was lost, so wait for it as for an accepted one.
		r.d.Log.Warn("node storage reset not confirmed, waiting for it", "error", err)
	}
	r.resetting, r.resetAt = true, time.Now()
	return true, nil
}

// resetRejected reports whether err proves that machined refused the reset before scheduling it: machined answers
// those cases with a status of its own, while a lost connection, a timeout or an internal error leave it open
// whether the reset runs.
func resetRejected(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.FailedPrecondition, codes.Unimplemented,
		codes.Unauthenticated, codes.AlreadyExists:
		return true
	default:
		return false
	}
}

// nodePass looks at the node once and moves it one stage forward. It reports done once etcd is bootstrapped.
func (r *runner) nodePass(ctx context.Context) (bool, error) {
	addr, addrErr := r.refreshAddress(ctx)
	stage, err := r.d.Node.Stage(ctx)
	if err != nil {
		return false, err
	}
	switch stage {
	case talos.StageMaintenance:
		return false, r.maintenance(ctx, addr, addrErr)
	case talos.StageRunning:
		return r.bootstrap(ctx)
	case talos.StageBooting:
		return r.booting(ctx, stage)
	default:
		r.set(StateInstalling, "", fmt.Sprintf("node stage %s", stage))
		return false, nil
	}
}

// booting bootstraps a configured node: Talos stays in the booting stage until etcd runs, and etcd waits for the
// bootstrap. A node without a configuration is still starting up and is left alone.
func (r *runner) booting(ctx context.Context, stage talos.Stage) (bool, error) {
	configured, err := r.d.Node.Configured(ctx)
	if err != nil || !configured {
		r.set(StateInstalling, "", fmt.Sprintf("node stage %s", stage))
		return false, err
	}
	return r.bootstrap(ctx)
}

// maintenance configures a node in maintenance mode from the answers of wizard part 1 unless it already has a
// configuration: the node stays in maintenance while it installs, so a restarted service must not generate and apply
// a second one. It waits in the wizard until the administrator applies it.
func (r *runner) maintenance(ctx context.Context, addr string, addrErr error) error {
	// applied covers the gap between ApplyConfig returning and the node reporting its configuration.
	if r.applied {
		r.set(StateInstalling, "", "configuration applied, waiting for the node to install")
		return nil
	}
	configured, err := r.d.Node.Configured(ctx)
	if err != nil {
		return err
	}
	if configured {
		r.set(StateInstalling, "", "the node has a configuration, waiting for it to install")
		return nil
	}
	if addrErr != nil {
		r.set(StateWaitingForAddress, "", "")
		return addrErr
	}
	if addr == "" {
		r.set(StateWaitingForAddress, "", "no global IPv4 address yet")
		return nil
	}
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return err
	}
	if r.answers == nil {
		in, err := r.d.Wizard.Run(ctx, ip, func(lines []string) {
			r.s.setConsole(StateSetup, lines)
			r.set(StateSetup, "", "waiting for wizard part 1")
		})
		if err != nil {
			return err
		}
		r.answers = &in
	}
	return r.configure(ctx, ip)
}

// refreshAddress reads the node addresses and returns the first global unicast IPv4 address, or "" if none.
func (r *runner) refreshAddress(ctx context.Context) (string, error) {
	prefixes, err := r.d.Node.Addresses(ctx)
	if err != nil {
		return "", err
	}
	addr, ok := machineconfig.PickAddress(prefixes)
	if !ok {
		return "", nil
	}
	if ip := addr.String(); ip != r.nodeIP {
		r.nodeIP = ip
		r.s.setNodeIP(ip)
		r.d.Log.Info("node address", "ip", ip)
	}
	return r.nodeIP, nil
}

// configure generates the machine configuration from the wizard answers for the node at ip and applies it; the node
// then installs and reboots. A static address goes on the link that holds ip. Answers the generator or the node
// refuses are dropped, so the wizard runs again.
func (r *runner) configure(ctx context.Context, ip netip.Addr) error {
	r.set(StateConfiguring, "", "generating the machine configuration")
	in := *r.answers
	in.ClusterName, in.InstallerImage, in.NodeIP = r.d.ClusterName, r.d.InstallerImage, ip.String()
	if in.Address.IsValid() {
		nw, err := r.d.Node.Network(ctx, ip)
		if err != nil {
			return err
		}
		if nw.Link == "" {
			return fmt.Errorf("no link holds the address %s", ip)
		}
		in.NodeIP, in.Link = in.Address.Addr().String(), nw.Link
	}
	cfg, err := r.d.Generate(in)
	if err != nil {
		// The error may quote the input, which holds the hand-over with the password hash and TOTP seed.
		r.answers = nil
		return errors.New("generate machine configuration failed: the answers were refused, the setup runs again")
	}
	if err := r.d.Node.ApplyConfig(ctx, cfg); err != nil {
		if status.Code(err) == codes.InvalidArgument {
			// Applying the same answers again would be refused again.
			r.answers = nil
		}
		return redacted("apply machine configuration", err)
	}
	r.applied = true
	r.s.setConsole(StateInstalling, []string{"Installing, the console moves to https://" + in.NodeIP + ":" + setup.Port + "/"})
	r.set(StateInstalling, "", "configuration applied, the node installs and reboots")
	return nil
}

// bootstrap bootstraps etcd unless the node says it already is. A failed check is not trusted: Bootstrap
// reports an existing etcd as ErrAlreadyBootstrapped, so calling it is always safe.
func (r *runner) bootstrap(ctx context.Context) (bool, error) {
	r.set(StateBootstrapping, "", "checking etcd")
	ok, err := r.d.Node.IsBootstrapped(ctx)
	if err == nil && ok {
		return true, nil
	}
	if err != nil {
		r.d.Log.Debug("etcd check failed, bootstrapping anyway", "error", err)
	}
	switch err := r.d.Node.Bootstrap(ctx); {
	case errors.Is(err, talos.ErrAlreadyBootstrapped):
		r.d.Log.Info("etcd already bootstrapped")
	case err != nil:
		return false, err
	default:
		r.d.Log.Info("etcd bootstrapped")
	}
	return true, nil
}

func (r *runner) connect(ctx context.Context) (Cluster, error) {
	r.set(StateHandover, "", "connecting to Kubernetes")
	kc, err := r.d.Node.Kubeconfig(ctx)
	if err != nil {
		return nil, redacted("fetch kubeconfig", err)
	}
	cluster, err := r.d.NewCluster(kc)
	if err != nil {
		return nil, errors.New("connect to Kubernetes: kubeconfig rejected")
	}
	return cluster, nil
}

// redacted replaces an error that may quote the configuration or kubeconfig with a fixed text and its gRPC code.
func redacted(what string, err error) error {
	return fmt.Errorf("%s failed (%s)", what, status.Code(err))
}
