package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

func TestSignInNeedsPasswordAndTOTP(t *testing.T) {
	h := newHarness(t)

	rec := h.request(http.MethodPost, "/appliance/api/v1/session", map[string]string{"user": testUser, "password": testPassword}, nil)
	wantStatus(t, rec, http.StatusBadRequest)
	h.now = h.now.Add(30 * time.Second)
	rec = h.request(http.MethodPost, "/appliance/api/v1/session", map[string]string{"user": testUser, "totp": h.code()}, nil)
	wantStatus(t, rec, http.StatusBadRequest)
	h.now = h.now.Add(30 * time.Second)

	s := h.signIn()
	rec = h.request(http.MethodGet, "/appliance/api/v1/session", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Session
	decode(t, rec, &got)
	if got.User != testUser || got.CsrfToken != s.csrf {
		t.Errorf("session = %+v, want user %s and the sign-in CSRF token", got, testUser)
	}
}

func TestSignInSetsAStrictCookie(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	wantStatus(t, rec, http.StatusOK)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want one", cookies)
	}
	c := cookies[0]
	if c.Name != auth.SessionCookieName || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/appliance/" {
		t.Errorf("cookie = %+v, want %s HttpOnly Secure SameSite=Strict Path=/appliance/", c, auth.SessionCookieName)
	}
}

func TestSignInFailuresLookAlike(t *testing.T) {
	h := newHarness(t)
	cases := map[string]map[string]string{
		"unknown user":   signInBody("nobody", testPassword, h.code()),
		"wrong password": signInBody(testUser, "wrong password value", h.code()),
		"wrong TOTP":     signInBody(testUser, testPassword, "000000"),
		"malformed TOTP": signInBody(testUser, testPassword, "12"),
	}
	var first *SignInError
	for name, body := range cases {
		rec := h.request(http.MethodPost, "/appliance/api/v1/session", body, nil)
		wantStatus(t, rec, http.StatusBadRequest)
		var got SignInError
		decode(t, rec, &got)
		if got.ServerTime.IsZero() {
			t.Errorf("%s: no server time", name)
		}
		got.ServerTime = time.Time{}
		if first == nil {
			first = &got
			continue
		}
		if got.Code != first.Code || got.Message != first.Message || got.Field != nil {
			t.Errorf("%s: answer %+v differs from %+v", name, got, *first)
		}
	}
	if first.Code != "invalid_credentials" {
		t.Errorf("code %q, want invalid_credentials", first.Code)
	}
}

func TestSignInRefusesAReplayedCode(t *testing.T) {
	h := newHarness(t)
	code := h.code()
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, code), nil), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, code), nil), http.StatusBadRequest)
}

func TestSignInHashesForUnknownUsers(t *testing.T) {
	h := newHarness(t)
	var hashes []string
	h.srv.checkPassword = func(hash, pw string) bool {
		hashes = append(hashes, hash)
		return auth.CheckPassword(hash, pw)
	}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody("nobody", testPassword, h.code()), nil), http.StatusBadRequest)
	if len(hashes) != 1 || hashes[0] == "" {
		t.Fatalf("password checks for an unknown user = %q, want one against a dummy hash", hashes)
	}
	if auth.CheckPassword(hashes[0], testPassword) {
		t.Error("the dummy hash accepts a password")
	}
}

func TestSignInRateLimited(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		rec := h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", h.code()), nil)
		wantStatus(t, rec, http.StatusBadRequest)
		_ = i
	}
	rec := h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	wantStatus(t, rec, http.StatusUnauthorized)
	var locked LockedError
	decode(t, rec, &locked)
	if locked.RetryAfter <= 0 {
		t.Errorf("retryAfter = %d, want > 0", locked.RetryAfter)
	}

	h.now = h.now.Add(16 * time.Minute)
	h.signIn()
}

func TestSignInLockedFromOnePeerOnly(t *testing.T) {
	h := newHarness(t)
	for range 5 {
		wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", "000000"), nil), http.StatusBadRequest)
	}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil), http.StatusUnauthorized)
	rec := h.requestFrom("192.0.2.11:40000", http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	wantStatus(t, rec, http.StatusOK)
}

func TestSignInRateLimitedPerPeer(t *testing.T) {
	h := newHarness(t)
	for i := range 20 {
		user := "nobody" + string(rune('a'+i))
		wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(user, testPassword, "000000"), nil), http.StatusBadRequest)
	}
	rec := h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	wantStatus(t, rec, http.StatusUnauthorized)
	rec = h.requestFrom("192.0.2.11:40000", http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	wantStatus(t, rec, http.StatusOK)
}

func TestSuccessfulSignInClearsUserFailures(t *testing.T) {
	h := newHarness(t)
	for range 4 {
		wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", "000000"), nil), http.StatusBadRequest)
	}
	h.signIn()
	for range 4 {
		wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", "000000"), nil), http.StatusBadRequest)
	}
	h.signIn()
}

func TestSignOut(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	rec := h.request(http.MethodDelete, "/appliance/api/v1/session", nil, s)
	wantStatus(t, rec, http.StatusNoContent)
	cleared := false
	for _, c := range rec.Result().Cookies() {
		cleared = cleared || (c.Name == auth.SessionCookieName && c.MaxAge < 0)
	}
	if !cleared {
		t.Error("sign-out does not clear the session cookie")
	}
	wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/session", nil, s), http.StatusUnauthorized)
}

func TestSessionEndsWhenIdle(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	h.now = h.now.Add(auth.IdleTimeout + time.Minute)
	wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/session", nil, s), http.StatusUnauthorized)
}

func TestSignInWithAMalformedBody(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", "{", nil), http.StatusBadRequest)
}

// TestSignInBoundsConcurrentPasswordChecks checks that parallel sign-ins, which each need tens of MiB for the
// password hash, run their checks one at a time.
func TestSignInBoundsConcurrentPasswordChecks(t *testing.T) {
	h := newHarness(t)
	var (
		mu           sync.Mutex
		active, peak int
	)
	h.srv.checkPassword = func(string, string) bool {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return false
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			peer := fmt.Sprintf("192.0.2.%d:40000", 20+i)
			rec := h.requestFrom(peer, http.MethodPost, "/appliance/api/v1/session", signInBody(fmt.Sprintf("nobody%d", i), testPassword, "000000"), nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("sign-in %d: status %d, want 400", i, rec.Code)
			}
		})
	}
	wg.Wait()
	if got := peak; got != maxPasswordChecks {
		t.Errorf("at most %d password checks ran at once, want %d", got, maxPasswordChecks)
	}
}

func TestPasswordCheckFreesItsSlotWhenItPanics(t *testing.T) {
	s := &Server{hashSlots: auth.NewHashSlots(1), checkPassword: func(string, string) bool { panic("check failed") }}
	func() {
		defer func() { _ = recover() }()
		_, _ = s.passwordMatches(context.Background(), "hash", "password")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.hashSlots.Acquire(ctx); err != nil {
		t.Fatal("the hash slot was not freed")
	}
}
