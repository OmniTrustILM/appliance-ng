package update

import (
	"context"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// The manager's Deployment and its container.
const (
	managerDeployment = "appliance-manager"
	managerContainer  = "manager"
)

// Reasons a system update fails before the node booted the new Talos. Talos and Kubernetes are as they were, and
// the manager of the running release records the failure.
const (
	// ManagerDidNotStart is a manager of the new release that did not take the update over in time.
	ManagerDidNotStart = "managerDidNotStart"
	// TalosUpgradeRefused is a Talos upgrade the node refused.
	TalosUpgradeRefused = "talosUpgradeRefused"
	// TalosNotRebooted is a node that did not boot the new Talos in time. The installed boot entry may stay the
	// default and boot the new Talos on a later reboot, without the health check of the update.
	TalosNotRebooted = "talosNotRebooted"
	// TalosInstalledNotRebooted is a new Talos installed as the next boot entry that the node did not reboot into in
	// time: the next reboot boots it, without the health check of the update.
	TalosInstalledNotRebooted = "talosInstalledNotRebooted"
)

// errHandedOver stops a manager at a step another manager runs; it is no failure.
var errHandedOver = errors.New("the update is with the other manager")

// stepFunc is what runs step of jb in this manager: the end of an update handed back, the part of the manager of the
// running release past the hand-over, or the step itself.
func (j *Jobs) stepFunc(jb *job, step string) (func(j *Jobs, ctx context.Context, jb *job) error, bool) {
	st := jb.status
	if st.Kind == release.BundleSystem {
		switch {
		case st.HandBack != "":
			return (*Jobs).handedBack, true
		case j.deps.Version != st.To && pastManager(step):
			return (*Jobs).oldManagerStep, true
		}
	}
	run, ok := stepTables[st.Kind].run[step]
	return run, ok
}

// pastManager tells whether step comes after the manager step of a system update.
func pastManager(step string) bool {
	return step == StepRolledBack || slices.Index(systemOrder, step) > slices.Index(systemOrder, StepManager)
}

// oldManagerStep is what the manager of the running release does past the hand-over: it waits for the new manager
// to take the update over, ends the rollback of Talos, which runs it again, and fails the update when the node
// booted without the new Talos, whose first-boot extension would have run the new manager. Every other step is the
// new manager's.
func (j *Jobs) oldManagerStep(ctx context.Context, jb *job) error {
	st := jb.status
	switch {
	case st.Step == StepRolledBack:
		return j.rolledBackStep(ctx, jb)
	case st.Step == StepTalos && st.BootID == "":
		return j.awaitHandOver(ctx, jb)
	case st.Step != StepTalos && st.Step != StepHealth:
		return errHandedOver
	}
	md, _, err := j.systemBundle(ctx, st.To, st.Step)
	if err != nil {
		return errHandedOver
	}
	node, err := j.deps.Talos(ctx)
	if err != nil {
		return errHandedOver
	}
	if boot, err := j.runningTalos(ctx, node, st.BootID, md); err != nil || !boot.rebooted || boot.run != talosOld {
		return errHandedOver
	}
	return &Error{Reason: TalosNotRebooted, step: st.Step}
}

// awaitHandOver waits, in the manager of the running release, for the manager of the new release to take the update
// over at the talos step. A reboot of the old Talos runs the old manager again, so before the recorded deadline it
// applies the new manager again; past it the update fails, with the old manager kept.
func (j *Jobs) awaitHandOver(ctx context.Context, jb *job) error {
	st := jb.status
	j.deps.Log.Info("the update waits for the manager of the new release", "to", st.To, "deadline", st.Deadline)
	if st.Deadline == nil || !j.deps.Now().Before(st.Deadline.Time) {
		return j.managerDidNotStart(ctx, jb)
	}
	if err := j.ensureNewManager(ctx, st.To); err != nil {
		return err
	}
	err := j.poll(ctx, StepTalos, st.Deadline.Sub(j.deps.Now()), func(ctx context.Context) (bool, error) {
		a, err := j.appliance(ctx)
		if err != nil {
			return false, err
		}
		return !awaitsHandOver(a.Status.Update, st), nil
	})
	switch {
	case Reason(err) == Timeout && ctx.Err() == nil:
		return j.managerDidNotStart(ctx, jb)
	case err != nil:
		return err
	}
	return errHandedOver
}

// awaitsHandOver tells whether u is still the update st waiting for the new manager.
func awaitsHandOver(u *v1alpha1.UpdateStatus, st v1alpha1.UpdateStatus) bool {
	// The status keeps the start in seconds.
	return u.Unfinished() && u.To == st.To && u.StartedAt.Unix() == st.StartedAt.Unix() && u.Step == StepTalos &&
		u.BootID == "" && u.HandBack == ""
}

// managerDidNotStart fails the update whose new manager did not take it over and keeps the old manager running.
func (j *Jobs) managerDidNotStart(ctx context.Context, jb *job) error {
	if previous := jb.status.PreviousManager; previous != "" {
		if err := j.setManagerImage(ctx, previous); err != nil {
			return fmt.Errorf("%w: %w", errInterrupted, err)
		}
	}
	return &Error{Reason: ManagerDidNotStart, step: StepTalos}
}

// ensureNewManager applies the manager's manifest of the system bundle to again unless the manager's Deployment runs
// the bundle's manager.
func (j *Jobs) ensureNewManager(ctx context.Context, to string) error {
	md, dir, err := j.systemBundle(ctx, to, StepTalos)
	if err != nil {
		return err
	}
	image, ok := builtImage(md, managerImage)
	if !ok {
		return &Error{Reason: InvalidBundle, step: StepTalos, err: errors.New("the bundle carries no manager image")}
	}
	running, err := j.managerImage(ctx)
	if err != nil {
		return fail(StepTalos, err)
	}
	if running == image.Ref {
		return nil
	}
	j.deps.Log.Info("applying the manager of the new release again", "to", to)
	return j.applyManager(ctx, dir, md, StepTalos)
}

// handBack hands the update back to the previous manager once it failed for reason before the node booted the new
// Talos: it records reason, which the previous manager records as the failure, and runs the previous manager again.
// The add-on pass stays held until then, so that the add-ons are never those of the new release on the old Talos.
func (j *Jobs) handBack(ctx context.Context, jb *job, reason string, cause error) error {
	j.deps.Log.Warn("the update failed before the node booted the new Talos; handing it back to the previous manager",
		"to", jb.status.To, "reason", reason, "error", cause)
	if jb.status.PreviousManager == "" {
		return &Error{Reason: reason, step: jb.status.Step, err: cause}
	}
	jb.status.HandBack = reason
	if err := j.record(ctx, jb.status); err != nil {
		// Never the update's failure: that would release the add-on pass on the old Talos. It is handed back again
		// once resumed.
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	return j.handedBack(ctx, jb)
}

// handedBack ends an update handed back: the previous manager records the failure, any other runs the previous
// manager again.
func (j *Jobs) handedBack(ctx context.Context, jb *job) error {
	st := jb.status
	if j.deps.Version != st.To {
		return &Error{Reason: st.HandBack, step: st.Step}
	}
	if err := j.setManagerImage(ctx, st.PreviousManager); err != nil {
		return fmt.Errorf("%w: %w", errInterrupted, err)
	}
	return errHandedOver
}

func (j *Jobs) managerImage(ctx context.Context) (string, error) {
	d := &appsv1.Deployment{}
	if err := j.read(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: managerDeployment}, d); err != nil {
		return "", err
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == managerContainer {
			return c.Image, nil
		}
	}
	return "", errors.New("the manager's Deployment has no manager container")
}

func (j *Jobs) setManagerImage(ctx context.Context, image string) error {
	return j.retry(ctx, func() error {
		d := &appsv1.Deployment{}
		if err := j.deps.Client.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: managerDeployment}, d); err != nil {
			return err
		}
		patch := client.MergeFrom(d.DeepCopy())
		for i := range d.Spec.Template.Spec.Containers {
			if d.Spec.Template.Spec.Containers[i].Name == managerContainer {
				d.Spec.Template.Spec.Containers[i].Image = image
			}
		}
		return j.deps.Client.Patch(ctx, d, patch, client.FieldOwner(kube.ManagerFieldManager))
	})
}

// handOverDeadline is when the manager of the new release must have taken the update over.
func (j *Jobs) handOverDeadline() *metav1.Time {
	deadline := metav1.NewTime(j.deps.Now().Add(j.deps.HandOverTimeout))
	return &deadline
}
