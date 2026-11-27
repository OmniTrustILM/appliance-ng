package update

import (
	"context"
	"errors"
	"io/fs"
	"slices"

	"filippo.io/age"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
)

// Reasons a rollback is refused, besides those of an update.
const (
	// NoPreUpdateBackup: the last update is not a platform update, or the backup taken before it is gone.
	NoPreUpdateBackup = "noPreUpdateBackup"
	// NoPreviousBundle: neither the platform bundle that ran before the update nor the release's own platform is
	// at hand.
	NoPreviousBundle = "noPreviousBundle"
	// UpgradeInProgress: the ILM operator upgrades the platform.
	UpgradeInProgress = "upgradeInProgress"
	// OperatorChanged: the update installed a newer ILM operator, which is never downgraded; the way back is a
	// restore onto a fresh appliance.
	OperatorChanged = "operatorChanged"
	// WrongKit: the recovery kit does not open the backup.
	WrongKit = "wrongKit"
	// BackupBroken: the backup is damaged, too new or holds another ILM version than the one that ran before.
	BackupBroken = "backupBroken"
	// NotNeeded: the update failed before its point of no return and never asked for the new ILM version; a rollback
	// would only lose the data changed since its backup.
	NotNeeded = "notNeeded"
	// ExternalDatabase: the platform uses an external database, which the old ILM would meet with the newer schema
	// before the backup is loaded.
	ExternalDatabase = "externalDatabase"
)

// KindRollback is the kind of a rollback in status.update; its steps are those of the restore in status.restore.
const KindRollback = v1alpha1.UpdateKindRollback

// AuditRollbackStart is the audit action of a started rollback; the restore records its end.
const AuditRollbackStart = "platform-rollback-start"

// Restorer starts a restore of a backup file in the background.
type Restorer interface {
	Start(ctx context.Context, file string, id age.Identity, opts restore.Options) error
}

// stepRollback names what failed in a rollback refused for another reason.
const stepRollback = "rollback"

// Rollback rolls the last platform update back, whether it finished or failed: the restore engine uninstalls the
// platform, installs it again at the ILM version of the platform bundle that ran before, or of the release's own
// platform, and restores the backup taken before the update with id, the recovery kit's backup key. The shared
// admission is held until the restore is recorded, from which on the restore keeps the other long operations out. It
// refuses with an *Error while another long operation runs, without the pinned backup or the previous platform, when
// the update installed a newer ILM operator, while the operator upgrades the platform, and when the kit does not
// open the backup or the backup cannot be restored.
func (j *Jobs) Rollback(ctx context.Context, id age.Identity, user string) error {
	if !j.begin() {
		return &Error{Reason: UpdateRunning, err: errUpdateRunning}
	}
	defer j.end()
	release, err := j.deps.Backups.Hold()
	if err != nil {
		return j.holdRefusal(ctx, err)
	}
	defer release()
	file, opts, err := j.rollbackPlan(ctx, user)
	if err != nil {
		return err
	}
	if err := j.deps.Restore.Start(ctx, file, id, opts); err != nil {
		return restoreRefusal(err)
	}
	u := opts.Rollback.Update
	j.auditEntry(user, AuditRollbackStart, auditOK, u, "")
	j.deps.Log.Info("platform rollback started", "from", u.From, "to", u.To, "ilm", opts.Rollback.Version)
	return nil
}

// RollbackRefused is the fixed reason the last platform update cannot be rolled back now, as Rollback would refuse
// it without the kit; empty when it can. It neither takes the admission nor starts anything.
func (j *Jobs) RollbackRefused(ctx context.Context) string {
	if _, _, err := j.rollbackPlan(ctx, ""); err != nil {
		return Reason(err)
	}
	return ""
}

// rollbackPlan is the backup file and the restore options of the rollback of the last platform update by user.
func (j *Jobs) rollbackPlan(ctx context.Context, user string) (string, restore.Options, error) {
	a, err := j.appliance(ctx)
	if err != nil {
		return "", restore.Options{}, fail(stepRollback, err)
	}
	u, err := lastPlatformUpdate(a)
	if err != nil {
		return "", restore.Options{}, err
	}
	file, err := j.pinnedBackup(u.Backup)
	if err != nil {
		return "", restore.Options{}, err
	}
	prev, err := j.previous(a, u.From, u.FromILM)
	if err != nil {
		return "", restore.Options{}, err
	}
	upgrading, err := backup.Upgrading(ctx, j.deps.Client)
	if err != nil {
		return "", restore.Options{}, fail(stepRollback, err)
	}
	if upgrading {
		return "", restore.Options{}, &Error{Reason: UpgradeInProgress, err: errPlatformUpgrading}
	}
	return file, restore.Options{InPlace: true, User: user, Rollback: &restore.Rollback{
		Version: prev.ilm, PlatformBundle: prev.bundle,
		Update: v1alpha1.UpdateStatus{
			Kind: KindRollback, From: u.To, To: u.From, FromILM: u.FromILM, Backup: u.Backup,
			StartedAt: metav1.NewTime(j.deps.Now()),
		},
	}}, nil
}

// lastPlatformUpdate is the platform update a rollback goes back from: the last update, finished or failed past its
// point of no return, or the update an abandoned rollback went back from, whose From and To it swapped.
func lastPlatformUpdate(a *v1alpha1.Appliance) (*v1alpha1.UpdateStatus, error) {
	u := a.Status.Update
	switch {
	case a.Status.Restore.Unsettled():
		return nil, &Error{Reason: RestoreRunning, err: errRestoreRunning}
	case a.Spec.Platform != nil && a.Spec.Platform.Database != nil:
		return nil, &Error{Reason: ExternalDatabase}
	case u.Unfinished():
		return nil, &Error{Reason: UpdateRunning, err: errUpdateRunning}
	case abandonedRollback(a):
		return &v1alpha1.UpdateStatus{Kind: release.BundlePlatform, From: u.To, To: u.From, FromILM: u.FromILM, Backup: u.Backup}, nil
	case u == nil || u.Kind != release.BundlePlatform || u.Backup == "":
		return nil, &Error{Reason: NoPreUpdateBackup}
	case u.Message != "" && !u.PointOfNoReturn:
		return nil, &Error{Reason: NotNeeded}
	}
	return u, nil
}

// abandonedRollback tells whether the last update is a rollback whose restore was given up before it was done: the
// platform is neither the updated nor the old one, and the pre-update backup is still the way back.
func abandonedRollback(a *v1alpha1.Appliance) bool {
	u, r := a.Status.Update, a.Status.Restore
	return u != nil && u.Kind == KindRollback && u.Backup != "" && r != nil && r.Rollback != nil &&
		r.Step == v1alpha1.RestoreStepAbandoned
}

// platformChanged tells whether the abandoned rollback r had begun to uninstall the platform: it failed at another
// step than the first stop.
func platformChanged(r *v1alpha1.RestoreStatus) bool {
	return r.Rollback.Reinstalled || r.AbandonedStep != v1alpha1.RestoreStepStop
}

// pinnedBackup is the path of the local backup name, pinned for a platform update.
func (j *Jobs) pinnedBackup(name string) (string, error) {
	pinned, err := j.pinned(name)
	if err != nil {
		return "", err
	}
	if !pinned {
		return "", &Error{Reason: NoPreUpdateBackup}
	}
	f, _, err := j.deps.Backups.Open(name)
	if errors.Is(err, backup.ErrNotFound) {
		return "", &Error{Reason: NoPreUpdateBackup, err: err}
	}
	if err != nil {
		return "", fail(stepRollback, err)
	}
	_ = f.Close()
	return f.Name(), nil
}

// pinned tells whether the local backup name is pinned for a platform update.
func (j *Jobs) pinned(name string) (bool, error) {
	records, err := j.deps.Backups.Local()
	if err != nil {
		return false, fail(stepRollback, err)
	}
	return slices.ContainsFunc(records, func(r v1alpha1.BackupRecord) bool {
		return r.Name == name && r.Pinned == release.BundlePlatform
	}), nil
}

// platformSource is the platform a rollback installs again: a loaded platform bundle, or the release's own platform,
// whose bundle and metadata are empty.
type platformSource struct {
	ilm    string
	bundle string
	md     *bundle.Metadata
}

// previous is the platform that ran before the update from bundle version from and ILM version fromILM, empty when
// not recorded: see source. The update must not have changed the ILM operator.
func (j *Jobs) previous(a *v1alpha1.Appliance, from, fromILM string) (platformSource, error) {
	src, err := j.source(a, from, fromILM)
	if err != nil {
		return platformSource{}, err
	}
	current, err := InstalledPlatform(j.deps.Dir, a.Status.Versions.PlatformBundle, j.deps.Arch)
	if err != nil {
		return platformSource{}, fail(stepRollback, err)
	}
	if older(j.activeOperator(src.md), j.activeOperator(current)) {
		return platformSource{}, &Error{Reason: OperatorChanged}
	}
	return src, nil
}

// source is the platform a rollback installs again. With the ILM version the update started from recorded, it is a
// loaded platform bundle that carries that version, the bundle from first, else the release's own platform when it
// embeds that version: a system update since may have brought a release with another. Without it, it is the bundle
// from when loaded, else the release's own platform when the update was the first.
func (j *Jobs) source(a *v1alpha1.Appliance, from, fromILM string) (platformSource, error) {
	if fromILM != "" {
		return j.sourceOf(a, from, fromILM)
	}
	md, err := InstalledPlatform(j.deps.Dir, from, j.deps.Arch)
	switch {
	case err == nil && md != nil:
		return platformSource{ilm: md.ILM, bundle: from, md: md}, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return platformSource{}, fail(stepRollback, err)
	case from != "" && from == a.Status.Versions.Appliance && j.deps.Release.Platform.ILMVersion != "":
		return platformSource{ilm: j.deps.Release.Platform.ILMVersion}, nil
	}
	return platformSource{}, &Error{Reason: NoPreviousBundle}
}

// sourceCandidates are the platform bundle versions a rollback to ILM version ilm may install again, in the order it
// tries them: the bundle from, then each loaded platform bundle for arch carrying ilm.
func sourceCandidates(a *v1alpha1.Appliance, from, ilm, arch string) []string {
	candidates := []string{from}
	if a.Status.Updates == nil || ilm == "" {
		return candidates
	}
	for _, b := range a.Status.Updates.Loaded {
		if runnable(b) && b.ILM == ilm && b.Arch == arch {
			candidates = append(candidates, b.Version)
		}
	}
	return candidates
}

// sourceOf is the platform of ILM version ilm: a loaded platform bundle carrying it, the bundle from first, else the
// release's own platform when it embeds ilm.
func (j *Jobs) sourceOf(a *v1alpha1.Appliance, from, ilm string) (platformSource, error) {
	for _, version := range sourceCandidates(a, from, ilm, j.deps.Arch) {
		md, err := InstalledPlatform(j.deps.Dir, version, j.deps.Arch)
		switch {
		case err == nil && md != nil && md.ILM == ilm:
			return platformSource{ilm: ilm, bundle: version, md: md}, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return platformSource{}, fail(stepRollback, err)
		}
	}
	if j.deps.Release.Platform.ILMVersion == ilm {
		return platformSource{ilm: ilm}, nil
	}
	return platformSource{}, &Error{Reason: NoPreviousBundle}
}

// activeOperator is the ILM operator the add-on pass applies with the platform bundle md: its operator when newer
// than the embedded one, else the embedded one.
func (j *Jobs) activeOperator(md *bundle.Metadata) string {
	return appliedOperator(j.embeddedOperator(), md)
}

// restoreRefusals map the refusals of a restore to the reasons of a rollback.
var restoreRefusals = []struct {
	err    error
	reason string
}{
	{restore.ErrBusy, RestoreRunning},
	{restore.ErrSetup, NotInstalled},
	{restore.ErrUpgradeInProgress, UpgradeInProgress},
	{restore.ErrForeignKit, WrongKit},
	{restore.ErrTampered, BackupBroken},
	{restore.ErrTruncated, BackupBroken},
	{restore.ErrFormatTooNew, BackupBroken},
}

// restoreRefusal is the refusal of a rollback whose restore did not start.
func restoreRefusal(err error) error {
	for _, r := range restoreRefusals {
		if errors.Is(err, r.err) {
			return &Error{Reason: r.reason, step: stepRollback, err: err}
		}
	}
	if errors.As(err, new(restore.ErrVersionUnavailable)) {
		return &Error{Reason: BackupBroken, step: stepRollback, err: err}
	}
	return fail(stepRollback, err)
}

// refuseFailedUpdate refuses an update while the last platform update failed past its point of no return, or a
// rollback of it was abandoned after it began to uninstall the platform, and the backup taken before it is still
// pinned: a new update would back up the half-updated platform and release that backup, the only way back.
func (j *Jobs) refuseFailedUpdate(a *v1alpha1.Appliance) error {
	u := a.Status.Update
	failed := u != nil && u.Kind == release.BundlePlatform && u.Message != "" && u.PointOfNoReturn && u.Backup != ""
	abandoned := abandonedRollback(a) && platformChanged(a.Status.Restore)
	if !failed && !abandoned {
		return nil
	}
	pinned, err := j.pinned(u.Backup)
	if err != nil {
		return err
	}
	if pinned {
		return &Error{Reason: LastUpdateFailed}
	}
	return nil
}
