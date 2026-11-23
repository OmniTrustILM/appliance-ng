package setup

import (
	"context"
	"crypto/tls"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/api/setupapi"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const (
	// maxBody caps the size of a request body before it is decoded.
	maxBody = 8 << 10
	// maxPasswordHashes bounds the password hashes that run at once; each takes 64 MiB.
	maxPasswordHashes = 1
)

// ServerDeps is what the setup server needs. Network, Apply and Log must not be nil; Now defaults to time.Now.
type ServerDeps struct {
	Addr string
	Cert tls.Certificate
	// Console holds the console build the server serves outside the API.
	Console fs.FS
	// Network reports the network setup DHCP gave the node, to prefill the node settings.
	Network func(ctx context.Context) (talos.Network, error)
	// Apply receives the machine configuration input once the administrator applies the answers; it must not block.
	Apply func(machineconfig.Input)
	Log   *slog.Logger
	Now   func() time.Time
}

// wizard is the state of wizard part 1: one setup session and the answers given in it, in memory until apply.
type wizard struct {
	code     *Code
	deps     ServerDeps
	sessions *auth.Sessions
	guard    auth.TOTPGuard
	// hashPassword is auth.HashPassword; tests replace it.
	hashPassword func(pw string) (string, error)
	hashSlots    auth.HashSlots

	mu           sync.Mutex
	step         setupapi.SetupStep
	mode         string
	user         string
	passwordHash string
	seed         string
	node         NodeAnswers
	input        machineconfig.Input
}

// NewServer returns the HTTPS server of wizard part 1 for code: the setup API under auth.APIBase/setup/ and the
// console under auth.BasePath.
func NewServer(code *Code, deps ServerDeps) *http.Server {
	w := newWizard(code, deps)
	return &http.Server{
		Addr:              w.deps.Addr,
		Handler:           w.handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{w.deps.Cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(w.deps.Log.Handler(), slog.LevelDebug),
	}
}

func newWizard(code *Code, deps ServerDeps) *wizard {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &wizard{
		code: code, deps: deps, sessions: auth.NewSessions(deps.Now), step: setupapi.Code,
		hashPassword: auth.HashPassword, hashSlots: auth.NewHashSlots(maxPasswordHashes),
	}
}

func (w *wizard) handler() http.Handler {
	badRequest := func(rw http.ResponseWriter, _ *http.Request, _ error) {
		auth.WriteError(rw, http.StatusBadRequest, "invalid_request", "The request is not valid.")
	}
	strict := setupapi.NewStrictHandlerWithOptions(w, nil, setupapi.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(rw http.ResponseWriter, _ *http.Request, err error) {
			w.deps.Log.Error("setup response failed", "error", err)
			auth.WriteError(rw, http.StatusInternalServerError, "internal", "The request failed.")
		},
	})
	api := setupapi.HandlerWithOptions(strict, setupapi.StdHTTPServerOptions{BaseURL: auth.APIBase, ErrorHandlerFunc: badRequest})

	mux := http.NewServeMux()
	mux.Handle(auth.APIBase+"/setup/code", apiHeaders(w.withSession(api)))
	mux.Handle(auth.APIBase+"/setup/state", apiHeaders(w.withSession(api)))
	mux.Handle(auth.APIBase+"/setup/", apiHeaders(w.sessions.Middleware(api)))
	console.Mount(mux, w.deps.Console)
	return mux
}

// apiHeaders keeps API answers out of caches and from being read as another content type, and caps the request body.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		r.Body = http.MaxBytesReader(rw, r.Body, maxBody)
		next.ServeHTTP(rw, r)
	})
}

type csrfKey struct{}

// withSession puts the CSRF token of the request's live session, if it has one, in the request context.
func (w *wizard) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if _, csrf, ok := w.sessions.Lookup(r); ok {
			r = r.WithContext(context.WithValue(r.Context(), csrfKey{}, csrf))
		}
		next.ServeHTTP(rw, r)
	})
}

// sessionCSRF returns the CSRF token of the request's session, if it has one.
func sessionCSRF(ctx context.Context) (string, bool) {
	if csrf, ok := ctx.Value(csrfKey{}).(string); ok {
		return csrf, true
	}
	return auth.CSRFFromContext(ctx)
}
