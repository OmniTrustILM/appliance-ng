package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	adminPassword = "admin-pw"
	fakeToken     = "admin-token"
)

// newServer serves the token endpoint of the master realm for admin/adminPassword under /kc and hands every other
// request to next once its token checks out.
func newServer(t *testing.T, next http.HandlerFunc) Admin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/kc/realms/master/protocol/openid-connect/token":
			if r.FormValue("grant_type") != "password" || r.FormValue("client_id") != "admin-cli" ||
				r.FormValue("username") != "admin" || r.FormValue("password") != adminPassword {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
		case r.Header.Get("Authorization") != "Bearer "+fakeToken:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			next(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return Admin{Base: srv.URL + "/kc", User: "admin", Password: adminPassword, Client: HTTPClient()}
}

func TestURL(t *testing.T) {
	ctx := context.Background()
	if got, err := URL(ctx, fake.NewClientBuilder().Build(), "http://keycloak.test/kc"); err != nil || got != "http://keycloak.test/kc" {
		t.Errorf("URL with an override = %q, %v; want the override", got, err)
	}
	if _, err := URL(ctx, fake.NewClientBuilder().Build(), ""); err == nil {
		t.Error("URL without the Service passed")
	}
	svc := func(ip string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: "ilm-keycloak-service"}, Spec: corev1.ServiceSpec{ClusterIP: ip}}
	}
	if _, err := URL(ctx, fake.NewClientBuilder().WithObjects(svc(corev1.ClusterIPNone)).Build(), ""); err == nil {
		t.Error("URL of a headless Service passed")
	}
	got, err := URL(ctx, fake.NewClientBuilder().WithObjects(svc("10.96.0.20")).Build(), "")
	if err != nil || got != "http://10.96.0.20:8080/kc" {
		t.Errorf("URL = %q, %v; want the Service's cluster IP on port 8080 under /kc", got, err)
	}
}

func TestHTTPClientUsesNoProxy(t *testing.T) {
	if HTTPClient().Transport.(*http.Transport).Proxy != nil {
		t.Error("want an HTTP client that uses no proxy")
	}
}

func TestCallDecodesTheAnswer(t *testing.T) {
	a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"n": 1}`))
	})
	token, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := a.Call(context.Background(), http.MethodPut, "/admin/x", token, map[string]string{"a": "b"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["n"] != json.Number("1") {
		t.Errorf("answer %v", out)
	}
}

func TestErrorsNameTheRequestOnly(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		status      int
		body        string
		unavailable bool
	}{
		"server error": {status: http.StatusServiceUnavailable, body: "secret detail", unavailable: true},
		"refused":      {status: http.StatusForbidden, body: "secret detail"},
		"not JSON":     {status: http.StatusOK, body: "secret detail"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			var out any
			err := a.Call(ctx, http.MethodGet, "/admin/x", fakeToken, nil, &out)
			if err == nil || errors.Is(err, ErrUnavailable) != tc.unavailable || strings.Contains(err.Error(), "secret detail") {
				t.Errorf("error %v", err)
			}
		})
	}
	a := newServer(t, nil)
	a.Password = "wrong"
	if _, err := a.Token(ctx); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("sign-in with a wrong password: %v", err)
	}
	a.Base = "http://127.0.0.1:1/kc"
	if _, err := a.Token(ctx); !errors.Is(err, ErrUnavailable) {
		t.Errorf("sign-in at a closed port: %v, want unavailable", err)
	}
}

func TestTokenNeedsAToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	if _, err := (Admin{Base: srv.URL, Client: HTTPClient()}).Token(context.Background()); err == nil {
		t.Error("a sign-in without a token passed")
	}
}

// TestSetPassword checks the calls: the sign-in, the user looked up by its exact name, then its password reset as
// a permanent one.
func TestSetPassword(t *testing.T) {
	var calls []string
	var reset map[string]any
	a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/kc/admin/realms/ilm/users":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "u-1", "username": "admin"}})
		case r.Method == http.MethodPut && r.URL.Path == "/kc/admin/realms/ilm/users/u-1/reset-password":
			_ = json.NewDecoder(r.Body).Decode(&reset)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := a.SetPassword(context.Background(), "ilm", "admin", "new-pw"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /kc/admin/realms/ilm/users?exact=true&username=admin", "PUT /kc/admin/realms/ilm/users/u-1/reset-password?"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls %q, want %q", calls, want)
	}
	if reset["type"] != "password" || reset["value"] != "new-pw" || reset["temporary"] != false || len(reset) != 3 {
		t.Errorf("reset body %v", reset)
	}
}

func TestSetPasswordFails(t *testing.T) {
	ctx := context.Background()
	for name, users := range map[string]string{
		"no user":      `[]`,
		"another user": `[{"id": "u-2", "username": "administrator"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(users))
			})
			if err := a.SetPassword(ctx, "ilm", "admin", "new-pw"); err == nil {
				t.Error("passed without the user")
			}
		})
	}
	a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"id": "u-1", "username": "admin"}]`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("new-pw is too weak"))
	})
	if err := a.SetPassword(ctx, "ilm", "admin", "new-pw"); err == nil || strings.Contains(err.Error(), "new-pw") {
		t.Errorf("refused reset: %v", err)
	}
	a.Password = "wrong"
	if err := a.SetPassword(ctx, "ilm", "admin", "new-pw"); err == nil {
		t.Error("passed with a refused sign-in")
	}
}
