package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/hostname"
	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
)

const (
	restorePath       = auth.APIBase + "/restore"
	restoreUploadPath = restorePath + "/upload"
	// restoreWorkTimeout bounds what reads a whole backup within one request: a fetch, an inspection and the check
	// before a resume.
	restoreWorkTimeout = time.Hour

	partSource      = "source"
	partName        = "name"
	fieldILMHost    = "ilmHostName"
	detailBackup    = "backup"
	detailRefused   = "refused"
	codeRestoreBusy = "busy"
	codeSetup       = "setup"
	codeVersion     = "version_unavailable"
)

var (
	answerNotInspected = refused(http.StatusConflict, "not_inspected", "Check the backup with the recovery kit first.")
	answerSuperseded   = refused(http.StatusConflict, "superseded",
		"Another backup check in this session replaced this one; check the backup again.")
	answerNotLocal = refused(http.StatusConflict, "not_local",
		"A restore in place takes a backup on this appliance's data volume; a fresh restore an uploaded or fetched one.")
	answerUnconfirmed = refusedField(codeInvalid, fieldConfirm,
		"Confirm that the backup replaces what the appliance holds now.")
	answerBadILMHost = refusedField(codeInvalid, fieldILMHost,
		"Enter a fully qualified host name, such as ilm.example.com, not an IP address.")
	answerBadSource = refusedField(codeInvalid, partSource, "Choose an uploaded or a local backup.")
	answerNoBackup  = refused(http.StatusNotFound, codeNotFound, "There is no backup of that name to restore.")
	answerFreshOnly = refused(http.StatusConflict, codeSetup,
		"Backups are uploaded or fetched only on a fresh appliance that waits for a restore; restore a local backup in place from the Backups page.")
	answerInPlaceOnly = refused(http.StatusConflict, codeSetup,
		"A local backup is restored in place on an appliance whose setup is complete.")
	answerRestoreBusy = refused(http.StatusConflict, codeRestoreBusy,
		"The appliance is handling a restore or another backup for it; try again once that has finished.")

	// restoreRefusals answer the refusals of inspecting, starting and resuming a restore with fixed codes and texts.
	restoreRefusals = []struct {
		err    error
		answer restoreAnswer
	}{
		{recovery.ErrWrongPassphrase, fromRefusal(refusedWrongPassphrase)},
		{recovery.ErrInvalidKit, fromRefusal(refusedInvalidKit)},
		{restore.ErrForeignKit, refused(http.StatusConflict, "foreign_kit",
			"The kit does not open this backup: the backup was encrypted to another appliance's backup key.")},
		{restore.ErrTampered, refused(http.StatusConflict, "tampered", "The backup was changed after it was written and cannot be restored.")},
		{restore.ErrTruncated, refused(http.StatusConflict, "truncated", "The backup file is incomplete; upload or fetch it again.")},
		{restore.ErrFormatTooNew, refused(http.StatusConflict, "format_too_new",
			"The backup was written in a newer format than this appliance reads.")},
		{restore.ErrBusy, refused(http.StatusConflict, codeRestoreBusy, "A restore is in progress.")},
		{restore.ErrNotWaiting, refused(http.StatusConflict, "not_waiting", "No restore waits to be resumed.")},
		{restore.ErrSetup, refused(http.StatusConflict, codeSetup,
			"The appliance's setup does not allow this restore: a fresh restore needs a fresh appliance, a restore in place one whose setup is complete.")},
		{restore.ErrUpgradeInProgress, refused(http.StatusConflict, "upgrade_in_progress",
			"The platform is being upgraded; restore once the upgrade has finished.")},
		{restore.ErrBackupRunning, refused(http.StatusConflict, "backup_running", "A backup is running; restore once it has finished.")},
	}
)

// inspection is a backup that passed Inspect, kept in memory for the session that checked it until a restore
// starts from it or another backup is checked. The backup key is never written anywhere. ref identifies the check to
// the start that confirms its summary.
type inspection struct {
	ref     string
	token   string
	name    string
	file    string
	local   bool
	summary restore.Summary
	id      age.Identity
}

// restoreAnswer answers a restore operation with status and, unless nil, body as JSON.
type restoreAnswer struct {
	status int
	body   any
}

func refused(status int, code, message string) restoreAnswer {
	return restoreAnswer{status: status, body: Error{Code: code, Message: message}}
}

func refusedField(code, field, message string) restoreAnswer {
	return restoreAnswer{status: http.StatusBadRequest, body: Error{Code: code, Message: message, Field: &field}}
}

// fromRefusal answers ref with 409 when it is a conflict, else with 400.
func fromRefusal(ref *refusal) restoreAnswer {
	if ref.conflict {
		return refused(http.StatusConflict, ref.code, ref.message)
	}
	return restoreAnswer{status: http.StatusBadRequest, body: ref.badRequest()}
}

func (a restoreAnswer) visit(w http.ResponseWriter) error {
	if a.body == nil {
		w.WriteHeader(a.status)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.status)
	return json.NewEncoder(w).Encode(a.body)
}

func (a restoreAnswer) VisitPutRestoreUploadResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPostRestoreListResponse(w http.ResponseWriter) error  { return a.visit(w) }
func (a restoreAnswer) VisitPostRestoreFetchResponse(w http.ResponseWriter) error { return a.visit(w) }
func (a restoreAnswer) VisitPostRestoreInspectResponse(w http.ResponseWriter) error {
	return a.visit(w)
}
func (a restoreAnswer) VisitPostRestoreStartResponse(w http.ResponseWriter) error  { return a.visit(w) }
func (a restoreAnswer) VisitPostRestoreResumeResponse(w http.ResponseWriter) error { return a.visit(w) }

// refusal is the code of a refusing answer; empty for one that is not refused.
func (a restoreAnswer) refusal() string {
	if e, ok := a.body.(Error); ok {
		return e.Code
	}
	return ""
}

// answered records details and the code of a refusal in the request's audit entry; never a passphrase, a kit or a
// credential.
func answered(ctx context.Context, details map[string]string, a restoreAnswer) restoreAnswer {
	if code := a.refusal(); code != "" {
		details[detailRefused] = code
	}
	recordDetails(ctx, details)
	return a
}

// restoreRefusal is the fixed answer to err, a refusal of Inspect or of the engine; false for another error.
func restoreRefusal(err error) (restoreAnswer, bool) {
	var version restore.ErrVersionUnavailable
	if errors.As(err, &version) {
		return versionUnavailable(version.Have, version.Want), true
	}
	for _, r := range restoreRefusals {
		if errors.Is(err, r.err) {
			return r.answer, true
		}
	}
	return restoreAnswer{}, false
}

func versionUnavailable(have, want string) restoreAnswer {
	return refused(http.StatusConflict, codeVersion,
		fmt.Sprintf("The backup holds ILM %s, but this appliance has ILM %s; restore it on an appliance with ILM %s.", want, have, want))
}

// restoreFailed is err without its text, which may name a file, a URL or a credential, for the internal error answer.
func restoreFailed(what string, err error) error {
	return fmt.Errorf("%s: %s", what, backup.Describe(err))
}

// GetRestore reports the setup phase and the latest restore.
func (s *Server) GetRestore(ctx context.Context, _ GetRestoreRequestObject) (GetRestoreResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetRestore409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	out := RestoreState{Setup: a.Status.Setup}
	if st := a.Status.Restore; st != nil {
		out.Restore = &RestoreProgress{
			Backup: st.Backup, InPlace: st.InPlace, Step: RestoreProgressStep(st.Step), PausedStep: optional(st.PausedStep),
			Warning: optional(st.Warning), IlmHostName: optional(st.ILMHostName),
		}
		if st.Message != "" {
			message := RestoreProgressMessage(st.Message)
			out.Restore.Message = &message
		}
		if !st.StartedAt.IsZero() {
			out.Restore.StartedAt = timeOf(&st.StartedAt)
		}
	}
	return GetRestore200JSONResponse(out), nil
}

// PostRestoreInspect checks a backup with the uploaded kit and keeps its backup key for the session.
func (s *Server) PostRestoreInspect(ctx context.Context, req PostRestoreInspectRequestObject) (PostRestoreInspectResponseObject, error) {
	parts, ref := readUpload(req.Body, partKit)
	if ref != nil {
		return fromRefusal(ref), nil
	}
	name, source := string(parts[partName]), RestoreInspectSource(parts[partSource])
	details := map[string]string{detailBackup: name, partSource: string(source)}
	if !source.Valid() {
		return answered(ctx, details, answerBadSource), nil
	}
	if !s.restoreMu.TryLock() {
		return answered(ctx, details, answerRestoreBusy), nil
	}
	defer s.restoreMu.Unlock()
	answer, err := s.inspect(ctx, name, source == SourceLocal, parts)
	return answered(ctx, details, answer), err
}

// inspect checks the backup name, local or uploaded, with the kit and passphrase in parts.
func (s *Server) inspect(ctx context.Context, name string, local bool, parts map[string][]byte) (restoreAnswer, error) {
	a, answer, err := s.restoreAppliance(ctx, local)
	if answer != nil || err != nil {
		return orFailure(answer), err
	}
	file, answer, err := s.restoreFile(name, local)
	if answer != nil || err != nil {
		return orFailure(answer), err
	}
	users, err := s.userNames(ctx)
	if err != nil {
		return restoreAnswer{}, err
	}
	kit, err := s.openKit(ctx, parts[partKit], string(parts[partPassphrase]))
	var summary restore.Summary
	if err == nil {
		summary, err = restore.Inspect(ctx, file, kit.BackupKey, s.deps.Release, users)
	}
	if answer, ok := restoreRefusal(err); ok {
		return answer, nil
	}
	if err != nil {
		return restoreAnswer{}, restoreFailed("inspect the backup", err)
	}
	if running := a.Status.Versions.ILM; local && running != summary.Manifest.ILM {
		return versionUnavailable(running, summary.Manifest.ILM), nil
	}
	in := &inspection{ref: rand.Text(), token: requestFrom(ctx).token, name: name, file: file, local: local, summary: summary, id: kit.BackupKey}
	s.keepInspection(in)
	s.deps.Log.Info("backup checked for a restore", "backup", name, "local", local, "ilm", summary.Manifest.ILM)
	return restoreAnswer{status: http.StatusOK, body: summaryOf(in, a.Spec.Node.HostName)}, nil
}

// orFailure is the refusal a, or no answer when a call failed instead.
func orFailure(a *restoreAnswer) restoreAnswer {
	if a == nil {
		return restoreAnswer{}
	}
	return *a
}

// restoreAppliance reads the Appliance and refuses unless its setup takes a backup of the kind: a local backup is
// restored in place once setup is complete, an uploaded one onto a fresh appliance that waits for a restore.
func (s *Server) restoreAppliance(ctx context.Context, local bool) (*v1alpha1.Appliance, *restoreAnswer, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		answer := restoreAnswer{status: http.StatusConflict, body: Error(notSetUp())}
		return nil, &answer, nil
	}
	if err != nil {
		return nil, nil, err
	}
	switch {
	case local && a.Status.Setup != v1alpha1.SetupComplete:
		return nil, &answerInPlaceOnly, nil
	case !local && a.Status.Setup != v1alpha1.SetupRestore:
		return nil, &answerFreshOnly, nil
	}
	return a, nil, nil
}

// restoreFile is the path of the backup name: a local backup, or the uploaded or fetched one.
func (s *Server) restoreFile(name string, local bool) (string, *restoreAnswer, error) {
	if !local {
		stored, ok := storedName(name)
		if !ok {
			return "", &answerBadName, nil
		}
		return s.uploaded(stored)
	}
	if s.deps.Backups == nil {
		return "", &answerNoBackup, nil
	}
	f, _, err := s.deps.Backups.Open(name)
	if errors.Is(err, backup.ErrNotFound) {
		return "", &answerNoBackup, nil
	}
	if err != nil {
		return "", nil, restoreFailed("open the local backup", err)
	}
	_ = f.Close()
	return f.Name(), nil, nil
}

// userNames are the console accounts of this appliance.
func (s *Server) userNames(ctx context.Context) ([]string, error) {
	secret, err := s.systemSecret(ctx, auth.UsersSecret)
	if err != nil || secret == nil {
		return nil, err
	}
	return auth.UserNames(secret)
}

func summaryOf(in *inspection, nodeHost string) RestoreSummary {
	sum, m := in.summary, in.summary.Manifest
	return RestoreSummary{
		Inspection: in.ref, Backup: in.name, InPlace: in.local, Time: m.Time.UTC(), HostName: m.HostName, IlmHostName: m.ILMHostName, NodeHostName: nodeHost,
		Ilm: m.ILM, Operator: m.Operator, Appliance: m.Appliance, Database: RestoreSummaryDatabase(m.Database),
		DatabaseIncluded: m.DatabaseIncluded, Accounts: nonNil(sum.Accounts), Clashes: nonNil(sum.Clashes), Components: nonNil(sum.Components),
	}
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return slices.Clone(list)
}

// keepInspection replaces the backup checked last.
func (s *Server) keepInspection(in *inspection) {
	s.inspectMu.Lock()
	defer s.inspectMu.Unlock()
	s.inspected = in
}

// takeInspection is the backup the session of token checked last, or nil.
func (s *Server) takeInspection(token string) *inspection {
	s.inspectMu.Lock()
	defer s.inspectMu.Unlock()
	if s.inspected == nil || token == "" || s.inspected.token != token {
		return nil
	}
	return s.inspected
}

// forgetInspection drops the backup checked last, so that its key leaves the manager's memory.
func (s *Server) forgetInspection() {
	s.keepInspection(nil)
}

// PostRestoreStart restores the backup the session checked last.
func (s *Server) PostRestoreStart(ctx context.Context, req PostRestoreStartRequestObject) (PostRestoreStartResponseObject, error) {
	in := s.takeInspection(requestFrom(ctx).token)
	details := map[string]string{}
	if in != nil {
		details[detailBackup] = in.name
	}
	if req.Body == nil || !req.Body.Confirm {
		return answered(ctx, details, answerUnconfirmed), nil
	}
	details["inPlace"] = strconv.FormatBool(req.Body.InPlace)
	if in == nil {
		return answered(ctx, details, answerNotInspected), nil
	}
	if req.Body.Inspection != in.ref {
		return answered(ctx, details, answerSuperseded), nil
	}
	if !s.restoreMu.TryLock() {
		return answered(ctx, details, answerRestoreBusy), nil
	}
	defer s.restoreMu.Unlock()
	answer, err := s.startRestore(ctx, in, req.Body)
	return answered(ctx, details, answer), err
}

func (s *Server) startRestore(ctx context.Context, in *inspection, body *RestoreStart) (restoreAnswer, error) {
	if body.InPlace != in.local {
		return answerNotLocal, nil
	}
	host := strings.ToLower(strings.TrimSpace(deref(body.IlmHostName)))
	if host != "" && !hostname.IsFQDN(host) {
		return answerBadILMHost, nil
	}
	if host == in.summary.Manifest.ILMHostName {
		host = ""
	}
	if in.local {
		a, err := s.appliance(ctx)
		if err != nil {
			return restoreAnswer{}, err
		}
		if running := a.Status.Versions.ILM; running != in.summary.Manifest.ILM {
			return versionUnavailable(running, in.summary.Manifest.ILM), nil
		}
	}
	user := requestFrom(ctx).user
	err := s.deps.Restore.Start(ctx, in.file, in.id, restore.Options{InPlace: in.local, ILMHostName: host, User: user})
	if answer, ok := restoreRefusal(err); ok {
		return answer, nil
	}
	if err != nil {
		return restoreAnswer{}, restoreFailed("start the restore", err)
	}
	s.forgetInspection()
	s.deps.Log.Info("restore started from the console", "backup", in.name, "inPlace", in.local, "user", user)
	return restoreAnswer{status: http.StatusAccepted}, nil
}

// PostRestoreResume resumes the restore that waits for its kit, or one that failed, with the uploaded kit.
func (s *Server) PostRestoreResume(ctx context.Context, req PostRestoreResumeRequestObject) (PostRestoreResumeResponseObject, error) {
	details := map[string]string{}
	if a, err := s.appliance(ctx); err == nil && a.Status.Restore != nil {
		details[detailBackup] = a.Status.Restore.Backup
	}
	parts, ref := readUpload(req.Body, partKit)
	if ref != nil {
		return answered(ctx, details, fromRefusal(ref)), nil
	}
	if !s.restoreMu.TryLock() {
		return answered(ctx, details, answerRestoreBusy), nil
	}
	defer s.restoreMu.Unlock()
	answer, err := s.resume(ctx, parts)
	return answered(ctx, details, answer), err
}

func (s *Server) resume(ctx context.Context, parts map[string][]byte) (restoreAnswer, error) {
	kit, err := s.openKit(ctx, parts[partKit], string(parts[partPassphrase]))
	if err == nil {
		err = s.deps.Restore.Resume(ctx, kit.BackupKey)
	}
	if answer, ok := restoreRefusal(err); ok {
		return answer, nil
	}
	if err != nil {
		return restoreAnswer{}, restoreFailed("resume the restore", err)
	}
	s.deps.Log.Info("restore resumed from the console", "user", requestFrom(ctx).user)
	return restoreAnswer{status: http.StatusAccepted}, nil
}

// uploaded is the path of the uploaded or fetched backup name, which must exist and be a plain file name.
func (s *Server) uploaded(name string) (string, *restoreAnswer, error) {
	if strings.Contains(name, "..") || filepath.Base(name) != name {
		return "", &answerBadName, nil
	}
	file := filepath.Join(s.deps.RestoreDir, name)
	if _, err := os.Stat(file); err != nil {
		return "", &answerNoBackup, nil
	}
	return file, nil, nil
}
