package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	// SessionCookieName is the cookie that carries the session token.
	SessionCookieName = "appliance_session"
	// IdleTimeout ends a session that has seen no request for this long.
	IdleTimeout = 30 * time.Minute
	// AbsoluteTimeout ends a session this long after sign-in.
	AbsoluteTimeout = 8 * time.Hour
	// BackgroundRefreshHeader set to 1 marks a request the console makes on its own, such as status polling. It does
	// not count as activity, so an open page alone does not keep the session from going idle.
	BackgroundRefreshHeader = "X-Background-Refresh"

	tokenBytes = 32
)

// Sessions keeps the console sessions in memory. It stores only the SHA-256
// of each session token. It is safe for concurrent use.
type Sessions struct {
	now func() time.Time

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]*session
}

type session struct {
	user     string
	csrf     string
	created  time.Time
	lastSeen time.Time
}

type contextKey int

const (
	userKey contextKey = iota
	csrfKey
)

// NewSessions returns an empty session store on the clock now; nil means
// time.Now.
func NewSessions(now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{now: now, sessions: map[[sha256.Size]byte]*session{}}
}

// Create starts a session for user and returns its token, for the session
// cookie, and its CSRF token, for the X-CSRF-Token header.
func (s *Sessions) Create(user string) (token, csrf string) {
	token, csrf = randomToken(), randomToken()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.sessions {
		if sess.expired(now) {
			delete(s.sessions, k)
		}
	}
	s.sessions[sha256.Sum256([]byte(token))] = &session{user: user, csrf: csrf, created: now, lastSeen: now}
	return token, csrf
}

// Delete ends the session of token, if there is one.
func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sha256.Sum256([]byte(token)))
}

// Middleware lets a request through only with a live session cookie and, for
// methods that change state, the session's CSRF token in X-CSRF-Token. It
// answers 401 without a session and 403 without the right CSRF token, and
// puts the user and the CSRF token in the request context.
func (s *Sessions) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, csrf, ok := s.Lookup(r)
		if !ok {
			WriteError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
			return
		}
		if !csrfOK(r, csrf) {
			WriteError(w, http.StatusForbidden, "csrf", "The request lacks a valid CSRF token.")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		ctx = context.WithValue(ctx, csrfKey, csrf)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Active returns the number of live sessions.
func (s *Sessions) Active() int {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if !sess.expired(now) {
			n++
		}
	}
	return n
}

// Lookup finds the live session of the request's cookie, marks it seen unless
// the request is a background refresh and returns its user and CSRF token.
func (s *Sessions) Lookup(r *http.Request) (user, csrf string, ok bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return "", "", false
	}
	key := sha256.Sum256([]byte(cookie.Value))
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, found := s.sessions[key]
	if !found {
		return "", "", false
	}
	if sess.expired(now) {
		delete(s.sessions, key)
		return "", "", false
	}
	if r.Header.Get(BackgroundRefreshHeader) != "1" {
		sess.lastSeen = now
	}
	return sess.user, sess.csrf, true
}

func (sess *session) expired(now time.Time) bool {
	return now.Sub(sess.lastSeen) > IdleTimeout || now.Sub(sess.created) >= AbsoluteTimeout
}

// UserFromContext returns the signed-in user that Middleware put in ctx.
func UserFromContext(ctx context.Context) (string, bool) {
	user, ok := ctx.Value(userKey).(string)
	return user, ok
}

// CSRFFromContext returns the session's CSRF token that Middleware put in ctx.
func CSRFFromContext(ctx context.Context) (string, bool) {
	csrf, ok := ctx.Value(csrfKey).(string)
	return csrf, ok
}

// SessionCookie returns the cookie that carries token.
func SessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

// ClearSessionCookie returns the cookie that removes the session cookie on
// sign-out.
func ClearSessionCookie() *http.Cookie {
	c := SessionCookie("")
	c.MaxAge = -1
	return c
}

// apiError is the JSON body of an error answer.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError answers with status and a JSON error body of the code and message.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Code: code, Message: message})
}

// randomToken returns 32 random bytes, base64url encoded.
func randomToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return base64.RawURLEncoding.EncodeToString(b)
}
