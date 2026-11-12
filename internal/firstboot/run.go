// Package firstboot turns a freshly booted Talos node into a running ILM appliance: it configures the node,
// bootstraps etcd, applies the add-ons and waits for the default ILM Platform.
package firstboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"time"

	"google.golang.org/grpc/status"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	maxBackoff             = time.Minute
	defaultConsoleInterval = 60 * time.Second
	// platformStep is the step that creates the ILM Platform; the service reports StatePlatform while it runs.
	platformStep = "platform"
)

// Deps is what Run needs. Console and Log must not be nil; ConsoleInterval defaults to defaultConsoleInterval.
type Deps struct {
	Node            talos.Node
	NewCluster      func([]byte) (kube.Cluster, error)
	Steps           []kube.Step
	Addons          fs.FS
	Generate        func(machineconfig.Input) ([]byte, error)
	Console         io.Writer
	Log             *slog.Logger
	Retry           time.Duration
	InstallerImage  string
	HostName        string
	ClusterName     string
	Version         string
	ConsoleInterval time.Duration
}

func (d Deps) validate() error {
	switch {
	case d.Node == nil || d.NewCluster == nil || d.Generate == nil || d.Addons == nil:
		return errors.New("first boot: node, cluster, generator and add-ons are required")
	case d.Console == nil || d.Log == nil:
		return errors.New("first boot: console and logger are required")
	case d.Retry <= 0:
		return errors.New("first boot: retry interval must be positive")
	case d.InstallerImage == "" || d.HostName == "" || d.ClusterName == "":
		return errors.New("first boot: installer image, host name and cluster name are required")
	}
	return nil
}

// Run drives the node to a ready ILM appliance and then idles until ctx is done. Every decision is taken from
// what the node and the cluster report, so a restart at any point resumes where the setup stands.
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
	// run returns an error only when ctx is cancelled; once ready, keep serving status until then.
	if r.run(ctx) == nil {
		<-ctx.Done()
	}
	return nil
}

type runner struct {
	d       Deps
	s       *StatusStore
	nodeIP  string
	applied bool // a configuration was applied by this process; never apply a second one
}

func (r *runner) run(ctx context.Context) error {
	if err := r.retry(ctx, r.nodePass); err != nil {
		return err
	}
	var cluster kube.Cluster
	err := r.retry(ctx, func(ctx context.Context) (bool, error) {
		var err error
		cluster, err = r.connect(ctx)
		return err == nil, err
	})
	if err != nil {
		return err
	}
	for _, step := range r.d.Steps {
		if err := r.retry(ctx, func(ctx context.Context) (bool, error) { return r.applyStep(ctx, cluster, step) }); err != nil {
			return err
		}
		if err := r.retry(ctx, func(ctx context.Context) (bool, error) { return r.waitStep(ctx, cluster, step) }); err != nil {
			return err
		}
	}
	r.set(StateReady, "", "ILM Platform is ready")
	return nil
}

// retry calls pass until it reports done. Between calls it waits Retry, doubling while the state and step stay
// the same, up to one minute. An error is recorded in the status message and the log; it never ends the loop.
func (r *runner) retry(ctx context.Context, pass func(context.Context) (bool, error)) error {
	failures := 0
	last := r.s.Get()
	for {
		done, err := pass(ctx)
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

// maintenance configures a node in maintenance mode unless it already has a configuration: the node stays in
// maintenance while it installs, so a restarted service must not generate and apply a second one.
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
	return r.configure(ctx, addr)
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

// configure generates the machine configuration for addr and applies it; the node then installs and reboots.
func (r *runner) configure(ctx context.Context, addr string) error {
	r.set(StateConfiguring, "", "generating the machine configuration")
	cfg, err := r.d.Generate(machineconfig.Input{
		ClusterName:    r.d.ClusterName,
		NodeIP:         addr,
		InstallerImage: r.d.InstallerImage,
		HostName:       r.d.HostName,
	})
	if err != nil {
		return fmt.Errorf("generate machine configuration: %w", err)
	}
	if err := r.d.Node.ApplyConfig(ctx, cfg); err != nil {
		return redacted("apply machine configuration", err)
	}
	r.applied = true
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

func (r *runner) connect(ctx context.Context) (kube.Cluster, error) {
	r.set(StateAddons, "", "connecting to Kubernetes")
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

func stepState(step kube.Step) State {
	if step.Name == platformStep {
		return StatePlatform
	}
	return StateAddons
}

func (r *runner) applyStep(ctx context.Context, cluster kube.Cluster, step kube.Step) (bool, error) {
	r.set(stepState(step), step.Name, "applying")
	for _, ns := range slices.Sorted(maps.Keys(step.NamespaceLabels)) {
		if err := cluster.LabelNamespace(ctx, ns, step.NamespaceLabels[ns]); err != nil {
			return false, err
		}
	}
	for _, file := range step.Files {
		docs, err := fs.ReadFile(r.d.Addons, file)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", file, err)
		}
		write := cluster.Apply
		if step.CreateOnly {
			write = cluster.Create
		}
		if err := write(ctx, docs); err != nil {
			return false, fmt.Errorf("apply %s: %w", file, err)
		}
	}
	r.d.Log.Info("step applied", "step", step.Name)
	return true, nil
}

// waitStep reports done once every wait of the step passes; otherwise the message names the first that does not.
func (r *runner) waitStep(ctx context.Context, cluster kube.Cluster, step kube.Step) (bool, error) {
	for _, w := range step.Wait {
		ready, reason, err := cluster.Ready(ctx, w)
		if err != nil {
			return false, err
		}
		if !ready {
			r.set(stepState(step), step.Name, fmt.Sprintf("waiting for %s %s: %s", w.Kind, objectName(w), reason))
			return false, nil
		}
	}
	r.d.Log.Info("step ready", "step", step.Name)
	return true, nil
}

func objectName(w kube.WaitFor) string {
	switch {
	case w.Namespace != "":
		return w.Namespace + "/" + w.Name
	case w.Name != "":
		return w.Name
	default:
		return "(all)"
	}
}

// redacted replaces an error that may quote the configuration or kubeconfig with a fixed text and its gRPC code.
func redacted(what string, err error) error {
	return fmt.Errorf("%s failed (%s)", what, status.Code(err))
}
