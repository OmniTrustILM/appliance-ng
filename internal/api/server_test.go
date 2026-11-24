package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

var (
	testClient client.Client
	testConfig *rest.Config
)

const (
	testUser     = "admin"
	testPassword = "correct horse battery staple"
	testPeer     = "192.0.2.10:40000"
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		println("KUBEBUILDER_ASSETS is not set; run the tests with make test")
		return 1
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	testConfig = cfg
	if err != nil {
		println("start envtest:", err.Error())
		return 1
	}
	defer func() { _ = env.Stop() }()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			println("scheme:", err.Error())
			return 1
		}
	}
	if testClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		println("client:", err.Error())
		return 1
	}
	for _, ns := range []string{SystemNamespace, platform.Namespace, access.Namespace, certs.ClusterResourceNamespace} {
		if err := testClient.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			println("namespace:", err.Error())
			return 1
		}
	}
	return m.Run()
}

// harness is a Server on the envtest cluster with a controllable clock, a captured log and an audit log in a
// temporary directory.
type harness struct {
	t       *testing.T
	srv     *Server
	handler http.Handler
	logs    *bytes.Buffer
	audit   *audit.Log
	auditFS string
	now     time.Time
	seed    string
}

var testRelease = &release.Manifest{
	Platform: release.PlatformDefaults{ILMVersion: "2.20.0", HostName: "ilm.appliance.internal"},
	Connectors: []release.Connector{
		{Name: "common-credential-provider", DisplayName: "Common Credential Provider"},
		{Name: "ejbca-ng-connector", DisplayName: "EJBCA NG Connector"},
		{Name: "external-authority-provider", DisplayName: "External Authority Provider", Private: true,
			Image: testRegistryHost + "/ilm-private/external-authority-provider:1.0.0"},
		{Name: testMailComponent, DisplayName: "Email Notification Provider", Settings: []string{release.SettingMail}},
	},
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessAs(t, testClient)
}

// newHarnessAs returns a harness whose server acts with c.
func newHarnessAs(t *testing.T, c client.Client) *harness {
	t.Helper()
	return newHarnessWith(t, c, testConfig)
}

// newHarnessWith returns a harness whose server acts with c and with a clientset on cfg.
func newHarnessWith(t *testing.T, c client.Client, cfg *rest.Config) *harness {
	t.Helper()
	resetCluster(t)
	dir := t.TempDir()
	log, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	h := &harness{t: t, logs: &bytes.Buffer{}, audit: log, auditFS: dir, now: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	users := auth.NewSecretUserStore(c, SystemNamespace)
	h.seed, _, err = auth.NewTOTP(testUser, "test")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := users.Create(context.Background(), auth.User{Name: testUser, PasswordHash: hash, TOTPSeed: h.seed}); err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = NewServer(Deps{
		Client:    c,
		Reader:    c,
		Kube:      kube,
		ClusterCA: testConfig.CAData,
		Users:     users,
		Audit:     log,
		Release:   testRelease,
		Log:       slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:       func() time.Time { return h.now },
	})
	h.handler = h.srv.Handler()
	return h
}

// resetCluster removes the Appliance and the Secrets earlier tests left behind.
func resetCluster(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	deleteAppliance(t)
	for _, ns := range []string{SystemNamespace, platform.Namespace} {
		if err := testClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
	}
}

// deleteAppliance removes the Appliance and keeps the accounts, so that signed-in sessions stay valid.
func deleteAppliance(t *testing.T) {
	t.Helper()
	if err := testClient.Delete(context.Background(), &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName}}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

// createAppliance creates the Appliance with setup at the given phase.
func createAppliance(t *testing.T, setup string) *v1alpha1.Appliance {
	t.Helper()
	ctx := context.Background()
	a := &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec:       v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "appliance.example.com"}},
	}
	if err := testClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if setup != "" {
		a.Status.Setup = setup
		if err := testClient.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func getAppliance(t *testing.T) *v1alpha1.Appliance {
	t.Helper()
	a := &v1alpha1.Appliance{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		t.Fatal(err)
	}
	return a
}

// session is a signed-in browser.
type session struct {
	cookie, csrf string
}

// request sends a request to the handler from testPeer.
func (h *harness) request(method, path string, body any, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.requestFrom(testPeer, method, path, body, s)
}

func (h *harness) requestFrom(peer, method, path string, body any, s *session) *httptest.ResponseRecorder {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		r = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, r)
	req.RemoteAddr = peer
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s != nil {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.cookie})
		req.Header.Set(auth.CSRFHeader, s.csrf)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// code returns the TOTP code of the test user at the harness clock.
func (h *harness) code() string {
	h.t.Helper()
	c, err := totp.GenerateCodeCustom(h.seed, h.now, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func signInBody(user, password, code string) map[string]string {
	return map[string]string{"user": user, "password": password, "totp": code}
}

// signIn signs the test user in and returns the session; the clock moves one TOTP step on, so the next sign-in
// gets a fresh code.
func (h *harness) signIn() *session {
	h.t.Helper()
	rec := h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil)
	h.now = h.now.Add(30 * time.Second)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("sign-in status %d: %s", rec.Code, rec.Body)
	}
	var body CsrfToken
	decode(h.t, rec, &body)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return &session{cookie: c.Value, csrf: body.CsrfToken}
		}
	}
	h.t.Fatal("sign-in set no session cookie")
	return nil
}

// entries returns the audit log, oldest first.
func (h *harness) entries() []audit.Entry {
	h.t.Helper()
	got, err := h.audit.Tail(1000)
	if err != nil {
		h.t.Fatal(err)
	}
	for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
		got[i], got[j] = got[j], got[i]
	}
	return got
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
}

func TestAPIAnswersAreNotCached(t *testing.T) {
	h := newHarness(t)
	rec := h.request(http.MethodGet, "/appliance/api/v1/session", nil, nil)
	wantStatus(t, rec, http.StatusUnauthorized)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options %q, want nosniff", got)
	}
}

func TestRequestBodyIsCapped(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	big := `{"hostName":"ilm.example.com","components":[],"x":"` + strings.Repeat("a", maxBody) + `"}`
	rec := h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", big, s)
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestProtectedOperationsNeedASession(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/appliance/api/v1/session"},
		{http.MethodDelete, "/appliance/api/v1/session"},
		{http.MethodGet, "/appliance/api/v1/status"},
		{http.MethodGet, "/appliance/api/v1/appliance"},
		{http.MethodPut, "/appliance/api/v1/wizard/platform"},
		{http.MethodGet, "/appliance/api/v1/catalogue"},
		{http.MethodGet, "/appliance/api/v1/audit"},
		{http.MethodPost, "/appliance/api/v1/platform/admin-credential"},
		{http.MethodGet, pathTrust},
		{http.MethodPost, pathTrust},
		{http.MethodPost, pathTrustPreview},
		{http.MethodGet, pathRegistries},
		{http.MethodPut, pathTestRegistry},
		{http.MethodGet, pathPlatform},
		{http.MethodPut, pathPlatform},
		{http.MethodGet, pathMail},
		{http.MethodPut, pathMail},
		{http.MethodDelete, pathMail},
		{http.MethodPost, pathMailTest},
		{http.MethodGet, pathAccess},
		{http.MethodPost, pathKubeconfig},
		{http.MethodDelete, pathKubeconfig + "/kc-any"},
		{http.MethodPost, pathTalosconfig},
		{http.MethodGet, pathRestore},
		{http.MethodPut, pathRestoreUpload},
		{http.MethodPost, pathRestoreList},
		{http.MethodPost, pathRestoreFetch},
		{http.MethodPost, pathRestoreInspect},
		{http.MethodPost, pathRestoreStart},
		{http.MethodPost, pathRestoreResume},
	} {
		if rec := h.request(tc.method, tc.path, nil, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: status %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestChangesNeedTheCSRFToken(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	wrong := &session{cookie: s.cookie, csrf: "wrong"}
	rec := h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}}, wrong)
	wantStatus(t, rec, http.StatusForbidden)
	if got := getAppliance(t).Spec.Platform; got != nil {
		t.Errorf("platform written without the CSRF token: %+v", got)
	}
}
