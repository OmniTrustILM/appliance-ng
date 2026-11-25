package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const (
	pathAdminPassword  = "/appliance/api/v1/platform/admin-password"
	firstAdminPassword = "first-Admin-password-1"
	keycloakAdminPW    = "keycloak-admin-pw"
	keycloakToken      = "keycloak-token"
	keycloakDetail     = "keycloak says the password is weak"
)

// fakeKeycloak is the admin REST API of the managed Keycloak with the ILM administrator; failWith, when set, is the
// status of the password reset.
type fakeKeycloak struct {
	url string

	mu       sync.Mutex
	calls    []string
	password string
	failWith int
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	k := &fakeKeycloak{}
	srv := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(srv.Close)
	k.url = srv.URL + "/kc"
	return k
}

func (k *fakeKeycloak) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/kc/realms/master/protocol/openid-connect/token":
		if r.FormValue("grant_type") != "password" || r.FormValue("client_id") != "admin-cli" ||
			r.FormValue("username") != "kc-admin" || r.FormValue("password") != keycloakAdminPW {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": keycloakToken})
	case r.Header.Get("Authorization") != "Bearer "+keycloakToken:
		w.WriteHeader(http.StatusUnauthorized)
	case r.Method == http.MethodGet && r.URL.Path == "/kc/admin/realms/ilm/users" &&
		r.URL.Query().Get("username") == "admin" && r.URL.Query().Get("exact") == "true":
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "u-1", "username": "admin"}})
	case r.Method == http.MethodPut && r.URL.Path == "/kc/admin/realms/ilm/users/u-1/reset-password":
		if k.failWith != 0 {
			w.WriteHeader(k.failWith)
			_, _ = w.Write([]byte(keycloakDetail))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["type"] != "password" || body["temporary"] != false {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		k.password, _ = body["value"].(string)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (k *fakeKeycloak) state() ([]string, string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.calls), k.password
}

func createAdminPasswordSecret(t *testing.T) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret},
		Data:       map[string][]byte{"password": []byte(firstAdminPassword)},
	}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func adminPasswordSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: platform.Namespace, Name: platform.AdminPasswordSecret}, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// readyForReset makes the Appliance with the managed Keycloak and a ready platform, the Keycloak administrator's
// Secret and the administrator password's Secret, and points the server at a fake Keycloak.
func readyForReset(t *testing.T, h *harness) *fakeKeycloak {
	t.Helper()
	createAppliance(t, v1alpha1.SetupComplete)
	setPlatformReady(t)
	createKeycloakAdminSecret(t)
	createAdminPasswordSecret(t)
	k := newFakeKeycloak(t)
	h.srv.deps.KeycloakURL = k.url
	return k
}

func setPlatformReady(t *testing.T) {
	t.Helper()
	a := getAppliance(t)
	meta.SetStatusCondition(&a.Status.Conditions, metav1.Condition{Type: v1alpha1.ConditionPlatformReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready"})
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func createKeycloakAdminSecret(t *testing.T) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.KeycloakAdminSecret},
		Data:       map[string][]byte{"username": []byte("kc-admin"), "password": []byte(keycloakAdminPW)},
	}
	if err := testClient.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// wantNoSecrets checks that the audit log and the manager log carry none of the passwords.
func wantNoSecrets(t *testing.T, h *harness, secrets ...string) {
	t.Helper()
	entries, err := json.Marshal(h.entries())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(secrets, firstAdminPassword, keycloakAdminPW, keycloakToken) {
		if strings.Contains(string(entries), secret) || strings.Contains(h.logs.String(), secret) {
			t.Errorf("the audit log or the manager log carries %q", secret)
		}
	}
}

func auditResults(h *harness, action AuditEntryAction) []string {
	var results []string
	for _, e := range h.entries() {
		if e.Action == string(action) {
			results = append(results, e.Result)
		}
	}
	return results
}

func TestAdminPasswordShownOnce(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	wantStatus(t, h.request(http.MethodPost, pathAdminPassword, nil, s), http.StatusNotFound)
	createAdminPasswordSecret(t)

	rec := h.request(http.MethodPost, pathAdminPassword, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got AdminPassword
	decode(t, rec, &got)
	if got.User != "admin" || got.Password != firstAdminPassword {
		t.Errorf("answer %+v, want admin and the Secret's password", got)
	}
	if a := adminPasswordSecret(t).Annotations[AdminPasswordShownAnnotation]; a != sha256Hex(firstAdminPassword) {
		t.Errorf("annotation %q, want the password's SHA-256", a)
	}
	wantStatus(t, h.request(http.MethodPost, pathAdminPassword, nil, s), http.StatusGone)

	if got := auditResults(h, AuditEntryActionAdminPasswordShown); !slices.Equal(got, []string{"failure", "success", "failure"}) {
		t.Errorf("audit results %v, want failure (not there yet), success, failure", got)
	}
	wantNoSecrets(t, h)
}

func TestAdminPasswordFailsClosedWithoutAudit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	createAdminPasswordSecret(t)
	h.srv.deps.Audit = failingAudit{}
	rec := h.request(http.MethodPost, pathAdminPassword, nil, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), firstAdminPassword) {
		t.Error("the password was sent without an audit entry")
	}
	if _, ok := adminPasswordSecret(t).Annotations[AdminPasswordShownAnnotation]; ok {
		t.Error("the password is used up although it was not shown")
	}
	h.srv.deps.Audit = h.audit
	wantStatus(t, h.request(http.MethodPost, pathAdminPassword, nil, s), http.StatusOK)
}

func TestAdminPasswordWithAnExternalIdentity(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Spec.Platform = &v1alpha1.PlatformSpec{HostName: "ilm.example.com", Identity: &v1alpha1.ExternalIdentity{
		Issuer: "https://idp.example.com", ClientID: "ilm", CredentialsRef: "appliance-identity",
	}}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	setPlatformReady(t)
	createAdminPasswordSecret(t)
	createKeycloakAdminSecret(t)
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		rec := h.request(method, pathAdminPassword, nil, s)
		wantStatus(t, rec, http.StatusConflict)
		var e Error
		decode(t, rec, &e)
		if e.Code != "external_identity" {
			t.Errorf("%s code %q, want external_identity", method, e.Code)
		}
	}
}

func TestSetAdminPassword(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	k := readyForReset(t, h)

	rec := h.request(http.MethodPut, pathAdminPassword, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got AdminPassword
	decode(t, rec, &got)
	calls, set := k.state()
	want := []string{
		"POST /kc/realms/master/protocol/openid-connect/token",
		"GET /kc/admin/realms/ilm/users",
		"PUT /kc/admin/realms/ilm/users/u-1/reset-password",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("Keycloak calls %q, want %q", calls, want)
	}
	if got.User != "admin" || got.Password == "" || got.Password == firstAdminPassword || got.Password != set {
		t.Errorf("answer %+v, want admin and the password Keycloak got", got)
	}
	secret := adminPasswordSecret(t)
	if string(secret.Data["password"]) != got.Password || secret.Annotations[AdminPasswordShownAnnotation] != sha256Hex(got.Password) {
		t.Errorf("the Secret does not hold the new password marked as shown")
	}
	wantStatus(t, h.request(http.MethodPost, pathAdminPassword, nil, s), http.StatusGone)
	if got := auditResults(h, AuditEntryActionAdminPasswordReset); !slices.Equal(got, []string{"success"}) {
		t.Errorf("audit results %v, want success", got)
	}
	wantNoSecrets(t, h, got.Password)
}

func TestSetAdminPasswordFailsClosedWithoutAudit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	k := readyForReset(t, h)
	h.srv.deps.Audit = failingAudit{}
	before := adminPasswordSecret(t)
	rec := h.request(http.MethodPut, pathAdminPassword, nil, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if calls, set := k.state(); len(calls) != 0 || set != "" {
		t.Errorf("Keycloak was called without an audit entry: %q", calls)
	}
	after := adminPasswordSecret(t)
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("the Secret changed without an audit entry")
	}
}

// busyRunner is a fakeRunner whose backup runs throughout.
type busyRunner struct{ *fakeRunner }

func (busyRunner) Exclusive(func() error) error { return backup.ErrBusy }

func TestSetAdminPasswordWhileABackupRuns(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	k := readyForReset(t, h)
	runner, _ := newFakeRunner(t)
	h.srv.deps.Backups = busyRunner{runner}
	before := adminPasswordSecret(t)
	wantCode(t, h.request(http.MethodPut, pathAdminPassword, nil, s), http.StatusConflict, "backup_running")
	if calls, _ := k.state(); len(calls) != 0 {
		t.Errorf("Keycloak was called while a backup runs: %q", calls)
	}
	if after := adminPasswordSecret(t); after.ResourceVersion != before.ResourceVersion {
		t.Error("the Secret changed while a backup runs")
	}
}

func TestSetAdminPasswordKeycloakFails(t *testing.T) {
	for name, fail := range map[string]func(*fakeKeycloak, *harness){
		"refused":      func(k *fakeKeycloak, _ *harness) { k.failWith = http.StatusBadRequest },
		"server error": func(k *fakeKeycloak, _ *harness) { k.failWith = http.StatusInternalServerError },
		"unreachable":  func(_ *fakeKeycloak, h *harness) { h.srv.deps.KeycloakURL = "http://127.0.0.1:1/kc" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			s := h.signIn()
			k := readyForReset(t, h)
			fail(k, h)
			before := adminPasswordSecret(t)
			rec := h.request(http.MethodPut, pathAdminPassword, nil, s)
			wantStatus(t, rec, http.StatusBadGateway)
			var e Error
			decode(t, rec, &e)
			if e.Code != "keycloak_failed" || strings.Contains(rec.Body.String(), keycloakDetail) {
				t.Errorf("answer %s, want the fixed keycloak_failed", rec.Body)
			}
			after := adminPasswordSecret(t)
			if after.ResourceVersion != before.ResourceVersion || string(after.Data["password"]) != firstAdminPassword {
				t.Error("the Secret changed although Keycloak did not take the password")
			}
			if got := auditResults(h, AuditEntryActionAdminPasswordReset); !slices.Equal(got, []string{"success", "failure"}) {
				t.Errorf("audit results %v, want success (the reset was tried), failure", got)
			}
			if strings.Contains(h.logs.String(), keycloakDetail) {
				t.Error("the log quotes Keycloak")
			}
			wantNoSecrets(t, h)
		})
	}
}

func TestSetAdminPasswordAnswersWithinTheWriteTimeout(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	readyForReset(t, h)
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	h.srv.deps.KeycloakURL = slow.URL + "/kc"
	defer func(d time.Duration) { keycloakResetTimeout = d }(keycloakResetTimeout)
	keycloakResetTimeout = 100 * time.Millisecond

	started := time.Now()
	wantCode(t, h.request(http.MethodPut, pathAdminPassword, nil, s), http.StatusBadGateway, "keycloak_failed")
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the reset took %v, want it bounded by its timeout", took)
	}
}

func TestSetAdminPasswordNotReady(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	createAdminPasswordSecret(t)
	k := newFakeKeycloak(t)
	h.srv.deps.KeycloakURL = k.url
	notReady := func() {
		t.Helper()
		rec := h.request(http.MethodPut, pathAdminPassword, nil, s)
		wantStatus(t, rec, http.StatusConflict)
		var e Error
		decode(t, rec, &e)
		if e.Code != "not_ready" {
			t.Errorf("code %q, want not_ready", e.Code)
		}
	}
	notReady()
	setPlatformReady(t)
	notReady()
	if calls, _ := k.state(); len(calls) != 0 {
		t.Errorf("Keycloak calls %q, want none", calls)
	}
}
