package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
)

const (
	backupsPath       = auth.APIBase + "/backups"
	backupTargetsPath = backupsPath + "/targets"
	// backupTargetSecretPrefix starts the name of a target's secrets Secret in SystemNamespace.
	backupTargetSecretPrefix = "appliance-backup-target-"
	pathBackupTargets        = "spec.backup.targets"
	defaultNFSVersion        = "4.1"
	defaultSSHPort           = 22
	maxBackupTargets         = 16
	maxScheduleLength        = 128
	maxSecretLength          = 64 << 10
	// downloadWriteTimeout bounds each write of a download, in place of the server's write timeout, which would cut
	// off a large backup.
	downloadWriteTimeout = time.Minute

	fieldTimeZone = "timeZone"
)

var (
	// targetTestTimeout bounds a target test, which for NFS starts a pod; the write timeout of a host key scan, which
	// may dial and shake hands for a minute, is extended to it.
	targetTestTimeout = 3 * time.Minute
	// targetTestCleanup is how long a target test may still clean up after its limit.
	targetTestCleanup = target.TestCleanupLimit
)

// answerMargin is the time left to write an answer once the work behind it has ended.
const answerMargin = 10 * time.Second

// targetTestWriteTimeout is the write timeout of a target test: its limit, its cleanup and the answer.
func targetTestWriteTimeout() time.Duration {
	return targetTestTimeout + targetTestCleanup + answerMargin
}

var (
	refusedBackupRunning = ConflictJSONResponse{Code: "backup_running", Message: "A backup is running; try again once it has finished."}
	refusedBackupNoKit   = ConflictJSONResponse{Code: "no_recovery_kit",
		Message: "Issue the recovery kit and confirm that it is stored safely first: backups are encrypted to its key."}
	refusedBackupUpgrade = ConflictJSONResponse{Code: "upgrade_in_progress", Message: "The platform is being upgraded; back up once the upgrade has finished."}
	refusedBackupRestore = ConflictJSONResponse{Code: "restore_in_progress",
		Message: "A restore is in progress or failed before it was done; back up once it has finished or been resumed."}
	refusedBackupNoSpace = ConflictJSONResponse{Code: "no_space",
		Message: "The data volume has too little free space for a backup: it needs twice the size of the last one."}
	backupsUnavailable = BackupsUnavailableJSONResponse{Code: "backups_unavailable", Message: "Backups are not available in this manager."}
	backupNotFound     = NotFoundJSONResponse{Code: codeNotFound, Message: "There is no local backup of that name."}
	targetNotFound     = NotFoundJSONResponse{Code: codeNotFound, Message: "There is no backup target of that name."}
	errTargetNotFound  = errors.New("backup target not configured")
	errTooManyTargets  = errors.New("too many backup targets")

	// hostKeyMessages explain the fixed reasons of a failed host key scan.
	hostKeyMessages = map[string]string{
		target.ReasonUnreachable: "The server cannot be reached from the appliance or through its proxy.",
		target.ReasonTimeout:     "The server did not answer in time.",
	}
	messageNoHostKey = "The server did not present an SSH host key."

	// targetSecretKeys are the keys of a target's Secret by type.
	targetSecretKeys = map[string][]string{
		v1alpha1.BackupTypeS3:   {target.AccessKeyKey, target.SecretKeyKey, target.CAKey},
		v1alpha1.BackupTypeSFTP: {target.PasswordKey, target.PrivateKeyKey},
	}
)

// GetBackups lists the local backups and the targets with their health; target secrets only as set or not set.
func (s *Server) GetBackups(ctx context.Context, _ GetBackupsRequestObject) (GetBackupsResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetBackups409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	out := Backups{Local: []BackupRecord{}, Targets: []BackupTarget{}}
	if st := a.Status.Backup; st != nil {
		out.Running, out.Last = st.Running, backupRecord(st.Last)
	}
	if out.Local, err = s.localBackups(a.Status.Backup); err != nil {
		return nil, err
	}
	for _, spec := range backupTargets(a) {
		t, err := s.backupTarget(ctx, a, spec)
		if err != nil {
			return nil, err
		}
		out.Targets = append(out.Targets, t)
	}
	if c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionBackupHealthy); c != nil {
		healthy := condition(*c)
		out.Healthy = &healthy
	}
	out.LastRestoreTest = timeOf(a.Status.LastRestoreTest)
	return GetBackups200JSONResponse(out), nil
}

// localBackups lists the backup files with the database state the status recorded for each.
func (s *Server) localBackups(st *v1alpha1.BackupStatus) ([]BackupRecord, error) {
	out := []BackupRecord{}
	if s.deps.Backups == nil {
		return out, nil
	}
	records, err := s.deps.Backups.Local()
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	if st != nil {
		for _, rec := range append(slices.Clone(st.Local), ptrSlice(st.Last)...) {
			known[rec.Name] = rec.Database
		}
	}
	for _, rec := range records {
		rec.Database = cmp.Or(known[rec.Name], rec.Database)
		out = append(out, *backupRecord(&rec))
	}
	return out, nil
}

func ptrSlice(rec *v1alpha1.BackupRecord) []v1alpha1.BackupRecord {
	if rec == nil {
		return nil
	}
	return []v1alpha1.BackupRecord{*rec}
}

func backupRecord(rec *v1alpha1.BackupRecord) *BackupRecord {
	if rec == nil {
		return nil
	}
	return &BackupRecord{Name: rec.Name, Time: rec.Time.UTC(), SizeBytes: rec.SizeBytes, Database: BackupRecordDatabase(rec.Database)}
}

func backupTargets(a *v1alpha1.Appliance) []v1alpha1.BackupTarget {
	if a.Spec.Backup == nil {
		return nil
	}
	return a.Spec.Backup.Targets
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// statusBackup is the backup part of the status: the last backup, the health of each target and the last restore
// test.
func (s *Server) statusBackup(a *v1alpha1.Appliance) *StatusBackup {
	out := &StatusBackup{Targets: []BackupTargetHealth{}, LastRestoreTest: timeOf(a.Status.LastRestoreTest)}
	if st := a.Status.Backup; st != nil {
		out.Running, out.Last = st.Running, backupRecord(st.Last)
	}
	for _, spec := range backupTargets(a) {
		out.Targets = append(out.Targets, s.targetHealth(a, spec))
	}
	return out
}

// targetHealth is the state of spec's copies, judged against the API's clock.
func (s *Server) targetHealth(a *v1alpha1.Appliance, spec v1alpha1.BackupTarget) BackupTargetHealth {
	out := BackupTargetHealth{Name: spec.Name}
	var st *v1alpha1.TargetStatus
	if a.Status.Backup != nil {
		if i := slices.IndexFunc(a.Status.Backup.Targets, func(t v1alpha1.TargetStatus) bool { return t.Name == spec.Name }); i >= 0 {
			st = &a.Status.Backup.Targets[i]
		}
	}
	if st != nil {
		out.LastSuccess, out.LastAttempt = timeOf(st.LastSuccess), timeOf(st.LastAttempt)
		if st.LastError != "" {
			reason := BackupTargetReason(st.LastError)
			out.LastError = &reason
		}
	}
	out.Warning = backup.Overdue(spec, st, s.deps.Now())
	return out
}

// backupTarget describes spec with its health and which of its secrets are stored.
func (s *Server) backupTarget(ctx context.Context, a *v1alpha1.Appliance, spec v1alpha1.BackupTarget) (BackupTarget, error) {
	out := BackupTarget{
		Name:      spec.Name,
		Type:      BackupTargetType(spec.Type),
		Schedule:  cmp.Or(spec.Schedule, backup.DefaultSchedule),
		TimeZone:  cmp.Or(spec.TimeZone, backup.DefaultTimeZone),
		Retention: int(spec.Retention),
		Health:    s.targetHealth(a, spec),
	}
	switch {
	case spec.S3 != nil:
		out.S3 = &S3Settings{Endpoint: spec.S3.Endpoint, Bucket: spec.S3.Bucket, Prefix: optional(spec.S3.Prefix), Region: optional(spec.S3.Region)}
		out.SecretsSet.Ca = spec.S3.CA != nil
	case spec.SFTP != nil:
		port := int(spec.SFTP.Port)
		out.Sftp = &SFTPSettings{Host: spec.SFTP.Host, Port: &port, Path: spec.SFTP.Path, User: spec.SFTP.User, HostKey: spec.SFTP.HostKey}
	case spec.NFS != nil:
		version := NFSSettingsVersion(cmp.Or(spec.NFS.Version, defaultNFSVersion))
		out.Nfs = &NFSSettings{Server: spec.NFS.Server, Export: spec.NFS.Export, Path: optional(spec.NFS.Path), Version: &version}
	}
	if spec.CredentialsRef == "" {
		return out, nil
	}
	secret, err := s.systemSecret(ctx, spec.CredentialsRef)
	if err != nil || secret == nil {
		return out, err
	}
	set := func(key string) bool { return len(secret.Data[key]) > 0 }
	out.SecretsSet.AccessKey, out.SecretsSet.SecretKey = set(target.AccessKeyKey), set(target.SecretKeyKey)
	out.SecretsSet.Password, out.SecretsSet.PrivateKey = set(target.PasswordKey), set(target.PrivateKeyKey)
	return out, nil
}

// PostBackups starts a backup to every target. The runner records how it ends and its refusals in the audit log,
// so the request is audited only when the runner did not: refused while a backup runs, or failed to start.
func (s *Server) PostBackups(ctx context.Context, _ PostBackupsRequestObject) (PostBackupsResponseObject, error) {
	req := requestFrom(ctx)
	if s.deps.Backups == nil {
		req.unaudited = true
		return PostBackups503JSONResponse{backupsUnavailable}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostBackups409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range backupTargets(a) {
		names = append(names, t.Name)
	}
	err = s.deps.Backups.Start(ctx, backup.RunManual, names, backup.ByUser(req.user))
	req.unaudited = !errors.Is(err, backup.ErrBusy)
	switch {
	case err == nil:
		s.deps.Log.Info("backup started from the console", "user", req.user, "targets", len(names))
		return PostBackups202Response{}, nil
	case errors.Is(err, backup.ErrBusy):
		recordDetails(ctx, map[string]string{"reason": backup.RunManual, "failed": "refused: Busy"})
		return PostBackups409JSONResponse{ConflictJSONResponse: refusedBackupRunning}, nil
	case errors.Is(err, backup.ErrNoKit):
		return PostBackups409JSONResponse{ConflictJSONResponse: refusedBackupNoKit}, nil
	case errors.Is(err, backup.ErrUpgradeInProgress):
		return PostBackups409JSONResponse{ConflictJSONResponse: refusedBackupUpgrade}, nil
	case errors.Is(err, backup.ErrRestoreInProgress):
		return PostBackups409JSONResponse{ConflictJSONResponse: refusedBackupRestore}, nil
	case errors.Is(err, backup.ErrNoSpace):
		return PostBackups409JSONResponse{ConflictJSONResponse: refusedBackupNoSpace}, nil
	}
	req.unaudited = false
	return nil, fmt.Errorf("start a backup: %s", backup.Describe(err))
}

// GetBackup streams a local backup; only names of the local list are served. Nothing is handed out without its
// audit entry.
func (s *Server) GetBackup(ctx context.Context, req GetBackupRequestObject) (GetBackupResponseObject, error) {
	if s.deps.Backups == nil {
		return GetBackup503JSONResponse{backupsUnavailable}, nil
	}
	f, size, err := s.deps.Backups.Open(req.Name)
	if errors.Is(err, backup.ErrNotFound) {
		return GetBackup404JSONResponse{NotFoundJSONResponse: backupNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the backup: %w", err)
	}
	user, _ := auth.UserFromContext(ctx)
	entry := audit.Entry{
		Time: s.deps.Now().UTC(), User: user, Action: string(AuditEntryActionBackupDownload), Result: string(Success),
		Details: map[string]string{"name": req.Name},
	}
	if err := s.deps.Audit.Write(entry); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("record the backup download: %w", err)
	}
	s.deps.Log.Info("backup downloaded", "name", req.Name, "user", user, "sizeBytes", size)
	return backupDownload{file: f, size: size, name: req.Name}, nil
}

// backupDownload streams a backup file. Each write gets its own deadline, so that the server's write timeout does
// not cut off a large file while a stalled client still cannot hold the connection.
type backupDownload struct {
	file *os.File
	size int64
	name string
}

func (d backupDownload) VisitGetBackupResponse(w http.ResponseWriter) error {
	defer func() { _ = d.file.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(d.size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.name}))
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(&deadlineWriter{w: w, rc: http.NewResponseController(w)}, d.file)
	return err
}

// deadlineWriter sets the write deadline before each write; a writer without deadlines writes without one.
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(downloadWriteTimeout))
	return d.w.Write(p)
}

// withWriteDeadline gives the requests to next d to answer, in place of the server's write timeout.
func withWriteDeadline(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
		next.ServeHTTP(w, r)
	})
}

// GetBackupTarget describes a target; its secrets only as set or not set.
func (s *Server) GetBackupTarget(ctx context.Context, req GetBackupTargetRequestObject) (GetBackupTargetResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetBackupTarget409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	targets := backupTargets(a)
	i := targetIndex(targets, req.Name)
	if i < 0 {
		return GetBackupTarget404JSONResponse{NotFoundJSONResponse: targetNotFound}, nil
	}
	out, err := s.backupTarget(ctx, a, targets[i])
	if err != nil {
		return nil, err
	}
	return GetBackupTarget200JSONResponse(out), nil
}

// PutBackupTarget adds a target or replaces its settings; its secrets go to the target's Secret in SystemNamespace.
func (s *Server) PutBackupTarget(ctx context.Context, req PutBackupTargetRequestObject) (PutBackupTargetResponseObject, error) {
	spec, secrets, ferr := parseBackupTarget(req.Name, req.Body)
	if ferr != nil {
		return PutBackupTarget400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	recordDetails(ctx, map[string]string{"name": spec.Name, "type": spec.Type})
	s.targetMu.Lock()
	defer s.targetMu.Unlock()
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutBackupTarget409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	ferr, err = s.saveBackupTarget(ctx, a, spec, secrets)
	switch {
	case errors.Is(err, errTooManyTargets):
		return PutBackupTarget409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "too_many_targets", Message: fmt.Sprintf("The appliance takes at most %d backup targets.", maxBackupTargets),
		}}, nil
	case err != nil:
		return nil, err
	case ferr != nil:
		return PutBackupTarget400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	return PutBackupTarget202Response{}, nil
}

// saveBackupTarget stores the secrets of spec, keeping the stored ones secrets leaves out, references them from spec
// and puts spec in the Appliance a, read under targetMu. Secrets of another type are dropped. A target the Appliance
// cannot take is refused before any secret is written, and the Secret is put back as it was when the Appliance is not
// updated.
func (s *Server) saveBackupTarget(ctx context.Context, a *v1alpha1.Appliance, spec v1alpha1.BackupTarget, secrets targetSecrets) (*fieldError, error) {
	if list := backupTargets(a); targetIndex(list, spec.Name) < 0 && len(list) >= maxBackupTargets {
		return nil, errTooManyTargets
	}
	name := backupTargetSecretName(spec.Name)
	stored, err := s.systemSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	data := mergeTargetSecrets(spec.Type, stored, secrets, relocated(backupTargets(a), spec))
	if ferr := missingSecret(spec.Type, data); ferr != nil {
		return ferr, nil
	}
	written, err := s.writeTargetSecret(ctx, name, stored, data, target.Location(spec))
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		spec.CredentialsRef = name
		if len(data[target.CAKey]) > 0 {
			spec.S3.CA = &v1alpha1.SecretKeyRef{Name: name, Key: target.CAKey}
		}
	}
	if err := s.putBackupTarget(ctx, spec); err != nil {
		if written {
			s.restoreTargetSecret(ctx, name, stored)
		}
		return nil, err
	}
	if len(data) == 0 && stored != nil {
		if err := s.deleteSystemSecret(ctx, name); err != nil {
			return nil, err
		}
	}
	recordPaths(ctx, []string{pathBackupTargets})
	s.deps.Log.Info("backup target saved", "target", spec.Name, "type", spec.Type)
	return nil, nil
}

// mergeTargetSecrets are the secrets of a target of type typ: the stored ones it keeps, replaced by those set. A
// relocated target keeps no stored credential, only its CA bundle, which is never sent anywhere.
func mergeTargetSecrets(typ string, stored *corev1.Secret, secrets targetSecrets, relocated bool) map[string][]byte {
	data := map[string][]byte{}
	for _, key := range targetSecretKeys[typ] {
		keep := !relocated || key == target.CAKey
		if keep && stored != nil && len(stored.Data[key]) > 0 && !slices.Contains(secrets.drop, key) {
			data[key] = stored.Data[key]
		}
		if value := secrets.set[key]; value != "" {
			data[key] = []byte(value)
		}
	}
	return data
}

// relocated tells whether spec sends its credentials elsewhere than the target of its name in list: to another
// type, S3 endpoint, or SFTP host, port or user. A new target is not relocated.
func relocated(list []v1alpha1.BackupTarget, spec v1alpha1.BackupTarget) bool {
	i := targetIndex(list, spec.Name)
	return i >= 0 && target.Location(list[i]) != target.Location(spec)
}

// writeTargetSecret writes data for the server location to the Secret name unless data is empty or stored holds it
// already; it tells whether it wrote.
func (s *Server) writeTargetSecret(ctx context.Context, name string, stored *corev1.Secret, data map[string][]byte, location string) (bool, error) {
	if len(data) == 0 || stored != nil && stored.Annotations[target.LocationAnnotation] == location &&
		maps.EqualFunc(stored.Data, data, func(a, b []byte) bool { return string(a) == string(b) }) {
		return false, nil
	}
	// putTargetSecret changes the Secret it is given; stored stays as read, for restoreTargetSecret.
	if err := s.putTargetSecret(ctx, stored.DeepCopy(), name, data, location); err != nil {
		return false, err
	}
	return true, nil
}

// putTargetSecret is putSystemSecret for the target Secret name, recording the server location its data was entered
// for.
func (s *Server) putTargetSecret(ctx context.Context, secret *corev1.Secret, name string, data map[string][]byte, location string) error {
	if secret == nil {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Type: corev1.SecretTypeOpaque}
	}
	metav1.SetMetaDataAnnotation(&secret.ObjectMeta, target.LocationAnnotation, location)
	return s.putSystemSecret(ctx, secret, name, corev1.SecretTypeOpaque, data)
}

// putBackupTarget replaces the target of spec's name in the Appliance, or adds it.
func (s *Server) putBackupTarget(ctx context.Context, spec v1alpha1.BackupTarget) error {
	return s.editBackupTargets(ctx, func(list []v1alpha1.BackupTarget) ([]v1alpha1.BackupTarget, error) {
		if i := targetIndex(list, spec.Name); i >= 0 {
			list[i] = spec
			return list, nil
		}
		if len(list) >= maxBackupTargets {
			return nil, errTooManyTargets
		}
		return append(list, spec), nil
	})
}

// restoreTargetSecret puts the target Secret name back as stored, deleting it when there was none, after the
// Appliance refused the target that referenced it. A failure is logged with its Kubernetes reason only.
func (s *Server) restoreTargetSecret(ctx context.Context, name string, stored *corev1.Secret) {
	ctx = context.WithoutCancel(ctx)
	var err error
	if stored == nil {
		err = s.deleteSystemSecret(ctx, name)
	} else {
		var current *corev1.Secret
		if current, err = s.systemSecret(ctx, name); err == nil {
			err = s.putTargetSecret(ctx, current, name, stored.Data, stored.Annotations[target.LocationAnnotation])
		}
	}
	if err != nil {
		s.deps.Log.Error("could not restore the secrets of a backup target that was not saved", "secret", name, "failure", apierrors.ReasonForError(err))
	}
}

// missingSecret refuses a target without the secrets its type needs.
func missingSecret(typ string, data map[string][]byte) *fieldError {
	switch {
	case typ == v1alpha1.BackupTypeS3 && len(data[target.AccessKeyKey]) == 0:
		return &fieldError{"s3.accessKey", "Enter the access key."}
	case typ == v1alpha1.BackupTypeS3 && len(data[target.SecretKeyKey]) == 0:
		return &fieldError{"s3.secretKey", "Enter the secret key."}
	case typ == v1alpha1.BackupTypeSFTP && len(data[target.PasswordKey]) == 0 && len(data[target.PrivateKeyKey]) == 0:
		return &fieldError{"sftp.password", "Enter a password or a private key."}
	}
	return nil
}

// DeleteBackupTarget removes a target, its secrets and its state; the copies on the target stay.
func (s *Server) DeleteBackupTarget(ctx context.Context, req DeleteBackupTargetRequestObject) (DeleteBackupTargetResponseObject, error) {
	recordDetails(ctx, map[string]string{"name": req.Name})
	s.targetMu.Lock()
	defer s.targetMu.Unlock()
	err := s.editBackupTargets(ctx, func(list []v1alpha1.BackupTarget) ([]v1alpha1.BackupTarget, error) {
		i := targetIndex(list, req.Name)
		if i < 0 {
			return nil, errTargetNotFound
		}
		return slices.Delete(list, i, i+1), nil
	})
	switch {
	case apierrors.IsNotFound(err):
		return DeleteBackupTarget409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case errors.Is(err, errTargetNotFound):
		return DeleteBackupTarget404JSONResponse{NotFoundJSONResponse: targetNotFound}, nil
	case err != nil:
		return nil, err
	}
	if err := s.deleteSystemSecret(ctx, backupTargetSecretName(req.Name)); err != nil {
		return nil, err
	}
	recordPaths(ctx, []string{pathBackupTargets})
	if err := s.forgetTargetStatus(ctx, req.Name); err != nil {
		// A target added again under the name shows the old state until its next copy.
		s.deps.Log.Warn("could not remove the state of the removed backup target", "target", req.Name, "failure", apierrors.ReasonForError(err))
	}
	s.deps.Log.Info("backup target removed", "target", req.Name)
	return DeleteBackupTarget202Response{}, nil
}

// forgetTargetStatus removes the state of the target name from the status.
func (s *Server) forgetTargetStatus(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		if a.Status.Backup == nil {
			return nil
		}
		i := slices.IndexFunc(a.Status.Backup.Targets, func(t v1alpha1.TargetStatus) bool { return t.Name == name })
		if i < 0 {
			return nil
		}
		a.Status.Backup.Targets = slices.Delete(a.Status.Backup.Targets, i, i+1)
		return s.deps.Client.Status().Update(ctx, a, client.FieldOwner(FieldManager))
	})
}

// editBackupTargets replaces the targets of the Appliance by edit's result, given a copy of them. An error of edit
// leaves the Appliance as it is and is returned.
func (s *Server) editBackupTargets(ctx context.Context, edit func([]v1alpha1.BackupTarget) ([]v1alpha1.BackupTarget, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		next, err := edit(slices.Clone(backupTargets(a)))
		if err != nil {
			return err
		}
		if len(next) == 0 {
			a.Spec.Backup = nil
		} else {
			a.Spec.Backup = &v1alpha1.BackupSpec{Targets: next}
		}
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
}

// PostBackupTargetTest writes, reads back and deletes a test file on a saved target.
func (s *Server) PostBackupTargetTest(ctx context.Context, req PostBackupTargetTestRequestObject) (PostBackupTargetTestResponseObject, error) {
	recordDetails(ctx, map[string]string{"name": req.Name})
	if s.deps.Backups == nil {
		return PostBackupTargetTest503JSONResponse{backupsUnavailable}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostBackupTargetTest409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	if targetIndex(backupTargets(a), req.Name) < 0 {
		return PostBackupTargetTest404JSONResponse{NotFoundJSONResponse: targetNotFound}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, targetTestTimeout)
	defer cancel()
	err = s.deps.Backups.TestTarget(ctx, req.Name)
	if errors.Is(err, backup.ErrNotFound) {
		return PostBackupTargetTest404JSONResponse{NotFoundJSONResponse: targetNotFound}, nil
	}
	out := BackupTargetTest{Ok: err == nil}
	result := "ok"
	if err != nil {
		reason := BackupTargetReason(target.Reason(err))
		out.Reason, result = &reason, string(reason)
	}
	recordDetails(ctx, map[string]string{"name": req.Name, "result": result})
	if !out.Ok {
		requestFrom(ctx).result = Failure
	}
	return PostBackupTargetTest200JSONResponse(out), nil
}

// PostBackupHostKey reads the SSH host key fingerprint of a server through the appliance's proxy.
func (s *Server) PostBackupHostKey(ctx context.Context, req PostBackupHostKeyRequestObject) (PostBackupHostKeyResponseObject, error) {
	if s.deps.Backups == nil {
		return PostBackupHostKey503JSONResponse{backupsUnavailable}, nil
	}
	if req.Body == nil || !validHost(req.Body.Host) {
		return PostBackupHostKey400JSONResponse{BadRequestJSONResponse: badRequestField("host", "Enter the server's host name or IP address.")}, nil
	}
	port := defaultSSHPort
	if req.Body.Port != nil {
		port = *req.Body.Port
	}
	recordDetails(ctx, map[string]string{"host": req.Body.Host, "port": strconv.Itoa(port)})
	if port < 1 || port > 65535 {
		return PostBackupHostKey400JSONResponse{BadRequestJSONResponse: badRequestField("port", "Enter a port between 1 and 65535.")}, nil
	}
	_, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PostBackupHostKey409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	fingerprint, err := s.deps.Backups.ScanHostKey(ctx, req.Body.Host, int32(port)) //nolint:gosec // checked to be a port above
	if err != nil {
		reason := target.Reason(err)
		s.deps.Log.Info("SSH host key not read", "host", req.Body.Host, "port", port, "failure", reason)
		message, ok := hostKeyMessages[reason]
		if !ok {
			message = messageNoHostKey
		}
		return PostBackupHostKey502JSONResponse{Code: reason, Message: message}, nil
	}
	return PostBackupHostKey200JSONResponse{Fingerprint: fingerprint}, nil
}

func targetIndex(list []v1alpha1.BackupTarget, name string) int {
	return slices.IndexFunc(list, func(t v1alpha1.BackupTarget) bool { return t.Name == name })
}

// backupTargetSecretName is the Secret in SystemNamespace with the secrets of the target name.
func backupTargetSecretName(name string) string {
	return backupTargetSecretPrefix + name
}
