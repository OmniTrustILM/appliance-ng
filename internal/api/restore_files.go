package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
)

const (
	// maxRestoreUpload caps an uploaded backup.
	maxRestoreUpload = 1 << 40
	// streamTimeout bounds each read of an upload and the answer after the last one, in place of the server's
	// timeouts, which would cut off a large backup.
	streamTimeout = time.Minute
	// restoreTargetName names a target entered for a restore, which is never saved.
	restoreTargetName = "restore"
	restoreDirMode    = 0o700
	restoreFileMode   = 0o600
	partFileSuffix    = ".part"
	detailType        = "type"
	actionFetch       = "fetch"
)

var (
	unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

	answerBadName = refusedField(codeInvalid, partName,
		"Choose a backup file with a name ending in .ilmbackup; it is stored with every character other than a letter, a digit, '.', '_' and '-' replaced by '_'.")
	answerNoSize  = refused(http.StatusBadRequest, codeInvalid, "Send the backup's size in Content-Length; a backup is at most 1 TiB.")
	answerPartial = refused(http.StatusBadRequest, "incomplete", "The backup did not arrive completely; upload it again.")
	answerNoSpace = refused(http.StatusConflict, "no_space",
		"The data volume has too little free space for the backup: it needs twice the backup's size.")
	answerNoTargets = restoreAnswer{status: http.StatusServiceUnavailable, body: Error(backupsUnavailable)}
	answerNotListed = refused(http.StatusNotFound, codeNotFound, "The target holds no backup of that name.")

	// targetMessages explain the fixed reasons of a target that failed during a restore.
	targetMessages = map[string]string{
		target.ReasonUnreachable:     "The target cannot be reached from the appliance or through its proxy.",
		target.ReasonAuthFailed:      "The target refused the credentials.",
		target.ReasonHostKeyMismatch: "The server's host key does not match the accepted one.",
		target.ReasonNotFound:        "The bucket, directory or export does not exist.",
		target.ReasonNoSpace:         "The target has no space left.",
		target.ReasonTimeout:         "The target did not answer in time.",
		target.ReasonFailed:          "The target could not be read.",
	}

	// errSource marks a failure to read the backup being received.
	errSource = errors.New("the backup could not be read")
)

// storedName is the name a backup to restore named name is stored under: every character other than a letter, a
// digit, '.', '_' and '-' replaced by '_', and a leading '.' too. False when name does not end in .ilmbackup after
// at least one character.
func storedName(name string) (string, bool) {
	stem, ok := strings.CutSuffix(name, backup.FileSuffix)
	if !ok || stem == "" {
		return "", false
	}
	safe := unsafeNameChars.ReplaceAllString(stem, "_")
	if strings.HasPrefix(safe, ".") {
		safe = "_" + safe[1:]
	}
	return safe + backup.FileSuffix, true
}

// targetFailed answers the failure of a target with its fixed reason; the driver's error text stays in the manager.
func (s *Server) targetFailed(action string, err error) restoreAnswer {
	reason := target.Reason(err)
	s.deps.Log.Info("restore target failed", "action", action, "failure", reason)
	return refused(http.StatusBadGateway, reason, targetMessages[reason])
}

// withStreamDeadlines lets an upload take as long as it keeps sending: each read of the body moves the read and
// write deadlines on.
func withStreamDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &deadlineReader{body: r.Body, rc: http.NewResponseController(w)}
		next.ServeHTTP(w, r)
	})
}

// deadlineReader sets the read and write deadlines before each read; a connection without deadlines reads without.
type deadlineReader struct {
	body io.ReadCloser
	rc   *http.ResponseController
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	deadline := time.Now().Add(streamTimeout)
	_ = d.rc.SetReadDeadline(deadline)
	_ = d.rc.SetWriteDeadline(deadline)
	return d.body.Read(p)
}

func (d *deadlineReader) Close() error { return d.body.Close() }

// PutRestoreUpload stores the uploaded backup as the one to restore.
func (s *Server) PutRestoreUpload(ctx context.Context, req PutRestoreUploadRequestObject) (PutRestoreUploadResponseObject, error) {
	name, ok := storedName(req.Params.Name)
	size := requestFrom(ctx).contentLength
	details := map[string]string{detailBackup: req.Params.Name}
	switch {
	case !ok:
		return answered(ctx, details, answerBadName), nil
	case size < 0 || size > maxRestoreUpload:
		return answered(ctx, details, answerNoSize), nil
	}
	answer, err := s.receive(ctx, func(keep string) (restoreAnswer, error) {
		err := s.storeBackup(name, size, req.Body, keep)
		if errors.Is(err, errSource) {
			return answerPartial, nil
		}
		return stored(err)
	})
	return answered(ctx, details, answer), err
}

// stored answers the storing of a backup that failed with err, or succeeded when err is nil.
func stored(err error) (restoreAnswer, error) {
	switch {
	case err == nil:
		return restoreAnswer{status: http.StatusNoContent}, nil
	case errors.Is(err, storage.ErrNoSpace):
		return answerNoSpace, nil
	}
	return restoreAnswer{}, err
}

// receive runs store, one upload or fetch at a time, while the appliance waits for a backup to restore. store gets
// the file a failed restore resumes from, which it keeps.
func (s *Server) receive(ctx context.Context, store func(keep string) (restoreAnswer, error)) (restoreAnswer, error) {
	if !s.restoreMu.TryLock() {
		return answerRestoreBusy, nil
	}
	defer s.restoreMu.Unlock()
	keep, answer, err := s.waitsForBackup(ctx)
	if answer != nil || err != nil {
		return orFailure(answer), err
	}
	return store(keep)
}

// waitsForBackup refuses unless the appliance is a fresh one waiting for a backup and no restore runs. It returns
// the file of a restore that failed and can be resumed.
func (s *Server) waitsForBackup(ctx context.Context) (string, *restoreAnswer, error) {
	a, answer, err := s.restoreAppliance(ctx, false)
	if answer != nil || err != nil {
		return "", answer, err
	}
	st := a.Status.Restore
	switch {
	case st.Active():
		return "", &answerRestoreBusy, nil
	case st.Unsettled():
		return st.File, nil, nil
	}
	return "", nil, nil
}

// storeBackup replaces the backup to restore by name, size bytes read from r. The earlier ones but keep are removed
// and the free space checked before anything is read; the file is written under a temporary name and renamed once
// complete. A failure to read r wraps errSource; a full volume storage.ErrNoSpace.
func (s *Server) storeBackup(name string, size int64, r io.Reader, keep string) error {
	s.forgetInspection()
	dir := s.deps.RestoreDir
	if err := os.MkdirAll(dir, restoreDirMode); err != nil {
		return fmt.Errorf("create the restore directory: %w", err)
	}
	if err := removeAllBut(dir, keep); err != nil {
		return fmt.Errorf("remove the earlier backup to restore: %w", err)
	}
	if err := s.checkSpace(dir, uint64(size)); err != nil { //nolint:gosec // checked to be positive by the callers
		return err
	}
	part := filepath.Join(dir, name+partFileSuffix)
	if err := writeFile(part, size, r); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("store the backup to restore: %w", err)
	}
	s.deps.Log.Info("backup to restore stored", "backup", name, "sizeBytes", size)
	return nil
}

// removeAllBut removes what dir holds except the file keep.
func removeAllBut(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if path := filepath.Join(dir, e.Name()); path != keep {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeFile writes size bytes of r to path and syncs them.
func writeFile(path string, size int64, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, restoreFileMode)
	if err != nil {
		return fmt.Errorf("create the backup to restore: %w", err)
	}
	defer func() { _ = f.Close() }()
	src := &sourceReader{r: io.LimitReader(r, size)}
	n, err := io.Copy(f, src)
	switch {
	case src.err != nil:
		return fmt.Errorf("%w: %w", errSource, src.err)
	case errors.Is(err, syscall.ENOSPC):
		return fmt.Errorf("%w: %w", storage.ErrNoSpace, err)
	case err != nil:
		return fmt.Errorf("write the backup to restore: %w", err)
	case n != size:
		return fmt.Errorf("%w: %d of %d bytes", errSource, n, size)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write the backup to restore: %w", err)
	}
	return f.Close()
}

// sourceReader keeps the error of reading r, to tell it from an error of writing.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// PostRestoreList lists the backups on a target entered for the restore.
func (s *Server) PostRestoreList(ctx context.Context, req PostRestoreListRequestObject) (PostRestoreListResponseObject, error) {
	var body *BackupTargetUpdate
	if req.Body != nil {
		body = &req.Body.Target
	}
	t, answer, err := s.enteredTarget(ctx, body)
	if answer != nil || err != nil {
		return orFailure(answer), err
	}
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	objects, err := t.List(ctx)
	if err != nil {
		return s.targetFailed("list", err), nil
	}
	slices.SortFunc(objects, func(a, b target.Object) int {
		return cmp.Or(b.Modified.Compare(a.Modified), cmp.Compare(a.Name, b.Name))
	})
	out := RestoreFiles{Files: make([]RestoreFile, len(objects))}
	for i, o := range objects {
		out.Files[i] = RestoreFile{Name: o.Name, SizeBytes: o.Size, Modified: o.Modified.UTC()}
	}
	return restoreAnswer{status: http.StatusOK, body: out}, nil
}

// PostRestoreFetch copies a backup from a target entered for the restore as the one to restore.
func (s *Server) PostRestoreFetch(ctx context.Context, req PostRestoreFetchRequestObject) (PostRestoreFetchResponseObject, error) {
	if req.Body == nil {
		return answered(ctx, map[string]string{}, answerBadName), nil
	}
	name := req.Body.Name
	details := map[string]string{detailBackup: name, detailType: string(req.Body.Target.Type)}
	as, ok := storedName(name)
	if !ok {
		return answered(ctx, details, answerBadName), nil
	}
	t, answer, err := s.enteredTarget(ctx, &req.Body.Target)
	if answer != nil || err != nil {
		return answered(ctx, details, orFailure(answer)), err
	}
	ctx, cancel := context.WithTimeout(ctx, restoreWorkTimeout)
	defer cancel()
	fetched, err := s.receive(ctx, func(keep string) (restoreAnswer, error) { return s.fetch(ctx, t, name, as, keep) })
	return answered(ctx, details, fetched), err
}

// fetch copies name from t to the restore directory as the file named as, keeping the file keep.
func (s *Server) fetch(ctx context.Context, t target.Target, name, as, keep string) (restoreAnswer, error) {
	objects, err := t.List(ctx)
	if err != nil {
		return s.targetFailed(actionFetch, err), nil
	}
	i := slices.IndexFunc(objects, func(o target.Object) bool { return o.Name == name })
	if i < 0 {
		return answerNotListed, nil
	}
	r, err := t.Get(ctx, name)
	if err != nil {
		return s.targetFailed(actionFetch, err), nil
	}
	defer func() { _ = r.Close() }()
	err = s.storeBackup(as, objects[i].Size, r, keep)
	if errors.Is(err, errSource) {
		return s.targetFailed(actionFetch, err), nil
	}
	return stored(err)
}

// enteredTarget opens the target of body, entered on the restore page; its secrets serve this request only.
func (s *Server) enteredTarget(ctx context.Context, body *BackupTargetUpdate) (target.Target, *restoreAnswer, error) {
	if s.deps.Backups == nil {
		return nil, &answerNoTargets, nil
	}
	spec, secrets, ferr := parseBackupTarget(restoreTargetName, body)
	data := map[string][]byte{}
	for key, value := range secrets.set {
		data[key] = []byte(value)
	}
	if ferr == nil {
		ferr = missingSecret(spec.Type, data)
	}
	if ferr != nil {
		answer := refusedField(codeInvalid, ferr.field, ferr.message)
		return nil, &answer, nil
	}
	if _, answer, err := s.waitsForBackup(ctx); answer != nil || err != nil {
		return nil, answer, err
	}
	t, err := s.deps.Backups.EnteredTarget(ctx, spec, data)
	if apierrors.IsNotFound(err) {
		answer := restoreAnswer{status: http.StatusConflict, body: Error(notSetUp())}
		return nil, &answer, nil
	}
	if err != nil {
		answer := s.targetFailed("open", err)
		return nil, &answer, nil
	}
	return t, nil, nil
}
