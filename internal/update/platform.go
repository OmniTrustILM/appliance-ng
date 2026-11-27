package update

import (
	"context"
	"errors"
	"io/fs"
	"slices"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// The Platform conditions the update reads, and the reasons with which the operator refuses a version.
const (
	platformAvailable = "Available"
	platformDegraded  = "Degraded"
)

var rejectedVersion = []string{"DowngradeForbidden", "UnsupportedVersion"}

// stepTable is the order of the steps of a kind of update and what runs each; each step can run again after a
// restart.
type stepTable struct {
	order []string
	run   map[string]func(j *Jobs, ctx context.Context, jb *job) error
}

// next is the step after step.
func (t *stepTable) next(step string) string {
	return t.order[slices.Index(t.order, step)+1]
}

// stepTables are the steps of each kind of update.
var stepTables = map[string]*stepTable{
	release.BundlePlatform: {
		order: []string{StepBackup, StepImages, StepOperator, StepVersion, StepWait, StepDone},
		run: map[string]func(j *Jobs, ctx context.Context, jb *job) error{
			StepBackup:   (*Jobs).backupStep,
			StepImages:   (*Jobs).imagesStep,
			StepOperator: (*Jobs).operatorStep,
			StepVersion:  (*Jobs).versionStep,
			StepWait:     (*Jobs).waitStep,
		},
	},
	release.BundleSystem: {order: systemOrder, run: systemSteps},
}

// backupStep takes the backup to every target of the Appliance and pins its local copy until the next update of its
// kind ends. A backup without the database is no way back from an update that migrates it.
func (j *Jobs) backupStep(ctx context.Context, jb *job) error {
	a, err := j.readAppliance(ctx, StepBackup)
	if err != nil {
		return err
	}
	var targets []string
	if a.Spec.Backup != nil {
		for _, t := range a.Spec.Backup.Targets {
			targets = append(targets, t.Name)
		}
	}
	rec, err := j.deps.Backups.Run(ctx, backup.RunUpdate, targets, backup.Pin(jb.status.Kind), backup.ByUser(jb.user))
	if err != nil {
		return &Error{Reason: BackupFailed, step: StepBackup, err: err}
	}
	if rec.Database != v1alpha1.BackupDatabaseIncluded {
		return &Error{Reason: BackupIncomplete, step: StepBackup, err: errNoDatabase}
	}
	jb.status.Backup = rec.Name
	return nil
}

// readAppliance reads the Appliance for step, trying again on a transient failure.
func (j *Jobs) readAppliance(ctx context.Context, step string) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	if err := j.read(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, fail(step, err)
	}
	return a, nil
}

// imagesStep makes sure the registry holds every image of the bundle, fetching the bundle online when it is not
// loaded, and tags each under its upstream tag, which workloads pull through the registry mirror.
func (j *Jobs) imagesStep(ctx context.Context, jb *job) error {
	md, err := j.bundle(ctx, jb.status.Kind, jb.status.To, StepImages)
	if err != nil {
		return err
	}
	reg, err := j.deps.Registry(ctx)
	if err != nil {
		return fail(StepImages, err)
	}
	if err := reg.TagImages(ctx, md.Images); err != nil {
		return &Error{Reason: ImagesMissing, step: StepImages, err: err}
	}
	return nil
}

// bundle reads the metadata of the bundle of kind and version, fetching the bundle online when it is not loaded.
func (j *Jobs) bundle(ctx context.Context, kind, version, step string) (*bundle.Metadata, error) {
	md, err := installed(j.deps.Dir, kind, version, j.deps.Arch)
	if errors.Is(err, fs.ErrNotExist) {
		if ferr := j.deps.Fetcher.Fetch(ctx, kind, version); ferr != nil {
			return nil, &Error{Reason: BundleUnavailable, step: step, err: ferr}
		}
		md, err = installed(j.deps.Dir, kind, version, j.deps.Arch)
	}
	if err != nil {
		return nil, &Error{Reason: BundleUnavailable, step: step, err: err}
	}
	return md, nil
}

// operatorStep records the bundle as the platform bundle, from which the add-on pass applies its ILM operator when it
// is newer than the embedded one, and waits for that operator to roll out.
func (j *Jobs) operatorStep(ctx context.Context, jb *job) error {
	md, err := j.bundle(ctx, release.BundlePlatform, jb.status.To, StepOperator)
	if err != nil {
		return err
	}
	if err := j.writeStatus(ctx, func(s *v1alpha1.ApplianceStatus) { s.Versions.PlatformBundle = md.Version }); err != nil {
		return err
	}
	if !bundleOperator(j.embeddedOperator(), md) {
		return nil
	}
	return j.poll(ctx, StepOperator, j.deps.OperatorTimeout, func(ctx context.Context) (bool, error) {
		return j.operatorRolledOut(ctx, md.Operator)
	})
}

// embeddedOperator is the version of the ILM operator the release embeds.
func (j *Jobs) embeddedOperator() string {
	embedded, _ := j.deps.Release.Operator()
	return embedded.Version
}

// operatorRolledOut tells whether the add-on pass applied the ILM operator version and its Deployment rolled out.
func (j *Jobs) operatorRolledOut(ctx context.Context, version string) (bool, error) {
	a, err := j.appliance(ctx)
	if err != nil || a.Status.Versions.Operator != version {
		return false, err
	}
	d := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: platform.OperatorNamespace, Name: platform.OperatorDeployment}
	if err := j.deps.Client.Get(ctx, key, d); err != nil {
		return false, err
	}
	return rolledOut(d), nil
}

// rolledOut tells whether every replica of d runs its current template and is available.
func rolledOut(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	return s.ObservedGeneration >= d.Generation && s.Replicas == want && s.UpdatedReplicas == want && s.AvailableReplicas == want
}

// versionStep asks for the bundle's ILM version, unless the operator is upgrading the platform. From here the update
// can only be rolled back.
func (j *Jobs) versionStep(ctx context.Context, jb *job) error {
	md, err := j.bundle(ctx, release.BundlePlatform, jb.status.To, StepVersion)
	if err != nil {
		return err
	}
	a, err := j.readAppliance(ctx, StepVersion)
	if err != nil {
		return err
	}
	if a.Spec.Platform == nil {
		return &Error{Reason: NotInstalled, step: StepVersion}
	}
	if a.Spec.Platform.ILMVersion != md.ILM {
		if err := j.setVersion(ctx, a, md.ILM); err != nil {
			return err
		}
	}
	jb.status.PointOfNoReturn = true
	return nil
}

func (j *Jobs) setVersion(ctx context.Context, a *v1alpha1.Appliance, version string) error {
	var upgrading bool
	err := j.retry(ctx, func() (err error) {
		upgrading, err = backup.Upgrading(ctx, j.deps.Client)
		return err
	})
	if err != nil {
		return fail(StepVersion, err)
	}
	if upgrading {
		return &Error{Reason: PlatformUpgrading, step: StepVersion, err: errPlatformUpgrading}
	}
	patch := client.MergeFrom(a.DeepCopy())
	a.Spec.Platform.ILMVersion = version
	err = j.retry(ctx, func() error {
		return j.deps.Client.Patch(ctx, a, patch, client.FieldOwner(kube.ManagerFieldManager))
	})
	if err != nil {
		return fail(StepVersion, err)
	}
	return nil
}

// waitStep waits until the operator runs the bundle's ILM version and the chosen connectors are ready, and fails at
// once when the operator rejects the version.
func (j *Jobs) waitStep(ctx context.Context, jb *job) error {
	md, err := j.bundle(ctx, release.BundlePlatform, jb.status.To, StepWait)
	if err != nil {
		return err
	}
	return j.poll(ctx, StepWait, j.deps.WaitTimeout, func(ctx context.Context) (bool, error) {
		return j.platformRuns(ctx, md.ILM)
	})
}

// platformRuns tells whether the Platform runs the ILM version, available, and the Appliance reports its connectors
// ready since its latest change; a rejected version is VersionRejected.
func (j *Jobs) platformRuns(ctx context.Context, version string) (bool, error) {
	p := &otilm.Platform{}
	if err := j.deps.Client.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: platform.Name}, p); err != nil {
		return false, err
	}
	if p.Spec.Version != version || p.Status.ObservedGeneration != p.Generation {
		return false, nil
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, platformDegraded); c != nil && c.Status == metav1.ConditionTrue &&
		slices.Contains(rejectedVersion, c.Reason) {
		return false, &Error{Reason: VersionRejected, step: StepWait, err: errors.New(c.Reason)}
	}
	if p.Status.ObservedVersion != version || !meta.IsStatusConditionTrue(p.Status.Conditions, platformAvailable) {
		return false, nil
	}
	a, err := j.appliance(ctx)
	if err != nil {
		return false, err
	}
	c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionConnectorsReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == a.Generation, nil
}
