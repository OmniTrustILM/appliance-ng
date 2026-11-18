// Package api serves the Appliance Manager's REST API, which api/openapi.yaml describes.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
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
)

// AuditStore records the changes made through the API.
type AuditStore interface {
	Write(e audit.Entry) error
	Tail(n int) ([]audit.Entry, error)
}

// Deps is what the Server needs. Now defaults to time.Now; everything else must be set.
type Deps struct {
	// Client reads the Appliance and the Secrets in SystemNamespace, possibly from a cache, and writes them.
	Client client.Client
	// Reader reads past the cache: the Secrets the API writes and those in the platform namespace.
	Reader  client.Reader
	Users   auth.UserStore
	Audit   AuditStore
	Release *release.Manifest
	Log     *slog.Logger
	Now     func() time.Time
}

// Server serves the manager's API under /api/v1/.
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
}

// NewServer returns the API server on deps.
func NewServer(deps Deps) *Server {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Server{
		deps:          deps,
		sessions:      auth.NewSessions(deps.Now),
		limiter:       auth.SignInLimiter(),
		checkPassword: auth.CheckPassword,
		hashSlots:     auth.NewHashSlots(maxPasswordChecks),
		dummyHash:     newDummyHash(),
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
	api := HandlerWithOptions(strict, StdHTTPServerOptions{ErrorHandlerFunc: badRequest})
	protected := s.sessions.Middleware(api)
	change := func(action AuditEntryAction) http.Handler {
		return s.withSession(s.audited(action, protected))
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/ca.pem", api)
	mux.Handle("POST /api/v1/session", s.audited(AuditEntryActionSignIn, api))
	mux.Handle("DELETE /api/v1/session", change(AuditEntryActionSignOut))
	mux.Handle("PUT /api/v1/wizard/platform", change(AuditEntryActionWizardPlatform))
	mux.Handle("POST /api/v1/platform/admin-credential", change(AuditEntryActionAdminCredential))
	mux.Handle("/api/", protected)
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
}

type requestKey struct{}

// withRequest keeps API answers out of caches and from being read as another content type, caps the request body
// and puts the request's details in its context.
func withRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		req := &request{peer: r.RemoteAddr}
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			req.peer = host
		}
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			req.token = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestKey{}, req)))
	})
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

// recordPaths adds the changed Appliance paths to the request's audit entry.
func recordPaths(ctx context.Context, paths []string) {
	if e := requestFrom(ctx).entry; e != nil {
		e.Paths = paths
	}
}

func badRequestField(field, message string) BadRequestJSONResponse {
	return BadRequestJSONResponse{Code: "invalid", Message: message, Field: &field}
}
