package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/siderolabs/talos/pkg/machinery/constants"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/firstboot"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// Steps of a system update besides those it shares with a platform update, and the step of a failed health check
// that rolled Talos back.
const (
	StepManager    = "manager"
	StepTalos      = "talos"
	StepHealth     = "health"
	StepKubernetes = "kubernetes"
	StepAddons     = v1alpha1.UpdateStepAddons
	StepRolledBack = "rolledBack"
)

// Reasons a system update is refused or fails.
const (
	// KubernetesOlderThanRunning is a system bundle whose Kubernetes is older than the one that runs: Kubernetes is
	// never downgraded.
	KubernetesOlderThanRunning = "kubernetesOlderThanRunning"
	// KubernetesMinorSkip is a system bundle whose Kubernetes is more than one minor version above the one that runs:
	// Kubernetes moves one minor version at a time.
	KubernetesMinorSkip = "kubernetesMinorSkip"
	// HealthCheckFailed is a node that did not come back healthy from the reboot into the new Talos in time; Talos
	// was rolled back.
	HealthCheckFailed = "healthCheckFailed"
	// TalosRollbackFailed is a failed health check whose Talos rollback did not take: the node runs the new Talos.
	TalosRollbackFailed = "talosRollbackFailed"
)

// systemOrder is the order of the steps of a system update.
var systemOrder = []string{
	StepBackup, StepImages, StepManager, StepTalos, StepHealth, StepKubernetes, StepAddons, StepDone,
}

// systemSteps run each step of a system update; each can run again after a restart.
var systemSteps = map[string]func(j *Jobs, ctx context.Context, jb *job) error{
	StepBackup:     (*Jobs).backupStep,
	StepImages:     (*Jobs).imagesStep,
	StepManager:    (*Jobs).managerStep,
	StepTalos:      (*Jobs).talosStep,
	StepHealth:     (*Jobs).healthStep,
	StepKubernetes: (*Jobs).kubernetesStep,
	StepAddons:     (*Jobs).addonsStep,
	StepRolledBack: (*Jobs).rolledBackStep,
}

// kubernetesComponent is a Kubernetes component a system update moves to the bundle's version: its name in the
// Talos configuration, the repository of its image and the k8s-app label of its static pod, empty for the kubelet.
type kubernetesComponent struct {
	name, repository, app string
}

// kubernetesComponents are moved in this order, each once the one before runs healthy at the new version.
var kubernetesComponents = []kubernetesComponent{
	{name: "apiServer", repository: "registry.k8s.io/kube-apiserver", app: "kube-apiserver"},
	{name: "controllerManager", repository: "registry.k8s.io/kube-controller-manager", app: "kube-controller-manager"},
	{name: "scheduler", repository: "registry.k8s.io/kube-scheduler", app: "kube-scheduler"},
	{name: "kubelet", repository: "ghcr.io/siderolabs/kubelet"},
}

// The files of a system bundle, and the names of the images built with the appliance it carries.
const (
	releaseFile    = "release.yaml"
	installerImage = "installer"
	managerImage   = "manager"
)

// The reason with which a system update marks the conditions it waits for the reconciler to report again.
const reasonRechecking = "UpdateRechecking"

// StartSystem checks that the loaded system bundle version to may be installed now and installs it in the
// background, started by user: the manager of the running release takes the backup, tags the images and applies the
// bundle's manager, whose manager upgrades Talos and Kubernetes and releases the add-on pass. It refuses with an
// *Error while another long operation runs, while the operator upgrades the platform, when the bundle is not loaded
// or cannot be installed, would downgrade Kubernetes, or needs more free space than the data volume has. A system
// update that failed past its point of no return runs again from its recorded step in the manager of its version.
func (j *Jobs) StartSystem(ctx context.Context, to string, user string) error {
	return j.startUpdate(ctx, user, func(ctx context.Context) (v1alpha1.UpdateStatus, error) {
		return j.admitSystem(ctx, to)
	})
}

// admitSystem refuses an update to the system bundle to that may not start now and returns its status.
func (j *Jobs) admitSystem(ctx context.Context, to string) (v1alpha1.UpdateStatus, error) {
	a, err := j.appliance(ctx)
	if err != nil {
		return v1alpha1.UpdateStatus{}, fail("read the appliance", err)
	}
	if err := j.refuseBusy(ctx, a); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	if err := j.refuseFailedUpdate(a); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	info, ok := target(a.Status.Updates, release.BundleSystem, to, j.deps.Arch, false)
	if !ok {
		return v1alpha1.UpdateStatus{}, &Error{Reason: NotFound}
	}
	if u := a.Status.Update; Retryable(u, to) && j.deps.Version == to {
		j.deps.Log.Info("the failed system update runs again from its step", "to", to, "step", u.Step, "reason", u.Message)
		st := *u
		st.Message = ""
		return st, nil
	}
	cur, err := j.systemCurrent(a)
	if err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	if err := j.refuseSystemBundle(a, info, cur); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	if err := j.refuseSpace(info); err != nil {
		return v1alpha1.UpdateStatus{}, err
	}
	return v1alpha1.UpdateStatus{
		Kind: release.BundleSystem, From: runningVersion(cur, release.BundleSystem), To: to, Step: StepBackup,
		StartedAt: metav1.NewTime(j.deps.Now()),
	}, nil
}

// Retryable tells whether st is a system update to version that failed past its point of no return: it may run again
// from its recorded step, in the manager of version.
func Retryable(st *v1alpha1.UpdateStatus, version string) bool {
	return st != nil && st.Kind == release.BundleSystem && st.Message != "" && st.PointOfNoReturn && st.To == version
}

// systemCurrent is what a runs for the check of a system bundle: its operator is the one the add-on pass applies,
// which runs, or is about to, even while the status reports none.
func (j *Jobs) systemCurrent(a *v1alpha1.Appliance) (Current, error) {
	cur := current(a, j.deps.Release, j.deps.Arch)
	md, err := InstalledPlatform(j.deps.Dir, a.Status.Versions.PlatformBundle, j.deps.Arch)
	if err != nil {
		return Current{}, fail("read the platform", err)
	}
	cur.Operator = j.activeOperator(md)
	return cur, nil
}

// refuseSystemBundle refuses the loaded system bundle info when its files are gone, it cannot be installed on cur,
// carries no installer, manager or etcd image, or would downgrade the Kubernetes that runs or skip one of its minor versions.
func (j *Jobs) refuseSystemBundle(a *v1alpha1.Appliance, info v1alpha1.BundleInfo, cur Current) error {
	md, err := installed(j.deps.Dir, release.BundleSystem, info.Version, j.deps.Arch)
	if err != nil {
		return &Error{Reason: BundleUnavailable, err: err}
	}
	infos := []v1alpha1.BundleInfo{info}
	if recheck(infos, cur); infos[0].Blocked != "" {
		return &Error{Reason: infos[0].Blocked}
	}
	for _, name := range []string{installerImage, managerImage} {
		if _, ok := builtImage(md, name); !ok {
			return &Error{Reason: InvalidBundle, err: fmt.Errorf("the bundle carries no %s image", name)}
		}
	}
	if len(bootImages(md)) == 0 {
		return &Error{Reason: InvalidBundle, err: errors.New("the bundle carries no etcd image")}
	}
	switch {
	case older(md.Kubernetes, a.Status.Versions.Kubernetes):
		return &Error{Reason: KubernetesOlderThanRunning}
	case skipsMinor(md.Kubernetes, a.Status.Versions.Kubernetes):
		return &Error{Reason: KubernetesMinorSkip}
	}
	return nil
}

// runs tells whether this manager has anything to do at the recorded step of st: past the hand-over, a system update
// runs in the manager of the version it installs. The manager of the running release waits for the hand-over, ends
// an update handed back to it or rolled back, which run it again, and fails one whose node booted without the new
// Talos.
func (j *Jobs) runs(st *v1alpha1.UpdateStatus) bool {
	if st.Kind != release.BundleSystem || j.deps.Version == st.To || st.HandBack != "" || st.Step == StepRolledBack {
		return true
	}
	i := slices.Index(systemOrder, st.Step)
	return i >= 0 && i <= slices.Index(systemOrder, StepHealth)
}

// systemBundle reads the metadata of the system bundle version and returns it with the directory of its files.
func (j *Jobs) systemBundle(ctx context.Context, version, step string) (*bundle.Metadata, string, error) {
	md, err := j.bundle(ctx, release.BundleSystem, version, step)
	if err != nil {
		return nil, "", err
	}
	return md, filepath.Join(j.deps.Dir, release.BundleSystem, tag(version, j.deps.Arch)), nil
}

// managerStep records the manager image that runs and the deadline by which the manager of the bundle must take the
// update over, once, before it applies the manager's manifest of the bundle with its manager image; resumed past that
// deadline, it fails the update as the hand-over does. The manager of the bundle runs once it is applied, so in that
// manager the step is done.
func (j *Jobs) managerStep(ctx context.Context, jb *job) error {
	if j.deps.Version == jb.status.To {
		return nil
	}
	if d := jb.status.Deadline; jb.status.PreviousManager != "" && d != nil && !j.deps.Now().Before(d.Time) {
		return j.managerDidNotStart(ctx, jb)
	}
	md, dir, err := j.systemBundle(ctx, jb.status.To, StepManager)
	if err != nil {
		return err
	}
	if jb.status.PreviousManager == "" {
		previous, err := j.managerImage(ctx)
		if err != nil {
			return fail(StepManager, err)
		}
		jb.status.PreviousManager, jb.status.Deadline = previous, j.handOverDeadline()
		if err := j.record(ctx, jb.status); err != nil {
			return err
		}
	}
	return j.applyManager(ctx, dir, md, StepManager)
}

// applyManager applies the manager's manifest of the bundle md, stored in dir, with its manager image.
func (j *Jobs) applyManager(ctx context.Context, dir string, md *bundle.Metadata, step string) error {
	manifest, err := managerManifest(dir, md)
	if err != nil {
		return &Error{Reason: InvalidBundle, step: step, err: err}
	}
	if err := j.retry(ctx, func() error { return j.deps.Manifests.Apply(ctx, manifest) }); err != nil {
		return fail(step, err)
	}
	return nil
}

// managerManifest is the manager's manifest of the bundle md, stored in dir, with the placeholder its release
// manifest names replaced by the bundle's manager image, as the first boot applies it. Fields of the release manifest
// this manager does not know are left alone.
func managerManifest(dir string, md *bundle.Metadata) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, releaseFile)) //nolint:gosec // built from the verified bundle's version
	if err != nil {
		return nil, fmt.Errorf("read the release manifest: %w", err)
	}
	var rel struct {
		ManagerImage string `json:"managerImage"`
	}
	if err := yaml.Unmarshal(data, &rel); err != nil {
		return nil, fmt.Errorf("read the release manifest: %w", err)
	}
	image, ok := builtImage(md, managerImage)
	if !ok {
		return nil, errors.New("the bundle carries no manager image")
	}
	return firstboot.ManagerManifest(os.DirFS(dir), rel.ManagerImage, image.Ref)
}

// builtImage is the image name built with the appliance that the bundle md carries, <registry>/<name>:<version>-<arch>.
func builtImage(md *bundle.Metadata, name string) (bundle.ImageRef, bool) {
	suffix := "/" + name + ":" + tag(md.Version, md.Arch)
	i := slices.IndexFunc(md.Images, func(im bundle.ImageRef) bool { return strings.HasSuffix(im.Ref, suffix) })
	if i < 0 {
		return bundle.ImageRef{}, false
	}
	return md.Images[i], true
}

// talosStep installs the bundle's installer, at the same Talos version too: the installer carries the release's
// first-boot extension, which applies its manager on every boot. It names the installer in the unattended install
// configuration and reboots the node with a power cycle into it. A failure before the boot ID is recorded hands the
// update back, unless Talos is installed already or the Talos API is unavailable; once recorded, a failure is tried
// again until the deadline, where the Talos the node runs decides. While the node keeps the recorded boot after the
// install, the reboot is issued again every RebootRetry.
func (j *Jobs) talosStep(ctx context.Context, jb *job) error {
	st := &jb.status
	recorded := st.BootID != ""
	if recorded && st.Deadline != nil && !j.deps.Now().Before(st.Deadline.Time) {
		return j.decide(ctx, jb, StepTalos)
	}
	node, md, err := j.installTalos(ctx, jb)
	switch {
	case errors.Is(err, errInterrupted) || errors.Is(err, errHandedOver) || ctx.Err() != nil:
		return err
	case err != nil && (recorded || st.Installed || unavailable(err)):
		return fmt.Errorf("%w: %w", errInterrupted, err)
	case err != nil:
		return j.handBack(ctx, jb, Reason(err), err)
	}
	issued := j.deps.Now()
	err = j.poll(ctx, StepTalos, st.Deadline.Sub(j.deps.Now()), func(ctx context.Context) (bool, error) {
		boot, err := j.runningTalos(ctx, node, st.BootID, md)
		if err == nil && !boot.rebooted && st.Installed {
			j.rebootAgain(ctx, node, &issued)
		}
		return boot.run == talosNew, err
	})
	if Reason(err) == Timeout && ctx.Err() == nil {
		return j.decide(ctx, jb, StepTalos)
	}
	return err
}

// unavailable tells whether err is a Talos API or network failure that may pass.
func unavailable(err error) bool {
	return talos.Interrupted(err) || transient(err)
}

// rebootAgain reboots the node with a power cycle again once RebootRetry passed since the reboot issued at issued,
// as after a reboot call cut off before the node took it.
func (j *Jobs) rebootAgain(ctx context.Context, node TalosNode, issued *time.Time) {
	if j.deps.Now().Sub(*issued) < j.deps.RebootRetry {
		return
	}
	*issued = j.deps.Now()
	j.deps.Log.Warn("the node did not reboot into the new Talos; rebooting it again")
	if err := node.RebootPowercycle(ctx); err != nil && !talos.Interrupted(err) {
		j.deps.Log.Warn("the node refused the reboot", "error", err)
	}
}

// installTalos records the node's boot ID, then names the installer in the unattended install configuration,
// installs it and records it installed with the deadline of the health check, and reboots. After a restart, while the
// node still runs the boot it recorded, it installs again until the install is recorded, and from then on only
// reboots again; the recorded deadline bounds the tries. A node that booted again without the new Talos hands the
// update back.
func (j *Jobs) installTalos(ctx context.Context, jb *job) (TalosNode, *bundle.Metadata, error) {
	md, _, err := j.systemBundle(ctx, jb.status.To, StepTalos)
	if err != nil {
		return nil, nil, err
	}
	installer, ok := builtImage(md, installerImage)
	if !ok {
		return nil, nil, &Error{Reason: ImagesMissing, step: StepTalos, err: errors.New("the bundle carries no installer")}
	}
	node, err := j.talos(ctx, StepTalos)
	if err != nil {
		return nil, nil, err
	}
	st := &jb.status
	if st.BootID == "" {
		if err := j.recordBoot(ctx, st); err != nil {
			return nil, nil, err
		}
	} else if boot, err := j.runningTalos(ctx, node, st.BootID, md); err != nil || boot.rebooted {
		return node, md, j.afterReboot(ctx, jb, boot, err)
	}
	if !st.Installed {
		if err := j.install(ctx, st, node, md, installer.Ref+"@"+installer.Digest); err != nil {
			return nil, nil, err
		}
	}
	j.deps.Log.Info("rebooting the node into the new Talos", "to", st.To)
	if err := node.RebootPowercycle(ctx); err != nil && !talos.Interrupted(err) {
		return nil, nil, fail(StepTalos, err)
	}
	return node, md, nil
}

// afterReboot is the outcome of a restart that finds the node booted again since the recorded boot: the new Talos is
// waited for, the old one hands the update back, and an unknown one is read again later.
func (j *Jobs) afterReboot(ctx context.Context, jb *job, boot talosBoot, err error) error {
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w", errInterrupted, err)
	case boot.run == talosNew:
		return nil
	}
	return j.handBack(ctx, jb, TalosNotRebooted, errors.New("the node booted again without the new Talos"))
}

// install pulls the boot images of the bundle md into the node's system store, names image in the unattended install
// configuration, installs it as the next boot entry and records the install in st with the deadline of the health
// check, taken from now so that the install's time does not shorten it.
func (j *Jobs) install(ctx context.Context, st *v1alpha1.UpdateStatus, node TalosNode, md *bundle.Metadata, image string) error {
	for _, boot := range bootImages(md) {
		j.deps.Log.Info("pulling a boot image into the node's system store", "image", boot)
		if err := node.PullSystemImage(ctx, boot); err != nil {
			return fail(StepTalos, err)
		}
	}
	j.deps.Log.Info("installing Talos", "image", image)
	if err := node.SetInstallerImage(ctx, image); err != nil {
		return fail(StepTalos, err)
	}
	if err := node.Upgrade(ctx, image); err != nil {
		if talos.Interrupted(err) {
			return fmt.Errorf("%w: %w", errInterrupted, err)
		}
		return &Error{Reason: TalosUpgradeRefused, step: StepTalos, err: err}
	}
	deadline := metav1.NewTime(j.deps.Now().Add(j.deps.HealthTimeout))
	st.Installed, st.Deadline = true, &deadline
	return j.record(ctx, *st)
}

// bootImages are the images of the bundle md that Talos runs itself, from its system store, before Kubernetes and the
// appliance's registry are up: etcd, whose image the machine configuration leaves at the Talos default. The kubelet
// image is named by the machine configuration and changes only at the kubernetes step. They are pulled by tag, which
// the images step put in the registry: Talos looks etcd up by its tagged name, and a pull by digest stores the image
// under its digest alone.
func bootImages(md *bundle.Metadata) []string {
	var images []string
	for _, im := range md.Images {
		if strings.HasPrefix(im.Ref, constants.EtcdImage+":") {
			images = append(images, im.Ref)
		}
	}
	return images
}

// talosRun is the Talos a node runs since a boot.
type talosRun int

const (
	talosOld talosRun = iota + 1
	talosNew
)

// talosBoot is what the node runs since a boot: its Talos, whether it booted again and whether it is ready.
type talosBoot struct {
	run      talosRun
	rebooted bool
	ready    bool
}

// runningTalos tells which Talos the node runs since the boot boot. The new Talos is that of the bundle md with its
// first-boot extension, whose version is the appliance's, after another boot; readiness does not count. The node runs
// the old Talos while it reports the boot boot. A node, or a Talos API, node nil included, that cannot be read is an
// error, after which nothing may be decided.
func (j *Jobs) runningTalos(ctx context.Context, node TalosNode, boot string, md *bundle.Metadata) (talosBoot, error) {
	n, err := kube.OwnNode(ctx, j.deps.Client)
	switch {
	case err != nil:
		return talosBoot{}, err
	case n == nil || n.Status.NodeInfo.BootID == "":
		return talosBoot{}, errors.New("the node reports no boot ID")
	case n.Status.NodeInfo.BootID == boot:
		return talosBoot{run: talosOld, ready: nodeReady(n)}, nil
	case node == nil:
		return talosBoot{}, errors.New("no access to the Talos API")
	}
	isNew, err := j.runsNew(ctx, node, md)
	if err != nil {
		return talosBoot{}, err
	}
	b := talosBoot{run: talosOld, rebooted: true, ready: nodeReady(n)}
	if isNew {
		b.run = talosNew
	}
	return b, nil
}

// decide ends the wait for the new Talos at its deadline on what the node runs, as read now: the new Talos, ready
// or not, is rolled back, and the old one hands the update back. What cannot be read is decided on a later try, so
// that an old Talos is never rolled back, which would boot the new one, and the previous manager never runs on the
// new Talos.
func (j *Jobs) decide(ctx context.Context, jb *job, step string) error {
	md, _, err := j.systemBundle(ctx, jb.status.To, step)
	if err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	node, err := j.deps.Talos(ctx)
	if err != nil {
		node = nil
	}
	boot, rerr := j.runningTalos(ctx, node, jb.status.BootID, md)
	switch {
	case rerr != nil:
		j.deps.Log.Warn("cannot tell which Talos the node runs; deciding later", "step", step, "error", errors.Join(err, rerr))
		return fmt.Errorf("%w: %w", errInterrupted, rerr)
	case boot.run == talosNew:
		j.deps.Log.Warn("the node did not come back healthy on the new Talos; rolling Talos back", "to", jb.status.To)
		return j.rollBackTalos(ctx, jb)
	}
	if jb.status.Installed && !boot.rebooted {
		return j.handBack(ctx, jb, TalosInstalledNotRebooted, errors.New("the node did not reboot into the installed Talos"))
	}
	return j.handBack(ctx, jb, TalosNotRebooted, errors.New("the node does not run the new Talos"))
}

// runsNew tells whether the node runs the Talos of the bundle md with its first-boot extension.
func (j *Jobs) runsNew(ctx context.Context, node TalosNode, md *bundle.Metadata) (bool, error) {
	version, _, err := node.Versions(ctx)
	if err != nil {
		return false, err
	}
	extension, err := node.ExtensionVersion(ctx, talos.FirstbootExtension)
	if err != nil {
		return false, err
	}
	return version == md.Talos && extension == md.Version, nil
}

// recordBoot records the boot ID of the node in st with a deadline for the install, which the install's record
// replaces with the deadline of the health check.
func (j *Jobs) recordBoot(ctx context.Context, st *v1alpha1.UpdateStatus) error {
	boot, err := j.bootID(ctx, StepTalos)
	if err != nil {
		return err
	}
	deadline := metav1.NewTime(j.deps.Now().Add(j.deps.HealthTimeout))
	st.BootID, st.Deadline = boot, &deadline
	return j.record(ctx, *st)
}

func (j *Jobs) talos(ctx context.Context, step string) (TalosNode, error) {
	node, err := j.deps.Talos(ctx)
	if err != nil {
		return nil, fail(step, err)
	}
	return node, nil
}

func (j *Jobs) bootID(ctx context.Context, step string) (string, error) {
	var node *corev1.Node
	err := j.retry(ctx, func() (err error) {
		node, err = kube.OwnNode(ctx, j.deps.Client)
		return err
	})
	switch {
	case err != nil:
		return "", fail(step, err)
	case node == nil || node.Status.NodeInfo.BootID == "":
		return "", &Error{Reason: Failed, step: step, err: errors.New("the node reports no boot ID")}
	}
	return node.Status.NodeInfo.BootID, nil
}

// rebooted tells whether the node booted since the boot boot, and is ready.
func (j *Jobs) rebooted(ctx context.Context, boot string) (bool, error) {
	node, err := kube.OwnNode(ctx, j.deps.Client)
	if err != nil || node == nil {
		return false, err
	}
	return node.Status.NodeInfo.BootID != boot && nodeReady(node), nil
}

func nodeReady(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
		return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
	})
}

// healthStep waits, until the recorded deadline, for the node to come back from the reboot into the new Talos
// ready, with the add-ons and the platform ready and the console answering. Past the deadline the Talos the node
// runs decides between a rollback and a hand-back. What cannot be read is read again. A healthy node passes the point
// of no return: Talos is no longer rolled back.
func (j *Jobs) healthStep(ctx context.Context, jb *job) error {
	st := jb.status
	switch {
	case st.PointOfNoReturn:
		// The health check passed before a restart.
		return nil
	case st.Deadline == nil:
		return &Error{Reason: Failed, step: StepHealth, err: errors.New("no health check deadline recorded")}
	}
	md, _, err := j.systemBundle(ctx, st.To, StepHealth)
	if err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	rechecked := false
	err = j.poll(ctx, StepHealth, st.Deadline.Sub(j.deps.Now()), func(ctx context.Context) (bool, error) {
		node, err := j.deps.Talos(ctx)
		if err != nil {
			return false, err
		}
		boot, err := j.runningTalos(ctx, node, st.BootID, md)
		if err != nil || boot.run != talosNew || !boot.ready {
			return false, err
		}
		return j.healthy(ctx, &rechecked)
	})
	switch {
	case Reason(err) == Timeout && ctx.Err() == nil:
		return j.decide(ctx, jb, StepHealth)
	case err != nil:
		return err
	}
	return j.pointOfNoReturn(ctx, jb)
}

// pointOfNoReturn records that the update jb can no longer be undone by a Talos rollback, once.
func (j *Jobs) pointOfNoReturn(ctx context.Context, jb *job) error {
	if jb.status.PointOfNoReturn {
		return nil
	}
	jb.status.PointOfNoReturn = true
	return j.record(ctx, jb.status)
}

// healthy tells whether the node that booted the new Talos is healthy. On its first call it marks the conditions
// reported before the reboot for the reconciler to report again, then waits for them.
func (j *Jobs) healthy(ctx context.Context, rechecked *bool) (bool, error) {
	if !*rechecked {
		if err := j.recheck(ctx, "the node rebooted", v1alpha1.ConditionAddonsReady, v1alpha1.ConditionPlatformReady); err != nil {
			return false, err
		}
		*rechecked = true
		return false, nil
	}
	a, err := j.appliance(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range []string{v1alpha1.ConditionAddonsReady, v1alpha1.ConditionPlatformReady} {
		if !meta.IsStatusConditionTrue(a.Status.Conditions, c) {
			return false, nil
		}
	}
	if err := j.deps.Console(ctx); err != nil {
		return false, fmt.Errorf("the console does not answer: %w", err)
	}
	return true, nil
}

// recheck marks the conditions types unknown, for the reconciler to report them again.
func (j *Jobs) recheck(ctx context.Context, message string, types ...string) error {
	return updateStatus(ctx, j.deps.Client, func(s *v1alpha1.ApplianceStatus) {
		for _, t := range types {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type: t, Status: metav1.ConditionUnknown, Reason: reasonRechecking, Message: message,
			})
		}
	})
}

// rollBackTalos records the rollback with the current boot ID and rolls Talos back, which reboots the node into the
// old Talos. A rollback call cut off may have been carried out: the boot decides then.
func (j *Jobs) rollBackTalos(ctx context.Context, jb *job) error {
	boot, err := j.bootID(ctx, StepHealth)
	if err != nil {
		return err
	}
	node, err := j.talos(ctx, StepHealth)
	if err != nil {
		return err
	}
	next := jb.status
	next.Step, next.BootID = StepRolledBack, boot
	deadline := metav1.NewTime(j.deps.Now().Add(j.deps.HealthTimeout))
	next.Deadline = &deadline
	if err := j.record(ctx, next); err != nil {
		return err
	}
	jb.status = next
	if err := node.Rollback(ctx); err != nil {
		if !talos.Interrupted(err) {
			return &Error{Reason: TalosRollbackFailed, step: StepHealth, err: err}
		}
		j.deps.Log.Warn("the Talos rollback call was cut off; waiting for the node to boot", "error", err)
	}
	return j.rolledBackStep(ctx, jb)
}

// rolledBackStep fails the update the failed health check rolled back once the node booted again, ready, and runs
// the old Talos; one that booted the new Talos again fails it as a rollback that did not take. It never rolls back
// again, which would switch the boot entry back: past the recorded deadline the Talos the node runs decides. What
// cannot be read is read again.
func (j *Jobs) rolledBackStep(ctx context.Context, jb *job) error {
	var limit time.Duration
	if d := jb.status.Deadline; d != nil {
		limit = d.Sub(j.deps.Now())
	}
	md, _, err := j.systemBundle(ctx, jb.status.To, StepRolledBack)
	if err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	err = j.poll(ctx, StepRolledBack, limit, func(ctx context.Context) (bool, error) {
		return j.rolledBack(ctx, jb.status.BootID, md)
	})
	switch {
	case Reason(err) == Timeout && ctx.Err() == nil:
		return j.decideRollback(ctx, jb)
	case err != nil:
		return err
	}
	return &Error{Reason: HealthCheckFailed, step: StepHealth}
}

// rolledBack tells whether the node booted since the boot boot, ready, into the old Talos; the new Talos is a rollback
// that did not take.
func (j *Jobs) rolledBack(ctx context.Context, boot string, md *bundle.Metadata) (bool, error) {
	rebooted, err := j.rebooted(ctx, boot)
	if err != nil || !rebooted {
		return false, err
	}
	node, err := j.deps.Talos(ctx)
	if err != nil {
		return false, err
	}
	isNew, err := j.runsNew(ctx, node, md)
	switch {
	case err != nil:
		return false, err
	case isNew:
		return false, &Error{Reason: TalosRollbackFailed, step: StepHealth, err: errors.New("the node booted the new Talos again")}
	}
	return true, nil
}

// decideRollback ends the wait for the rollback of Talos at its deadline on the Talos the node runs, as read now:
// the old Talos fails the update as rolled back, the new one as a rollback that did not take. What cannot be read is
// decided on a later try.
func (j *Jobs) decideRollback(ctx context.Context, jb *job) error {
	md, _, err := j.systemBundle(ctx, jb.status.To, StepRolledBack)
	if err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	node, err := j.deps.Talos(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	isNew, err := j.runsNew(ctx, node, md)
	switch {
	case err != nil:
		j.deps.Log.Warn("cannot tell which Talos the node runs after the rollback; deciding later", "error", err)
		return fmt.Errorf("%w: %w", errInterrupted, err)
	case isNew:
		return &Error{Reason: TalosRollbackFailed, step: StepHealth, err: errors.New("the node runs the new Talos")}
	}
	return &Error{Reason: HealthCheckFailed, step: StepHealth}
}

// kubernetesStep moves Kubernetes to the bundle's version one component after the other, each unless it runs at that
// version already, and waits for each to run healthy. What runs decides, never the machine configuration. A Talos call
// that fails for a reason that may pass is tried again once resumed.
func (j *Jobs) kubernetesStep(ctx context.Context, jb *job) error {
	md, _, err := j.systemBundle(ctx, jb.status.To, StepKubernetes)
	if err != nil {
		return err
	}
	running, err := j.kubeletVersion(ctx)
	switch {
	case err != nil:
		return fail(StepKubernetes, err)
	case older(md.Kubernetes, running):
		return &Error{Reason: KubernetesOlderThanRunning, step: StepKubernetes}
	}
	images, err := componentImages(md)
	if err != nil {
		return err
	}
	if err := j.pointOfNoReturn(ctx, jb); err != nil {
		return err
	}
	var node TalosNode
	for i, c := range kubernetesComponents {
		if err := j.moveComponent(ctx, &node, c, images[i], md.Kubernetes); err != nil {
			return err
		}
	}
	return nil
}

// kubeletVersion is the version of the kubelet the node runs.
func (j *Jobs) kubeletVersion(ctx context.Context) (string, error) {
	var node *corev1.Node
	err := j.retry(ctx, func() (err error) {
		node, err = kube.OwnNode(ctx, j.deps.Client)
		return err
	})
	switch {
	case err != nil:
		return "", err
	case node == nil:
		return "", errors.New("the node is not registered")
	}
	return node.Status.NodeInfo.KubeletVersion, nil
}

// setComponent sets the image of the component c through the Talos API node, reached first when nil.
func (j *Jobs) setComponent(ctx context.Context, node *TalosNode, c kubernetesComponent, image string) error {
	if *node == nil {
		n, err := j.deps.Talos(ctx)
		if err != nil {
			return kubernetesFailure(err)
		}
		*node = n
	}
	j.deps.Log.Info("moving a Kubernetes component", "component", c.name, "image", image)
	if err := (*node).SetKubernetesImages(ctx, c.name, image); err != nil {
		return kubernetesFailure(err)
	}
	return nil
}

// kubernetesFailure is a failed Talos call of the kubernetes step: tried again once resumed when it may pass, else
// the update's failure.
func kubernetesFailure(err error) error {
	if unavailable(err) {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	return fail(StepKubernetes, err)
}

// componentImages are the images of kubernetesComponents at the Kubernetes version of the bundle md, which carries
// them.
func componentImages(md *bundle.Metadata) ([]string, error) {
	images := make([]string, 0, len(kubernetesComponents))
	for _, c := range kubernetesComponents {
		ref := c.repository + ":" + md.Kubernetes
		if !slices.ContainsFunc(md.Images, func(im bundle.ImageRef) bool { return im.Ref == ref }) {
			return nil, &Error{Reason: ImagesMissing, step: StepKubernetes, err: fmt.Errorf("the bundle carries no %s", ref)}
		}
		images = append(images, ref)
	}
	return images, nil
}

// moveComponent sets the image of the component c, unless it runs it already, and waits for it to run healthy at
// version. The Talos API, node, is reached once a component needs it.
func (j *Jobs) moveComponent(ctx context.Context, node *TalosNode, c kubernetesComponent, image, version string) error {
	var at bool
	err := j.retry(ctx, func() (err error) {
		at, _, err = j.component(ctx, c, image, version)
		return err
	})
	if err != nil {
		return fail(StepKubernetes, err)
	}
	if !at {
		if err := j.setComponent(ctx, node, c, image); err != nil {
			return err
		}
	}
	return j.poll(ctx, StepKubernetes, j.deps.ComponentTimeout, func(ctx context.Context) (bool, error) {
		at, healthy, err := j.component(ctx, c, image, version)
		return at && healthy, err
	})
}

// component tells whether the component c runs image, the kubelet version, and whether it is healthy: its static
// pod ready, or the node ready.
func (j *Jobs) component(ctx context.Context, c kubernetesComponent, image, version string) (at, healthy bool, err error) {
	if c.app == "" {
		node, err := kube.OwnNode(ctx, j.deps.Client)
		if err != nil || node == nil {
			return false, false, err
		}
		return node.Status.NodeInfo.KubeletVersion == version, nodeReady(node), nil
	}
	pods := &corev1.PodList{}
	if err := j.deps.Client.List(ctx, pods, client.InNamespace(metav1.NamespaceSystem), client.MatchingLabels{"k8s-app": c.app}); err != nil {
		return false, false, err
	}
	for _, p := range pods.Items {
		if len(p.Spec.Containers) > 0 && p.Spec.Containers[0].Image == image {
			return true, podReady(&p), nil
		}
	}
	return false, false, nil
}

func podReady(p *corev1.Pod) bool {
	return slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// addonsStep releases the add-on pass, which the step's being recorded does, and waits for the pass of the new
// release to finish.
func (j *Jobs) addonsStep(ctx context.Context, _ *job) error {
	if err := j.recheck(ctx, "the add-on pass of the new release runs", v1alpha1.ConditionAddonsReady); err != nil {
		return fail(StepAddons, err)
	}
	return j.poll(ctx, StepAddons, j.deps.WaitTimeout, func(ctx context.Context) (bool, error) {
		a, err := j.appliance(ctx)
		if err != nil {
			return false, err
		}
		c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionAddonsReady)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason != v1alpha1.ReasonUpdateHeld &&
			c.ObservedGeneration == a.Generation, nil
	})
}
