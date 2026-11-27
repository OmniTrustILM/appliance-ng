package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/Masterminds/semver/v3"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/update"
)

const (
	updatesPath       = auth.APIBase + "/updates"
	updatesUploadPath = updatesPath + "/upload"
	// updatesRollbackPath takes the recovery kit.
	updatesRollbackPath = updatesPath + "/rollback"
	// maxBundleUpload caps an uploaded bundle.
	maxBundleUpload = 64 << 30

	codeUpdatesUnavailable = "updates_unavailable"
	codeUpdateBusy         = "busy"
	// codeBundleLoading refuses another operation while an update or a bundle load runs.
	codeBundleLoading = "bundle_loading"
	detailKind        = "kind"
	detailVersion     = "version"
)

var (
	answerUpdatesUnavailable = refused(http.StatusServiceUnavailable, codeUpdatesUnavailable, "Update bundles are not available in this manager.")
	answerBadKind            = refusedField(codeInvalid, detailKind, "Choose a system or a platform bundle.")
	answerBadVersion         = refusedField(codeInvalid, detailVersion, "Enter a version X.Y.Z.")
	answerNoBundleSize       = refused(http.StatusBadRequest, codeInvalid, "Send the bundle's size in Content-Length; a bundle is at most 64 GiB.")
	answerPartialBundle      = refused(http.StatusBadRequest, "incomplete", "The bundle did not arrive completely; upload it again.")
	answerBundleNoSpace      = refused(http.StatusConflict, "no_space",
		"The data volume has too little free space for the bundle: it needs three times the bundle's size.")
	answerUpdateBusy = refused(http.StatusConflict, codeUpdateBusy,
		"A backup, a restore, an update or another bundle load is running; try again once it has finished.")
	answerNoNotes = refused(http.StatusNotFound, codeNotFound, "There are no release notes for that bundle.")

	answerUpdateUnconfirmed = refusedField(codeInvalid, "confirm",
		"Confirm the update: the way back is a reinstall and a restore of the backup taken first, which loses the data changed since.")
	answerSystemUnconfirmed = refusedField(codeInvalid, "confirm",
		"Confirm the update: the manager restarts and the node reboots, which takes the appliance down for about 10 minutes.")
	answerUpdateBlocked = refused(http.StatusConflict, "blocked", "The bundle cannot be installed on what runs.")

	answerRollbackUnconfirmed = refusedField(codeInvalid, fieldConfirm,
		"Confirm the rollback: ILM is installed again at the previous version and the backup taken before the update is restored, which loses the data changed since.")
)

// rollbackRefusals answer the refusals of a rollback that are not those of an update.
var rollbackRefusals = map[string]restoreAnswer{
	update.NoPreUpdateBackup: refused(http.StatusConflict, "no_pre_update_backup",
		"There is no platform update to roll back, or the backup taken before it is gone."),
	update.NoPreviousBundle: refused(http.StatusConflict, "no_previous_bundle",
		"The platform bundle that ran before the update is no longer on the appliance, and an older bundle cannot be loaded; restore the backup taken before the update onto a fresh appliance instead."),
	update.OperatorChanged: refused(http.StatusConflict, "operator_changed",
		"The update installed a newer ILM operator, which is never downgraded; restore the backup onto a fresh appliance instead."),
	update.NotNeeded: refused(http.StatusConflict, "not_needed",
		"The update failed before it asked for the new ILM version, so ILM still runs the previous version; a rollback would only lose the data changed since the backup."),
	update.ExternalDatabase: refused(http.StatusConflict, "external_database",
		"The platform uses an external database, which a rollback cannot restore; restore the backup onto a fresh appliance instead."),
	update.WrongKit: refused(http.StatusConflict, "foreign_kit",
		"The kit does not open the backup taken before the update: the backup was encrypted to another backup key."),
	update.BackupBroken: refused(http.StatusConflict, "backup_broken",
		"The backup taken before the update cannot be restored: it is damaged, too new, or holds another ILM version."),
	update.UpgradeInProgress: updateRefusals[update.PlatformUpgrading],
}

// updateRefusals answer the refusals of an update that are not about the bundle itself.
var updateRefusals = map[string]restoreAnswer{
	update.UpdateRunning:  refused(http.StatusConflict, "update_running", "Another update is running."),
	update.BackupRunning:  refused(http.StatusConflict, "backup_running", "A backup is running; update once it has finished."),
	update.RestoreRunning: refused(http.StatusConflict, "restore_running", "A restore is running; update once it has ended."),
	update.BundleLoading:  refused(http.StatusConflict, codeBundleLoading, "A bundle is loading; update once it has finished."),
	update.Busy:           answerUpdateBusy,
	update.PlatformUpgrading: refused(http.StatusConflict, "upgrade_in_progress",
		"The ILM operator is upgrading the platform; update once it has finished."),
	update.NotInstalled: refused(http.StatusConflict, "not_installed", "The platform is not installed yet."),
	update.LastUpdateFailed: refused(http.StatusConflict, "last_update_failed",
		"The last platform update failed; roll it back before installing another."),
	update.OperatorOlderThanRunning: refused(http.StatusConflict, "operator_older_than_running",
		"The bundle's ILM operator is older than the one that runs, which is never downgraded."),
	update.NotFound: refused(http.StatusNotFound, codeNotFound, "The bundle is neither loaded nor found online."),
	update.KubernetesOlderThanRunning: refused(http.StatusConflict, "kubernetes_older_than_running",
		"The bundle's Kubernetes is older than the one that runs, which is never downgraded."),
	update.NoSpace: refused(http.StatusConflict, "no_space",
		"The data volume has too little free space for the update: it needs twice the bundle's size."),
}

// systemRefusals answer the refusals of a system update that differ from those of a platform update.
var systemRefusals = map[string]restoreAnswer{
	update.NotFound: refused(http.StatusNotFound, codeNotFound, "The system bundle is not loaded."),
	update.BundleUnavailable: refused(http.StatusConflict, "bundle_unavailable",
		"The bundle's files are no longer on the appliance; load the bundle again."),
	update.InvalidBundle: refused(http.StatusConflict, "invalid_bundle",
		"The bundle carries no installer, manager or etcd image, so it cannot be installed."),
	update.UnmirroredRegistry: refused(http.StatusConflict, "unmirrored_registry",
		"The bundle's installer, manager or etcd image comes from a registry this appliance does not mirror."),
	update.KubernetesMinorSkip: refused(http.StatusConflict, "kubernetes_minor_skip",
		"The bundle's Kubernetes is more than one minor version above the one that runs; install the system releases in between first."),
}

// blockedReasons are the reasons a bundle cannot be installed on what runs.
var blockedReasons = map[string]struct{}{
	update.OlderThanRunning: {}, update.OperatorOutOfRange: {}, update.NeedsAppliance: {}, update.WrongArch: {},
	update.SameVersion: {}, update.InvalidMetadata: {},
}

// Updater loads bundles, one at a time, into the appliance's registry and data volume.
type Updater interface {
	// Reserve takes the shared admission for a load of kind by user; it refuses with the reason update.Busy.
	Reserve(ctx context.Context, kind, user string) (*update.Reservation, error)
	// Last is the latest load.
	Last() update.LoadState
	// Notes are the release notes of a bundle loaded or found online; update.ErrNoNotes when there are none.
	Notes(kind, version string) ([]byte, error)
}

// PlatformUpdater installs a platform bundle in the background and writes the audit entry of the start.
type PlatformUpdater interface {
	// StartPlatform starts the update to version to by user; it refuses with an *update.Error.
	StartPlatform(ctx context.Context, to, user string) error
	// StartSystem starts the update to the system bundle version to by user; it refuses with an *update.Error.
	StartSystem(ctx context.Context, to, user string) error
	PlatformRollback
}

// PlatformRollback rolls the last platform update back in the background and writes the audit entry of the start.
type PlatformRollback interface {
	// Rollback rolls the last platform update back with id, the recovery kit's backup key, by user; it refuses with
	// an *update.Error.
	Rollback(ctx context.Context, id age.Identity, user string) error
	// RollbackRefused is the fixed reason the last platform update cannot be rolled back now; empty when it can.
	RollbackRefused(ctx context.Context) string
}

// UpdateFinder checks the online repository for newer bundles and records them in the status.
type UpdateFinder interface {
	Check(ctx context.Context) error
}

func (a restoreAnswer) VisitGetUpdatesResponse(w http.ResponseWriter) error       { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesCheckResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPutUpdatesUploadResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesFetchResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesPlatformResponse(w http.ResponseWriter) error {
	return a.visit(w)
}
func (a restoreAnswer) VisitPostUpdatesSystemResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitGetUpdateNotesResponse(w http.ResponseWriter) error    { return a.visit(w) }
func (a restoreAnswer) VisitPostUpdatesRollbackResponse(w http.ResponseWriter) error {
	return a.visit(w)
}

func (s *Server) updatesAvailable() bool {
	return s.deps.Updates != nil && s.deps.Finder != nil
}

// GetUpdates lists the bundles available online and loaded, and the latest load.
func (s *Server) GetUpdates(ctx context.Context, _ GetUpdatesRequestObject) (GetUpdatesResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	u, err := s.updates(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.Platform != nil {
		u.Rollback = rollbackOffer(s.deps.Platform.RollbackRefused(ctx))
	}
	return restoreAnswer{status: http.StatusOK, body: u}, nil
}

func rollbackOffer(reason string) *RollbackOffer {
	return &RollbackOffer{Available: reason == "", Reason: optional(reason)}
}

// PostUpdatesCheck checks the online repository now; a failed check is answered with its fixed reason.
func (s *Server) PostUpdatesCheck(ctx context.Context, _ PostUpdatesCheckRequestObject) (PostUpdatesCheckResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	if err := s.deps.Finder.Check(ctx); err != nil {
		requestFrom(ctx).result = Failure
		recordDetails(ctx, map[string]string{detailRefused: update.Reason(err)})
	}
	u, err := s.updates(ctx)
	if err != nil {
		return nil, err
	}
	return restoreAnswer{status: http.StatusOK, body: u}, nil
}

// PutUpdatesUpload stores the uploaded bundle and loads it in the background. The admission and the free space are
// checked before the body is read.
func (s *Server) PutUpdatesUpload(ctx context.Context, req PutUpdatesUploadRequestObject) (PutUpdatesUploadResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	kind := req.Params.Kind
	details := map[string]string{detailKind: string(kind)}
	size := requestFrom(ctx).contentLength
	switch {
	case !kind.Valid():
		return answered(ctx, details, answerBadKind), nil
	case size < 0 || size > maxBundleUpload:
		return answered(ctx, details, answerNoBundleSize), nil
	}
	r, answer, err := s.reserve(ctx, string(kind))
	if answer != nil || err != nil {
		return answered(ctx, details, orFailure(answer)), err
	}
	path, err := s.storeBundle(size, req.Body)
	switch {
	case errors.Is(err, errSource):
		r.Cancel()
		return answered(ctx, details, answerPartialBundle), nil
	case errors.Is(err, storage.ErrNoSpace):
		r.Cancel()
		return answered(ctx, details, answerBundleNoSpace), nil
	case err != nil:
		r.Cancel()
		return nil, err
	}
	r.Load(path)
	return answered(ctx, details, restoreAnswer{status: http.StatusAccepted}), nil
}

// PostUpdatesFetch loads a bundle from the online repository in the background.
func (s *Server) PostUpdatesFetch(ctx context.Context, req PostUpdatesFetchRequestObject) (PostUpdatesFetchResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	if req.Body == nil || !req.Body.Kind.Valid() {
		return answered(ctx, map[string]string{}, answerBadKind), nil
	}
	details := map[string]string{detailKind: string(req.Body.Kind)}
	if !validVersion(req.Body.Version) {
		return answered(ctx, details, answerBadVersion), nil
	}
	details[detailVersion] = req.Body.Version
	r, answer, err := s.reserve(ctx, string(req.Body.Kind))
	if answer != nil || err != nil {
		return answered(ctx, details, orFailure(answer)), err
	}
	r.Fetch(req.Body.Version)
	return answered(ctx, details, restoreAnswer{status: http.StatusAccepted}), nil
}

// GetUpdateNotes serves the release notes of a bundle as Markdown.
func (s *Server) GetUpdateNotes(_ context.Context, req GetUpdateNotesRequestObject) (GetUpdateNotesResponseObject, error) {
	if !s.updatesAvailable() {
		return answerUpdatesUnavailable, nil
	}
	notes, err := s.deps.Updates.Notes(string(req.Kind), req.Version)
	if errors.Is(err, update.ErrNoNotes) {
		return answerNoNotes, nil
	}
	if err != nil {
		return nil, err
	}
	return GetUpdateNotes200TextmarkdownResponse{Body: bytes.NewReader(notes), ContentLength: int64(len(notes))}, nil
}

// reserve takes the shared admission for a load of kind by the request's user, or answers why not.
func (s *Server) reserve(ctx context.Context, kind string) (*update.Reservation, *restoreAnswer, error) {
	r, err := s.deps.Updates.Reserve(ctx, kind, requestFrom(ctx).user)
	if update.Reason(err) == update.Busy {
		return nil, &answerUpdateBusy, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return r, nil, nil
}

// storeBundle writes size bytes of the upload to a new file in the uploads directory, once three times that is free
// for the file and the load that follows, and returns its path. A failure to read the upload wraps errSource, a full
// volume storage.ErrNoSpace; nothing is kept then.
func (s *Server) storeBundle(size int64, body interface{ Read([]byte) (int, error) }) (string, error) {
	dir := s.deps.UpdatesDir
	if err := os.MkdirAll(dir, restoreDirMode); err != nil {
		return "", fmt.Errorf("create the uploads directory: %w", err)
	}
	// checkSpace wants twice what it is given free: three times the size is twice one and a half times it.
	need := uint64(size) + uint64(size+1)/2 //nolint:gosec // checked to be positive by the caller
	if err := s.checkSpace(dir, need); err != nil {
		return "", err
	}
	name := make([]byte, 16)
	_, _ = rand.Read(name) // crypto/rand.Read never returns an error.
	path := filepath.Join(dir, hex.EncodeToString(name)+".tar")
	if err := writeFile(path, size, body); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	s.deps.Log.Info("bundle upload stored", "sizeBytes", size)
	return path, nil
}

// updates are the update bundles in the status and the latest load.
func (s *Server) updates(ctx context.Context) (Updates, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return Updates{}, err
	}
	out := Updates{
		Available: []UpdateBundle{}, Loaded: []UpdateBundle{}, Operator: optional(a.Status.Versions.Operator),
		ExternalDatabase: a.Spec.Platform != nil && a.Spec.Platform.Database != nil,
	}
	if u := a.Status.Updates; u != nil {
		if u.LastCheck != nil {
			out.LastCheck = &u.LastCheck.Time
		}
		if u.LastCheckError != "" {
			e := UpdatesLastCheckError(u.LastCheckError)
			out.LastCheckError = &e
		}
		out.Available, out.Loaded = updateBundles(u.Available), updateBundles(u.Loaded)
	}
	if u := a.Status.Update; u != nil {
		out.Update = &UpdateProgress{
			Kind: UpdateProgressKind(u.Kind), From: u.From, To: u.To, Step: optional(u.Step), Message: optional(u.Message),
			Backup: optional(u.Backup), StartedAt: u.StartedAt.Time, PointOfNoReturn: u.PointOfNoReturn,
		}
		// A rollback is done by a restore, whose step and failure are those of the rollback.
		if r := a.Status.Restore; u.Kind == update.KindRollback && r != nil && r.Rollback != nil {
			out.Update.Step, out.Update.Message = optional(r.Step), optional(r.Message)
		}
	}
	if last := s.deps.Updates.Last(); last.State != "" {
		out.Load = &BundleLoad{
			State: BundleLoadState(last.State), Kind: BundleKind(last.Kind), StartedAt: last.StartedAt,
			Version: optional(last.Version), Reason: optional(last.Reason),
		}
	}
	return out, nil
}

func updateBundles(infos []v1alpha1.BundleInfo) []UpdateBundle {
	out := make([]UpdateBundle, 0, len(infos))
	for _, b := range infos {
		u := UpdateBundle{
			Kind: BundleKind(b.Kind), Version: b.Version, Arch: b.Arch, SizeBytes: b.SizeBytes,
			Ilm: optional(b.ILM), Operator: optional(b.Operator), MinAppliance: optional(b.MinAppliance),
			OperatorRange: optional(b.OperatorRange), Needs: optional(b.Needs),
			Notes: updatesPath + "/notes/" + url.PathEscape(b.Kind) + "/" + url.PathEscape(b.Version),
		}
		if b.LoadedAt != nil {
			u.LoadedAt = &b.LoadedAt.Time
		}
		if b.Blocked != "" {
			blocked := UpdateBundleBlocked(b.Blocked)
			u.Blocked = &blocked
		}
		out = append(out, u)
	}
	return out
}

// PostUpdatesPlatform starts the update to a platform bundle in the background, once confirmed. The job writes the
// audit entry of a started update; a refused one is recorded here.
func (s *Server) PostUpdatesPlatform(ctx context.Context, req PostUpdatesPlatformRequestObject) (PostUpdatesPlatformResponseObject, error) {
	if req.Body == nil {
		return s.startUpdate(ctx, nil, false, answerUpdateUnconfirmed, nil, updateRefusal)
	}
	return s.startUpdate(ctx, &req.Body.Version, req.Body.Confirm, answerUpdateUnconfirmed, s.platformStarter, updateRefusal)
}

// PostUpdatesSystem starts the update to a system bundle in the background, once confirmed, with the audit of a
// platform update.
func (s *Server) PostUpdatesSystem(ctx context.Context, req PostUpdatesSystemRequestObject) (PostUpdatesSystemResponseObject, error) {
	if req.Body == nil {
		return s.startUpdate(ctx, nil, false, answerSystemUnconfirmed, nil, systemRefusal)
	}
	return s.startUpdate(ctx, &req.Body.Version, req.Body.Confirm, answerSystemUnconfirmed, s.systemStarter, systemRefusal)
}

func (s *Server) platformStarter(ctx context.Context, version string) error {
	return s.deps.Platform.StartPlatform(ctx, version, requestFrom(ctx).user)
}

func (s *Server) systemStarter(ctx context.Context, version string) error {
	return s.deps.Platform.StartSystem(ctx, version, requestFrom(ctx).user)
}

// startUpdate refuses an update that is not confirmed or names no version X.Y.Z with unconfirmed, and starts the
// others with start, answering its refusals with refusal; a nil version is a request without a body.
func (s *Server) startUpdate(ctx context.Context, version *string, confirm bool, unconfirmed restoreAnswer, start func(context.Context, string) error, refusal func(error) (restoreAnswer, bool)) (restoreAnswer, error) {
	if !s.updatesAvailable() || s.deps.Platform == nil {
		return answerUpdatesUnavailable, nil
	}
	details := map[string]string{}
	if version != nil {
		details[detailVersion] = *version
	}
	switch {
	case version == nil || !confirm:
		return answered(ctx, details, unconfirmed), nil
	case !validVersion(*version):
		return answered(ctx, details, answerBadVersion), nil
	}
	err := start(ctx, *version)
	if err == nil {
		requestFrom(ctx).unaudited = true
		return restoreAnswer{status: http.StatusAccepted}, nil
	}
	if answer, ok := refusal(err); ok {
		return answered(ctx, details, answer), nil
	}
	return restoreAnswer{}, fmt.Errorf("start the update: %w", err)
}

// PostUpdatesRollback rolls the last platform update back with the uploaded recovery kit in the background, once
// confirmed. The job writes the audit entry of a started rollback; a refused one is recorded here, never with the kit
// or the passphrase.
func (s *Server) PostUpdatesRollback(ctx context.Context, req PostUpdatesRollbackRequestObject) (PostUpdatesRollbackResponseObject, error) {
	if !s.updatesAvailable() || s.deps.Platform == nil {
		return answerUpdatesUnavailable, nil
	}
	details := map[string]string{}
	parts, ref := readUpload(req.Body, partKit)
	switch {
	case ref != nil:
		return answered(ctx, details, fromRefusal(ref)), nil
	case string(parts[fieldConfirm]) != "true":
		return answered(ctx, details, answerRollbackUnconfirmed), nil
	}
	kit, err := s.openKit(ctx, parts[partKit], string(parts[partPassphrase]))
	if answer, ok := restoreRefusal(err); ok {
		return answered(ctx, details, answer), nil
	}
	if err != nil {
		return nil, restoreFailed("open the recovery kit", err)
	}
	err = s.deps.Platform.Rollback(ctx, kit.BackupKey, requestFrom(ctx).user)
	if err == nil {
		requestFrom(ctx).unaudited = true
		return restoreAnswer{status: http.StatusAccepted}, nil
	}
	if answer, ok := rollbackRefusal(err); ok {
		return answered(ctx, details, answer), nil
	}
	return nil, restoreFailed("start the rollback", err)
}

// rollbackRefusal is the answer to a refusal of a rollback; false for a failure.
func rollbackRefusal(err error) (restoreAnswer, bool) {
	var e *update.Error
	if errors.As(err, &e) {
		if answer, ok := rollbackRefusals[e.Reason]; ok {
			return answer, true
		}
	}
	return updateRefusal(err)
}

// systemRefusal is the answer to a refusal of a system update; false for a failure.
func systemRefusal(err error) (restoreAnswer, bool) {
	var e *update.Error
	if errors.As(err, &e) {
		if answer, ok := systemRefusals[e.Reason]; ok {
			return answer, true
		}
	}
	return updateRefusal(err)
}

// updateRefusal is the answer to a refusal of an update; false for a failure.
func updateRefusal(err error) (restoreAnswer, bool) {
	var e *update.Error
	if !errors.As(err, &e) {
		return restoreAnswer{}, false
	}
	if answer, ok := updateRefusals[e.Reason]; ok {
		return answer, true
	}
	if _, ok := blockedReasons[e.Reason]; ok {
		return answerUpdateBlocked, true
	}
	return restoreAnswer{}, false
}

func validVersion(v string) bool {
	_, err := semver.StrictNewVersion(v)
	return err == nil
}
