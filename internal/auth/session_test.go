package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// whoami answers with the signed-in user and the session's CSRF token.
var whoami = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	csrf, _ := CSRFFromContext(r.Context())
	if !ok {
		http.Error(w, "no user", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"user": user, "csrf": csrf})
})

func call(t *testing.T, h http.Handler, method, token, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/whoami", nil)
	if token != "" {
		req.AddCookie(SessionCookie(token))
	}
	if csrf != "" {
		req.Header.Set(CSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSessionCreateTokens(t *testing.T) {
	s := NewSessions(nil)
	token, csrf := s.Create("admin")
	for name, v := range map[string]string{"token": token, "csrf": csrf} {
		raw, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(raw) != 32 {
			t.Fatalf("%s %q is not 32 bytes of base64url (err %v)", name, v, err)
		}
	}
	if token == csrf {
		t.Fatal("token and CSRF token are equal")
	}
	if other, _ := s.Create("admin"); other == token {
		t.Fatal("two sessions share a token")
	}
	rec := call(t, s.Middleware(whoami), http.MethodGet, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with a fresh session = %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["user"] != "admin" || body["csrf"] != csrf {
		t.Fatalf("context holds %v", body)
	}
}

func TestSessionIdleAndAbsoluteTimeout(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := NewSessions(clock.now)
	h := s.Middleware(whoami)

	t.Run("idle", func(t *testing.T) {
		token, _ := s.Create("admin")
		clock.advance(29 * time.Minute)
		if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusOK {
			t.Fatalf("after 29 min idle = %d, want 200", rec.Code)
		}
		clock.advance(29 * time.Minute)
		if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusOK {
			t.Fatalf("29 min after the last request = %d, want 200 (activity extends the session)", rec.Code)
		}
		clock.advance(30*time.Minute + time.Second)
		if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("after 30 min idle = %d, want 401", rec.Code)
		}
		clock.advance(-30 * time.Minute)
		if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("an expired session came back: %d", rec.Code)
		}
	})

	t.Run("absolute", func(t *testing.T) {
		token, _ := s.Create("admin")
		for elapsed := time.Duration(0); elapsed < 8*time.Hour-20*time.Minute; elapsed += 20 * time.Minute {
			clock.advance(20 * time.Minute)
			if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusOK {
				t.Fatalf("active session after %s = %d, want 200", elapsed+20*time.Minute, rec.Code)
			}
		}
		clock.advance(20 * time.Minute)
		if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("active session after 8 h = %d, want 401", rec.Code)
		}
	})
}

func TestSessionUnauthorized(t *testing.T) {
	s := NewSessions(nil)
	h := s.Middleware(whoami)
	token, _ := s.Create("admin")
	for name, tok := range map[string]string{"no cookie": "", "unknown token": "bm90LWEtc2Vzc2lvbg"} {
		rec := call(t, h, http.MethodGet, tok, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s = %d, want 401", name, rec.Code)
		}
		var body apiError
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body.Code == "" || body.Message == "" {
			t.Fatalf("%s: body %+v, err %v; want JSON {code, message}", name, body, err)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: Content-Type %q", name, ct)
		}
	}
	s.Delete(token)
	if rec := call(t, h, http.MethodGet, token, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after Delete = %d, want 401", rec.Code)
	}
	s.Delete("never-existed")
}

func TestCSRFRequired(t *testing.T) {
	s := NewSessions(nil)
	h := s.Middleware(whoami)
	token, csrf := s.Create("admin")
	otherToken, otherCSRF := s.Create("admin")

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if rec := call(t, h, m, token, ""); rec.Code != http.StatusOK {
			t.Errorf("%s without CSRF = %d, want 200", m, rec.Code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if rec := call(t, h, m, token, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF = %d, want 403", m, rec.Code)
		}
		if rec := call(t, h, m, token, "wrong"); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a wrong CSRF = %d, want 403", m, rec.Code)
		}
		if rec := call(t, h, m, token, otherCSRF); rec.Code != http.StatusForbidden {
			t.Errorf("%s with another session's CSRF = %d, want 403", m, rec.Code)
		}
		if rec := call(t, h, m, token, csrf); rec.Code != http.StatusOK {
			t.Errorf("%s with the right CSRF = %d, want 200", m, rec.Code)
		}
	}
	if rec := call(t, h, http.MethodPost, otherToken, otherCSRF); rec.Code != http.StatusOK {
		t.Errorf("POST of the other session = %d, want 200", rec.Code)
	}
	if rec := call(t, h, http.MethodPost, "", csrf); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST with CSRF but no session = %d, want 401", rec.Code)
	}
}

func TestSessionCookies(t *testing.T) {
	c := SessionCookie("tok")
	if c.Name != "appliance_session" || c.Value != "tok" || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("SessionCookie = %+v", c)
	}
	cl := ClearSessionCookie()
	if cl.Name != "appliance_session" || cl.Value != "" || cl.MaxAge >= 0 || !cl.HttpOnly || !cl.Secure ||
		cl.SameSite != http.SameSiteStrictMode || cl.Path != "/" {
		t.Fatalf("ClearSessionCookie = %+v", cl)
	}
}

func TestSessionsPruneExpired(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := NewSessions(clock.now)
	s.Create("admin")
	s.Create("admin")
	clock.advance(31 * time.Minute)
	s.Create("admin")
	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d sessions kept, want 1: expired sessions are pruned on Create", n)
	}
}

func TestContextWithoutSession(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := UserFromContext(req.Context()); ok {
		t.Fatal("UserFromContext found a user in an empty context")
	}
	if _, ok := CSRFFromContext(req.Context()); ok {
		t.Fatal("CSRFFromContext found a token in an empty context")
	}
}

func TestSessionLookupAndActive(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	s := NewSessions(clock.now)
	if s.Active() != 0 {
		t.Fatalf("Active() = %d on an empty store", s.Active())
	}
	token, csrf := s.Create("setup")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, _, ok := s.Lookup(r); ok {
		t.Error("Lookup found a session without a cookie")
	}
	r.AddCookie(SessionCookie(token))
	user, gotCSRF, ok := s.Lookup(r)
	if !ok || user != "setup" || gotCSRF != csrf {
		t.Errorf("Lookup = %q, %q, %v; want the session", user, gotCSRF, ok)
	}
	if s.Active() != 1 {
		t.Errorf("Active() = %d, want 1", s.Active())
	}

	clock.advance(IdleTimeout + time.Second)
	if s.Active() != 0 {
		t.Errorf("Active() = %d after the idle timeout, want 0", s.Active())
	}
}
