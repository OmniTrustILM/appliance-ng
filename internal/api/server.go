// Package api serves the Appliance Manager's REST API, which api/openapi.yaml describes.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/backup/target"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/mail"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
	"github.com/OmniTrustILM/appliance-ng/internal/storage"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	// SystemNamespace holds the Appliance's own Secrets.
	SystemNamespace = kube.SystemNamespace
	// FieldManager is the field manager of the API's writes.
	FieldManager = kube.ManagerFieldManager

	// maxBody caps the size of a request body before it is decoded.
	maxBody = 16 << 10
	// maxPasswordChecks bounds the password checks that run at once; each takes 64 MiB of the manager's memory.
	maxPasswordChecks = 1
	// namePath ends the path of a named item.
	namePath = "/{name}"
)

// AuditStore records the changes made through the API.
type AuditStore interface {
	Write(e audit.Entry) error
	// Page returns up to n entries, newest first, written before the cursor before, and the cursor of the next page.
	Page(before string, n int) (entries []audit.Entry, next string, err error)
}

// BackupRunner takes backups, serves the local ones and reaches the targets.
type BackupRunner interface {
	Run(ctx context.Context, reason string, targets []string, opts ...backup.RunOption) (v1alpha1.BackupRecord, error)
	// Start makes the checks of Run and takes the backup in the background.
	Start(ctx context.Context, reason string, targets []string, opts ...backup.RunOption) error
	Local() ([]v1alpha1.BackupRecord, error)
	Open(name string) (*os.File, int64, error)
	TestTarget(ctx context.Context, name string) error
	ScanHostKey(ctx context.Context, host string, port int32) (string, error)
	// EnteredTarget opens a target entered for one request with the given credentials; nothing is saved.
	EnteredTarget(ctx context.Context, spec v1alpha1.BackupTarget, creds map[string][]byte) (target.Target, error)
}

// Restorer restores a backup onto the appliance.
type Restorer interface {
	Start(ctx context.Context, file string, id age.Identity, opts restore.Options) error
	Resume(ctx context.Context, id age.Identity) error
}

// Deps is what the Server needs. Now defaults to time.Now; without Talos, reboot and shutdown are unavailable;
// without Backups, no backup follows a confirmed recovery kit and the backup operations are unavailable; everything
// else must be set.
type Deps struct {
	// Client reads the Appliance and the Secrets in SystemNamespace, possibly from a cache, and writes them.
	Client client.Client
	// Reader reads past the cache: the Secrets the API writes and those in the platform namespace.
	Reader client.Reader
	// Kube requests tokens and reads the token Secrets of the kubeconfigs the API issues.
	Kube kubernetes.Interface
	// ClusterCA is the PEM CA of the cluster's API server, which issued kubeconfigs trust.
	ClusterCA []byte
	Users     auth.UserStore
	Audit     AuditStore
	Release   *release.Manifest
	Log       *slog.Logger
	Now       func() time.Time
	// Talos returns the Talos API of the node, or an error while it cannot be reached.
	Talos func(context.Context) (talos.Node, error)
	// Backups takes a backup to every target once a recovery kit is confirmed and serves the backup operations.
	Backups BackupRunner
	// Restore restores a backup onto the appliance.
	Restore Restorer
	// RestoreDir keeps the backup uploaded or fetched for a restore.
	RestoreDir string
	// CheckDatabase opens a session on an external database as username with password, verifying the server
	// against the PEM ca when set, as backup.CheckExternal does; without it a database test fails.
	CheckDatabase func(ctx context.Context, db backup.ExternalDatabase, username, password string, ca []byte) error
}

// Server serves the manager's API under auth.APIBase.
type Server struct {
	deps     Deps
	sessions *auth.Sessions
	limiter  *auth.Limiter
	guard    auth.TOTPGuard

	// checkPassword is auth.CheckPassword; tests replace it.
	checkPassword func(hash, pw string) bool
	// hashSlots bounds the password checks that run at once.
	hashSlots auth.HashSlots
	// dummyHash is checked against for an unknown user, so that the answer takes as long as for a known one.
	dummyHash string
	// sendMail is mail.SendTest; tests replace it.
	sendMail sendFunc
	// kitMu keeps recovery kit changes one at a time.
	kitMu sync.Mutex
	// targetMu keeps backup target changes one at a time, so that a target's settings and its Secret change together.
	targetMu sync.Mutex
	// restoreMu keeps the uploads, fetches, inspections and resumes of a restore one at a time.
	restoreMu sync.Mutex
	// inspectMu guards inspected.
	inspectMu sync.Mutex
	// inspected is the backup checked last, for the session that checked it.
	inspected *inspection
	// checkSpace is storage.Check; tests replace it.
	checkSpace func(path string, expected uint64) error
	// identityClient returns the HTTP client that reaches identity providers; tests replace it.
	identityClient func(ctx context.Context, a *v1alpha1.Appliance) (*http.Client, error)
	// openBroker is dialBroker; tests replace it.
	openBroker func(ctx context.Context, mq v1alpha1.ExternalMessaging, username, password string, ca []byte) error
}

// NewServer returns the API server on deps.
func NewServer(deps Deps) *Server {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	s := &Server{
		deps:          deps,
		sessions:      auth.NewSessions(deps.Now).WithUserCheck(accountExists(deps.Users)),
		limiter:       auth.SignInLimiter(),
		checkPassword: auth.CheckPassword,
		hashSlots:     auth.NewHashSlots(maxPasswordChecks),
		dummyHash:     newDummyHash(),
		sendMail:      mail.SendTest,
		checkSpace:    storage.Check,
	}
	s.identityClient, s.openBroker = s.identityHTTPClient, dialBroker
	return s
}

// accountExists returns the check that keeps a session only while its user is a console account. A failed read
// keeps the session, so that a passing error does not sign everyone out.
func accountExists(users auth.UserStore) func(ctx context.Context, user string) bool {
	return func(ctx context.Context, user string) bool {
		if users == nil {
			return true
		}
		_, err := users.Get(ctx, user)
		return !errors.Is(err, auth.ErrUserNotFound)
	}
}

// Handler returns the API's routes. A change with a live session and a sign-in the limiter let through write one
// audit entry each; requests refused for lack of a session or while sign-in is locked write none, so that
// anonymous requests cannot flood the audit log.
func (s *Server) Handler() http.Handler {
	badRequest := func(w http.ResponseWriter, _ *http.Request, _ error) {
		auth.WriteError(w, http.StatusBadRequest, "invalid_request", "The request is not valid.")
	}
	strict := NewStrictHandlerWithOptions(s, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			s.deps.Log.Error("API request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			auth.WriteError(w, http.StatusInternalServerError, "internal", "The request failed; the manager log has the details.")
		},
	})
	api := HandlerWithOptions(strict, StdHTTPServerOptions{BaseURL: auth.APIBase, ErrorHandlerFunc: badRequest})
	protected := s.sessions.Middleware(api)
	change := func(action AuditEntryAction) http.Handler {
		return s.withSession(s.audited(action, protected))
	}

	mux := http.NewServeMux()
	mux.Handle(route(http.MethodGet, auth.APIBase+"/ca.pem"), api)
	mux.Handle(route(http.MethodPost, auth.APIBase+"/session"), s.audited(AuditEntryActionSignIn, api))
	mux.Handle(route(http.MethodDelete, auth.APIBase+"/session"), change(AuditEntryActionSignOut))
	mux.Handle(route(http.MethodPut, auth.APIBase+"/wizard/platform"), change(AuditEntryActionWizardPlatform))
	mux.Handle(route(http.MethodPost, auth.APIBase+"/platform/admin-credential"), change(AuditEntryActionAdminCredential))
	mux.Handle(route(http.MethodPut, auth.APIBase+"/node"), change(AuditEntryActionNodeSettings))
	mux.Handle(route(http.MethodPost, auth.APIBase+"/system/reboot"), change(AuditEntryActionReboot))
	mux.Handle(route(http.MethodPost, auth.APIBase+"/system/shutdown"), change(AuditEntryActionShutdown))
	mux.Handle(route(http.MethodPut, certificatePath), change(AuditEntryActionCertificateUpload))
	mux.Handle(route(http.MethodPost, certificatePath+"/csr"), change(AuditEntryActionCertificateCsr))
	mux.Handle(route(http.MethodPut, certificatePath+"/csr/signed"), change(AuditEntryActionCertificateSigned))
	mux.Handle(route(http.MethodPut, certificatePath+"/builtin"), change(AuditEntryActionCertificateBuiltin))
	mux.Handle(route(http.MethodPost, trustPath), change(AuditEntryActionTrustAdd))
	mux.Handle(route(http.MethodDelete, trustPath+"/{sha256}"), change(AuditEntryActionTrustRemove))
	mux.Handle(route(http.MethodPut, registriesPath+"/{host}"), change(AuditEntryActionRegistrySet))
	mux.Handle(route(http.MethodDelete, registriesPath+"/{host}"), change(AuditEntryActionRegistryRemove))
	mux.Handle(route(http.MethodPut, platformPath), change(AuditEntryActionPlatformSettings))
	mux.Handle(route(http.MethodPost, platformPath+"/dependencies/test"), withWriteDeadline(dependencyTestTimeout+answerMargin, change(AuditEntryActionDependencyTest)))
	mux.Handle(route(http.MethodPut, mailPath), change(AuditEntryActionMailSettings))
	mux.Handle(route(http.MethodDelete, mailPath), change(AuditEntryActionMailRemove))
	mux.Handle(route(http.MethodPost, mailPath+"/test"), change(AuditEntryActionMailTest))
	mux.Handle(route(http.MethodPost, kubeconfigPath), change(AuditEntryActionKubeconfigIssue))
	mux.Handle(route(http.MethodDelete, kubeconfigPath+namePath), change(AuditEntryActionKubeconfigRevoke))
	mux.Handle(route(http.MethodPost, talosconfigPath), change(AuditEntryActionTalosconfigIssue))
	mux.Handle(route(http.MethodPost, recoveryKitPath), change(AuditEntryActionRecoveryKitIssue))
	mux.Handle(route(http.MethodPost, recoveryKitPath+"/reissue"), change(AuditEntryActionRecoveryKitReissue))
	mux.Handle(route(http.MethodPost, recoveryKitPath+"/replace"), change(AuditEntryActionRecoveryKitReplace))
	mux.Handle(route(http.MethodPost, recoveryKitPath+"/confirm"), change(AuditEntryActionRecoveryKitConfirm))
	mux.Handle(route(http.MethodPost, backupsPath), change(AuditEntryActionBackup))
	mux.Handle(route(http.MethodPut, backupTargetsPath+namePath), change(AuditEntryActionBackupTargetSet))
	mux.Handle(route(http.MethodDelete, backupTargetsPath+namePath), change(AuditEntryActionBackupTargetRemove))
	mux.Handle(route(http.MethodPost, backupTargetsPath+"/hostkey"), withWriteDeadline(targetTestTimeout, change(AuditEntryActionBackupHostKeyScan)))
	mux.Handle(route(http.MethodPost, backupTargetsPath+"/{name}/test"), withWriteDeadline(targetTestWriteTimeout(), change(AuditEntryActionBackupTargetTest)))
	mux.Handle(route(http.MethodPut, restoreUploadPath), withStreamDeadlines(change(AuditEntryActionRestoreUpload)))
	mux.Handle(route(http.MethodPost, restorePath+"/fetch"), withWriteDeadline(restoreWorkTimeout+answerMargin, change(AuditEntryActionRestoreFetch)))
	mux.Handle(route(http.MethodPost, restorePath+"/inspect"), withWriteDeadline(restoreWorkTimeout+answerMargin, change(AuditEntryActionRestoreInspect)))
	mux.Handle(route(http.MethodPost, restorePath+"/start"), withWriteDeadline(restoreWorkTimeout+answerMargin, change(AuditEntryActionRestoreStart)))
	mux.Handle(route(http.MethodPost, restorePath+"/resume"), withWriteDeadline(restoreWorkTimeout+answerMargin, change(AuditEntryActionRestoreResume)))
	mux.Handle(route(http.MethodPost, restorePath+"/list"), withWriteDeadline(targetTestWriteTimeout(), protected))
	mux.Handle(auth.BasePath+"/api/", protected)
	return withRequest(mux)
}

// request is what the handlers need of the HTTP request that the strict server does not pass on.
type request struct {
	// peer is the IP address of the TCP peer; the manager is reached directly, so no proxy header is trusted.
	peer string
	// token is the session cookie's value.
	token string
	// user is the user of the request's live session, for a change.
	user string
	// entry is the audit entry of a request that changes something; nil for other requests.
	entry *audit.Entry
	// unaudited is set by a handler whose request must not be audited.
	unaudited bool
	// written is the result of an entry the handler wrote itself.
	written AuditEntryResult
	// result, when set, is the audit result of an answer that reports a failure with a success status.
	result AuditEntryResult
	// contentLength is the request body's declared size; -1 when unknown.
	contentLength int64
}

type requestKey struct{}

// route is the mux pattern of method on path.
func route(method, path string) string {
	return method + " " + path
}

// withRequest keeps API answers out of caches and from being read as another content type, caps the request body
// and puts the request's details in its context.
func withRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r.URL.Path))
		req := &request{peer: r.RemoteAddr, contentLength: r.ContentLength}
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			req.peer = host
		}
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			req.token = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, req)))
	})
}

// bodyLimit is the largest request body accepted for path: certificate uploads carry chains, trust uploads CA
// bundles, re-issuing the recovery kit uploads the kit, backup targets carry private keys and CA bundles, and a
// restore takes the kit, a target and the backup itself.
func bodyLimit(path string) int64 {
	if path == restoreUploadPath {
		return maxRestoreUpload
	}
	for _, prefix := range []string{certificatePath, trustPath, recoveryKitPath + "/reissue", backupTargetsPath, restorePath} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return maxUploadBody
		}
	}
	return maxBody
}

func requestFrom(ctx context.Context) *request {
	if req, ok := ctx.Value(requestKey{}).(*request); ok {
		return req
	}
	return &request{}
}

// withSession answers 401 to a request without a live session; it reaches next, and so the audit log, only with
// one. The session's CSRF token is checked later, so a change refused for a wrong token is audited.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := s.sessions.Lookup(r)
		if !ok {
			s.deps.Log.Debug("change without a session refused", "method", r.Method, "path", r.URL.Path)
			auth.WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
			return
		}
		requestFrom(r.Context()).user = user
		next.ServeHTTP(w, r)
	})
}

// auditUser sets the user of the request's audit entry.
func auditUser(ctx context.Context, user string) {
	if e := requestFrom(ctx).entry; e != nil {
		e.User = user
	}
}

// recordDetails adds what an issued or revoked credential grants to the request's audit entry.
func recordDetails(ctx context.Context, details map[string]string) {
	if e := requestFrom(ctx).entry; e != nil {
		e.Details = details
	}
}

// recordPaths adds the changed Appliance paths to the request's audit entry.
func recordPaths(ctx context.Context, paths []string) {
	if e := requestFrom(ctx).entry; e != nil {
		e.Paths = paths
	}
}

func badRequestField(field, message string) BadRequestJSONResponse {
	return BadRequestJSONResponse{Code: "invalid", Message: message, Field: &field}
}
