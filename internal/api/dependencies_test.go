package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/backup"
	"github.com/OmniTrustILM/appliance-ng/internal/restore"
)

const (
	pathDependencyTest = "/appliance/api/v1/platform/dependencies/test"
	databasePassword   = "db-Pa55word-that-must-stay-inside"
	databaseNext       = "db-Pa55word-the-next-one-inside"
	messagingPassword  = "mq-Pa55word-that-must-stay-inside"
	identitySecret     = "idp-client-secret-that-must-stay-inside"
)

func externalDatabaseBody(host string, password *string) *DatabaseDependency {
	return &DatabaseDependency{Mode: ModeExternal, Host: ptr(host), Name: ptr("ilm"), Username: ptr("ilm"), Password: password}
}

func externalDependencies() *DependenciesUpdate {
	return &DependenciesUpdate{
		Database: externalDatabaseBody("db.example.com", ptr(databasePassword)),
		Messaging: &MessagingDependency{Mode: ModeExternal, Host: ptr("mq.example.com"), VirtualHost: ptr("ilm"), Username: ptr("ilm"),
			Password: ptr(messagingPassword)},
		Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("https://idp.example.com/realms/ilm"), ClientId: ptr("ilm"),
			Scopes: &[]string{"openid", "profile", "openid"}, Audiences: &[]string{" ilm ", "ilm"}, ClientSecret: ptr(identitySecret)},
	}
}

func platformUpdate(deps *DependenciesUpdate) PlatformUpdate {
	return PlatformUpdate{HostName: "ilm.example.com", Components: []string{}, Dependencies: deps}
}

func secretExists(t *testing.T, name string) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: name}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestWizardChoosesDependencies(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)

	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Dependencies: externalDependencies(), Utils: ptr(true)}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)

	p := getAppliance(t).Spec.Platform
	wantDB := v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: DatabaseSecretName}
	if p.Database == nil || *p.Database != wantDB || p.Messaging == nil || p.Messaging.Port != 5672 || p.Messaging.VirtualHost != "ilm" ||
		p.Identity == nil || !slices.Equal(p.Identity.Scopes, []string{"openid", "profile"}) || !slices.Equal(p.Identity.Audiences, []string{"ilm"}) ||
		!p.Utils {
		t.Fatalf("platform %+v", p)
	}
	stored := systemSecret(t, IdentitySecretName).Data
	if string(stored[v1alpha1.CredentialsClientSecretKey]) != identitySecret {
		t.Error("the client secret is not stored")
	}
	if string(stored[v1alpha1.CredentialsIssuerKey]) != p.Identity.Issuer || string(stored[v1alpha1.CredentialsClientIDKey]) != p.Identity.ClientID {
		t.Error("the client secret is not stored with its issuer and client ID")
	}
	if e := lastEntry(h); !slices.Contains(e.Paths, "spec.platform") {
		t.Errorf("audit paths %v", e.Paths)
	}

	var got PlatformState
	decode(t, h.request(http.MethodGet, pathPlatform, nil, s), &got)
	d := got.Dependencies
	if d.Database.Mode != ModeExternal || deref(d.Database.Username) != "ilm" || d.Database.PasswordSet == nil || !*d.Database.PasswordSet ||
		d.Identity.ClientSecretSet == nil || !*d.Identity.ClientSecretSet || d.Identity.Audiences == nil || !slices.Equal(*d.Identity.Audiences, []string{"ilm"}) ||
		!got.Utils || got.SetupComplete {
		t.Errorf("dependencies %+v utils %v", d, got.Utils)
	}

	// Before setup is complete a dependency may become managed again; its credentials go.
	body.Dependencies = &DependenciesUpdate{Database: &DatabaseDependency{Mode: ModeManaged}}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if p := getAppliance(t).Spec.Platform; p.Database != nil || p.Messaging == nil || p.Messaging.VirtualHost != "ilm" {
		t.Errorf("platform %+v, want only the database managed", p)
	}
	if secretExists(t, DatabaseSecretName) {
		t.Error("the database credentials are kept for a managed database")
	}
	if e := lastEntry(h); !slices.Contains(e.Paths, "spec.platform.database") || !slices.Contains(e.Paths, "spec.platform.database.credentialsRef") {
		t.Errorf("audit paths %v", e.Paths)
	}
}

func TestDependencyRefusals(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	for field, deps := range map[string]*DependenciesUpdate{
		"dependencies.database.mode":         {Database: &DatabaseDependency{Mode: "other"}},
		"dependencies.database.host":         {Database: externalDatabaseBody("not a host", ptr("pw"))},
		"dependencies.database.name":         {Database: &DatabaseDependency{Mode: ModeExternal, Host: ptr("db"), Username: ptr("u")}},
		"dependencies.database.port":         {Database: &DatabaseDependency{Mode: ModeExternal, Host: ptr("db"), Port: ptr(0), Name: ptr("ilm")}},
		"dependencies.database.username":     {Database: &DatabaseDependency{Mode: ModeExternal, Host: ptr("db"), Name: ptr("ilm")}},
		"dependencies.database.password":     {Database: externalDatabaseBody("db.example.com", nil)},
		"dependencies.messaging.host":        {Messaging: &MessagingDependency{Mode: ModeExternal}},
		"dependencies.identity.issuer":       {Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("http://idp.example.com")}},
		"dependencies.identity.clientId":     {Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("https://idp.example.com")}},
		"dependencies.identity.scopes":       {Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("https://idp.example.com"), ClientId: ptr("c"), Scopes: &[]string{"a b"}}},
		"dependencies.identity.audiences":    {Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("https://idp.example.com"), ClientId: ptr("c"), Audiences: &[]string{"a b"}}},
		"dependencies.identity.clientSecret": {Identity: &IdentityDependency{Mode: ModeExternal, Issuer: ptr("https://idp.example.com"), ClientId: ptr("c")}},
	} {
		body := PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Dependencies: deps}
		rec := h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), field, "")
	}
}

func TestModeSwitchRefusedAfterSetup(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)

	rec := h.request(http.MethodPut, pathPlatform, platformUpdate(&DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword))}), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.mode", "backup")
	if secretExists(t, DatabaseSecretName) || getAppliance(t).Spec.Platform.Database != nil {
		t.Fatal("a refused switch changed the appliance")
	}

	// An appliance that chose an external database before setup was complete keeps it, with editable details.
	a := getAppliance(t)
	a.Spec.Platform.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm", CredentialsRef: DatabaseSecretName}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	stored := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: DatabaseSecretName},
		Data: map[string][]byte{"username": []byte("ilm"), "password": []byte(databasePassword)}}
	if err := testClient.Create(context.Background(), stored); err != nil {
		t.Fatal(err)
	}

	rec = h.request(http.MethodPut, pathPlatform, platformUpdate(&DependenciesUpdate{Database: &DatabaseDependency{Mode: ModeManaged}}), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.mode", "fresh appliance")

	// The same server keeps the stored password; another needs it again.
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}, Utils: ptr(true),
		Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", nil)}}, s), http.StatusAccepted)
	if p := getAppliance(t).Spec.Platform; !p.Utils || p.Database == nil {
		t.Errorf("platform %+v", p)
	}
	rec = h.request(http.MethodPut, pathPlatform, platformUpdate(&DependenciesUpdate{Database: externalDatabaseBody("db2.example.com", nil)}), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.password", "server")
	// Nor is it sent to the same server over a weaker connection.
	plain := externalDatabaseBody("db.example.com", nil)
	plain.TlsMode = ptr(TLSDisable)
	rec = h.request(http.MethodPut, pathPlatform, platformUpdate(&DependenciesUpdate{Database: plain}), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.password", "server")
	wantStatus(t, h.request(http.MethodPut, pathPlatform, platformUpdate(&DependenciesUpdate{Database: externalDatabaseBody("db2.example.com", ptr(databaseNext))}), s), http.StatusAccepted)
	if got := string(systemSecret(t, DatabaseSecretName).Data["password"]); got != databaseNext {
		t.Error("the new password is not stored")
	}
	if p := getAppliance(t).Spec.Platform; p.Database.Host != "db2.example.com" || !p.Utils {
		t.Errorf("platform %+v, want the new host and utils kept", p)
	}
	if e := lastEntry(h); !slices.Equal(e.Paths, []string{"spec.platform.database", "spec.platform.database.credentialsRef"}) {
		t.Errorf("audit paths %v", e.Paths)
	}
}

func TestMessagingPortDefaultsToTheTLSPortWithTLS(t *testing.T) {
	for tls, want := range map[bool]int32{false: 5672, true: 5671} {
		in, ferr := parseDependencies(&DependenciesUpdate{Messaging: &MessagingDependency{Mode: ModeExternal, Host: ptr("mq.example.com"),
			Username: ptr("ilm"), Tls: ptr(tls)}})
		if ferr != nil || in.spec.Messaging.Port != want {
			t.Errorf("tls %v: %+v (%v), want port %d", tls, in, ferr, want)
		}
	}
}

func TestLocationHoldsTheTLSSettings(t *testing.T) {
	db := &v1alpha1.PlatformSpec{Database: &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5432, Name: "ilm"}}
	verified := db.DeepCopy()
	verified.Database.TLSMode = v1alpha1.DatabaseTLSVerifyFull
	mq := &v1alpha1.PlatformSpec{Messaging: &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5671, VirtualHost: "ilm"}}
	amqps := mq.DeepCopy()
	amqps.Messaging.TLS = true
	if location(db, v1alpha1.DependencyDatabase) == location(verified, v1alpha1.DependencyDatabase) {
		t.Error("a database location ignores the TLS mode")
	}
	if location(mq, v1alpha1.DependencyMessaging) == location(amqps, v1alpha1.DependencyMessaging) {
		t.Error("a broker location ignores TLS")
	}
}

func dependencyTest(t *testing.T, kind DependencyTestKind, settings any) DependencyTest {
	t.Helper()
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	body := DependencyTest{Kind: kind}
	if err := body.Settings.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	return body
}

func testResult(t *testing.T, h *harness, s *session, body DependencyTest) DependencyTestResult {
	t.Helper()
	rec := h.request(http.MethodPost, pathDependencyTest, body, s)
	wantStatus(t, rec, http.StatusOK)
	var got DependencyTestResult
	decode(t, rec, &got)
	return got
}

func wantResult(t *testing.T, got DependencyTestResult, reason DependencyTestResultReason) {
	t.Helper()
	if reason == "" && (!got.Ok || got.Reason != nil) || reason != "" && (got.Ok || got.Reason == nil || *got.Reason != reason) {
		t.Errorf("result %+v, want reason %q", got, reason)
	}
}

// A dependency test reads the stored credentials between saves and makes its check without holding saves up.
func TestDependencyTestWaitsForASave(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	heldDuringCheck := true
	h.srv.deps.CheckDatabase = func(context.Context, backup.ExternalDatabase, string, string, []byte) error {
		if h.srv.credentialsMu.TryLock() {
			heldDuringCheck = false
			h.srv.credentialsMu.Unlock()
		}
		return nil
	}
	body := dependencyTest(t, KindDatabase, externalDatabaseBody("db.example.com", ptr(databasePassword)))
	h.srv.credentialsMu.Lock()
	done := make(chan DependencyTestResult, 1)
	go func() { done <- testResult(t, h, s, body) }()
	select {
	case <-done:
		t.Fatal("a dependency test read the credentials during a save")
	case <-time.After(200 * time.Millisecond):
	}
	h.srv.credentialsMu.Unlock()
	wantResult(t, <-done, "")
	if heldDuringCheck {
		t.Error("the check held up the saves")
	}
}

func TestDependencyTests(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)

	t.Run("database", func(t *testing.T) {
		var checked backup.ExternalDatabase
		var user, password string
		var answer error
		h.srv.deps.CheckDatabase = func(_ context.Context, db backup.ExternalDatabase, u, p string, _ []byte) error {
			checked, user, password = db, u, p
			return answer
		}
		body := dependencyTest(t, KindDatabase, externalDatabaseBody("db.example.com", ptr(databasePassword)))
		wantResult(t, testResult(t, h, s, body), "")
		if checked.Host != "db.example.com" || checked.Port != 5432 || checked.Name != "ilm" || user != "ilm" || password != databasePassword {
			t.Errorf("checked %+v as %s", checked, user)
		}
		for _, c := range []struct {
			err    error
			reason DependencyTestResultReason
		}{
			{backup.ErrDatabaseAuthFailed, DependencyAuthFailed},
			{backup.ErrDatabaseUnreachable, DependencyUnreachable},
			{context.DeadlineExceeded, DependencyTimeout},
			{errors.New("FATAL " + databasePassword), DependencyFailed},
		} {
			answer = c.err
			wantResult(t, testResult(t, h, s, body), c.reason)
		}
		if strings.Contains(h.logs.String(), databasePassword) {
			t.Error("the log holds the database password")
		}
		e := lastEntry(h)
		if e.Action != string(AuditEntryActionDependencyTest) || e.Result != string(Failure) || e.Details["kind"] != "database" || e.Details["result"] != "failed" {
			t.Errorf("audit entry %+v", e)
		}

		rec := h.request(http.MethodPost, pathDependencyTest, dependencyTest(t, KindDatabase, externalDatabaseBody("db.example.com", nil)), s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), "settings.password", "")
		rec = h.request(http.MethodPost, pathDependencyTest, dependencyTest(t, KindDatabase, DatabaseDependency{Mode: ModeManaged}), s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), "settings.mode", "")
	})

	t.Run("messaging", func(t *testing.T) {
		var opened v1alpha1.ExternalMessaging
		var user, password string
		var answer error
		h.srv.openBroker = func(_ context.Context, mq v1alpha1.ExternalMessaging, u, p string, _ []byte) error {
			opened, user, password = mq, u, p
			return answer
		}
		settings := MessagingDependency{Mode: ModeExternal, Host: ptr("mq.example.com"), VirtualHost: ptr("ilm"), Username: ptr("ilm"), Password: ptr(messagingPassword)}
		wantResult(t, testResult(t, h, s, dependencyTest(t, KindMessaging, settings)), "")
		if opened.Host != "mq.example.com" || opened.Port != 5672 || opened.VirtualHost != "ilm" || user != "ilm" || password != messagingPassword {
			t.Errorf("opened %+v as %s", opened, user)
		}
		for _, c := range []struct {
			err    error
			reason DependencyTestResultReason
		}{
			{errBrokerAuth, DependencyAuthFailed},
			{errBrokerVhost, DependencyNotFound},
			{errInvalidAnswer, DependencyInvalid},
		} {
			answer = c.err
			wantResult(t, testResult(t, h, s, dependencyTest(t, KindMessaging, settings)), c.reason)
		}
		// Without a password the stored one is used only for the broker it was given for; none is stored here.
		settings.Password = nil
		rec := h.request(http.MethodPost, pathDependencyTest, dependencyTest(t, KindMessaging, settings), s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), "settings.password", "")
		settings.VirtualHost = nil
		body := dependencyTest(t, KindMessaging, settings)
		if in, ferr := parseTestSettings(&body); ferr != nil || in.spec.Messaging.VirtualHost != "/" {
			t.Errorf("virtual host of %+v, %v; want the default /", in, ferr)
		}
	})

	t.Run("identity", func(t *testing.T) {
		var doc map[string]any
		status := http.StatusOK
		idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/realms/ilm/.well-known/openid-configuration" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(doc)
		}))
		t.Cleanup(idp.Close)
		h.srv.identityClient = func(context.Context, *v1alpha1.Appliance) (*http.Client, error) { return idp.Client(), nil }
		issuer := idp.URL + "/realms/ilm"
		body := dependencyTest(t, KindIdentity, IdentityDependency{Mode: ModeExternal, Issuer: ptr(issuer), ClientId: ptr("ilm")})

		doc = map[string]any{"issuer": issuer, "jwks_uri": issuer + "/certs"}
		wantResult(t, testResult(t, h, s, body), "")
		doc = map[string]any{"issuer": "https://other.example.com", "jwks_uri": issuer + "/certs"}
		wantResult(t, testResult(t, h, s, body), DependencyInvalid)
		doc = map[string]any{"issuer": issuer}
		wantResult(t, testResult(t, h, s, body), DependencyInvalid)
		status = http.StatusInternalServerError
		wantResult(t, testResult(t, h, s, body), DependencyFailed)
		other := dependencyTest(t, KindIdentity, IdentityDependency{Mode: ModeExternal, Issuer: ptr(idp.URL + "/other"), ClientId: ptr("ilm")})
		wantResult(t, testResult(t, h, s, other), DependencyNotFound)
	})

	rec := h.request(http.MethodPost, pathDependencyTest, DependencyTest{Kind: "other"}, s)
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestIdentityClientUsesTheProxy(t *testing.T) {
	h := newHarness(t)
	installedAppliance(t)
	a := getAppliance(t)
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTPS: "http://proxy.example.com:3128"}
	c, err := h.srv.identityHTTPClient(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://idp.example.com/", nil)
	proxy, err := c.Transport.(*http.Transport).Proxy(req)
	if err != nil || proxy == nil || proxy.Host != "proxy.example.com:3128" {
		t.Errorf("proxy %v, %v", proxy, err)
	}
}

func TestRestoreStartWithDependencies(t *testing.T) {
	h, s, f, _, kit := freshRestore(t)
	rec := h.inspect("upload", restoreName, kit, restorePassphrase, s)
	wantStatus(t, rec, http.StatusOK)
	var sum RestoreSummary
	decode(t, rec, &sum)
	if sum.Dependencies.Database.Mode != ModeManaged || sum.Dependencies.Identity.Mode != ModeManaged {
		t.Errorf("summary dependencies %+v", sum.Dependencies)
	}

	start := RestoreStart{Inspection: h.checked(), Confirm: true, Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", nil)}}
	rec = h.request(http.MethodPost, pathRestoreStart, start, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.password", "")

	start.Dependencies = &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword)), Identity: &IdentityDependency{Mode: ModeManaged}}
	wantStatus(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusAccepted)
	d := f.started[0].opts.Dependencies
	if d == nil || !slices.Equal(d.Replace, []string{"database", "identity"}) || d.Database == nil || d.Database.Host != "db.example.com" || d.Identity != nil {
		t.Errorf("restore dependencies %+v", d)
	}
	if got := string(systemSecret(t, DatabaseSecretName).Data["password"]); got != databasePassword {
		t.Error("the entered password is not stored")
	}
	if e := lastEntry(h); e.Details["dependencies"] != "database,identity" {
		t.Errorf("audit details %v", e.Details)
	}
}

func TestRestoreStartRefusesInvalidDependencies(t *testing.T) {
	h, s, _, _, kit := freshRestore(t)
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	start := RestoreStart{Inspection: h.checked(), Confirm: true, Dependencies: &DependenciesUpdate{Database: &DatabaseDependency{Mode: "other"}}}
	rec := h.request(http.MethodPost, pathRestoreStart, start, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.database.mode", "")
}

func TestDependencyTLSSettings(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	ca := string(newTestCA(t).pem)
	put := func(deps *DependenciesUpdate) *httptest.ResponseRecorder {
		return h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Dependencies: deps}, s)
	}
	database := func(mode DatabaseDependencyTlsMode, ca *string) *DependenciesUpdate {
		d := externalDatabaseBody("db.example.com", ptr(databasePassword))
		d.TlsMode, d.Ca = &mode, ca
		return &DependenciesUpdate{Database: d}
	}
	messaging := func(tls bool, ca *string) *DependenciesUpdate {
		return &DependenciesUpdate{Messaging: &MessagingDependency{Mode: ModeExternal, Host: ptr("mq.example.com"), Port: ptr(5671),
			Username: ptr("ilm"), Password: ptr(messagingPassword), Tls: &tls, Ca: ca}}
	}
	for field, deps := range map[string]*DependenciesUpdate{
		"dependencies.database.ca":      database(TLSVerifyFull, nil),
		"dependencies.database.tlsMode": database("prefer", nil),
		"dependencies.messaging.ca":     messaging(true, ptr("not a certificate")),
	} {
		rec := put(deps)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), field, "")
	}
	rec := put(messaging(true, nil))
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "dependencies.messaging.ca", "Upload")

	wantStatus(t, put(database(TLSVerifyFull, &ca)), http.StatusAccepted)
	wantStatus(t, put(messaging(true, &ca)), http.StatusAccepted)
	p := getAppliance(t).Spec.Platform
	if p.Database.TLSMode != "verify-full" || p.Database.CA == nil || *p.Database.CA != (v1alpha1.SecretKeyRef{Name: DatabaseSecretName, Key: "ca.crt"}) ||
		!p.Messaging.TLS || p.Messaging.CA == nil {
		t.Fatalf("platform database %+v messaging %+v", p.Database, p.Messaging)
	}
	if got := string(systemSecret(t, DatabaseSecretName).Data["ca.crt"]); got != ca {
		t.Error("the database CA is not stored")
	}
	var got PlatformState
	decode(t, h.request(http.MethodGet, pathPlatform, nil, s), &got)
	if d := got.Dependencies; d.Database.TlsMode == nil || *d.Database.TlsMode != TLSVerifyFull || !*d.Database.CaSet || !*d.Messaging.Tls || !*d.Messaging.CaSet {
		t.Errorf("dependencies %+v %+v", d.Database, d.Messaging)
	}

	// The stored CA is kept without a new one, and dropped once the server is no longer verified.
	wantStatus(t, put(database(TLSVerifyFull, nil)), http.StatusAccepted)
	if got := string(systemSecret(t, DatabaseSecretName).Data["ca.crt"]); got != ca {
		t.Error("the stored CA is not kept")
	}
	wantStatus(t, put(database(TLSRequire, nil)), http.StatusAccepted)
	if p := getAppliance(t).Spec.Platform; p.Database.TLSMode != "require" || p.Database.CA != nil || len(systemSecret(t, DatabaseSecretName).Data["ca.crt"]) != 0 {
		t.Errorf("database %+v keeps a CA it does not use", p.Database)
	}
}

func TestDependencyTestsWithTLS(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	ca := newTestCA(t)

	var mode string
	var gotCA []byte
	h.srv.deps.CheckDatabase = func(_ context.Context, db backup.ExternalDatabase, _, _ string, ca []byte) error {
		mode, gotCA = db.TLSMode, ca
		return nil
	}
	settings := externalDatabaseBody("db.example.com", ptr(databasePassword))
	settings.TlsMode, settings.Ca = ptr(TLSVerifyFull), ptr(string(ca.pem))
	wantResult(t, testResult(t, h, s, dependencyTest(t, KindDatabase, settings)), "")
	if mode != "verify-full" || string(gotCA) != string(ca.pem) {
		t.Errorf("checked with mode %q and CA %q", mode, gotCA)
	}
	settings.Ca = nil
	rec := h.request(http.MethodPost, pathDependencyTest, dependencyTest(t, KindDatabase, settings), s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "settings.ca", "")

	var gotBrokerCA []byte
	h.srv.openBroker = func(_ context.Context, _ v1alpha1.ExternalMessaging, _, _ string, ca []byte) error {
		gotBrokerCA = ca
		return nil
	}
	broker := MessagingDependency{Mode: ModeExternal, Host: ptr("mq.example.com"), Port: ptr(5671), Username: ptr("ilm"), Password: ptr(messagingPassword),
		Tls: ptr(true), Ca: ptr(string(ca.pem))}
	wantResult(t, testResult(t, h, s, dependencyTest(t, KindMessaging, broker)), "")
	if string(gotBrokerCA) != string(ca.pem) {
		t.Errorf("broker opened with CA %q", gotBrokerCA)
	}
}

// listener answers every connection on 127.0.0.1 with answer, with TLS under a certificate of ca when ca is set,
// and returns its port.
func listener(t *testing.T, ca *testCA, answer string) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if ca != nil {
		key := newECKey(t)
		pair, err := tls.X509KeyPair(ca.issue(t, key.Public(), []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}), pkcs8PEM(t, key))
		if err != nil {
			t.Fatal(err)
		}
		l = tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 8)
			_, _ = conn.Read(buf)
			_, _ = conn.Write([]byte(answer))
			_ = conn.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestDialBroker(t *testing.T) {
	ca := newTestCA(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	broker := func(port int) v1alpha1.ExternalMessaging {
		return v1alpha1.ExternalMessaging{Host: "127.0.0.1", Port: int32(port), VirtualHost: "/"}
	}
	for _, tc := range []struct {
		name   string
		port   int
		ca     []byte
		reason DependencyTestResultReason
	}{
		{"unreachable", closedPort, nil, DependencyUnreachable},
		{"not AMQP", listener(t, nil, "HTTP/1.1 400 Bad Request\r\n\r\n"), nil, DependencyInvalid},
		{"closed at once", listener(t, nil, ""), nil, DependencyInvalid},
		{"untrusted", listener(t, newTestCA(t), "AMQP"), ca.pem, DependencyUntrusted},
		{"trusted but not AMQP", listener(t, ca, "HTTP/1.1 400 Bad Request\r\n\r\n"), ca.pem, DependencyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := dialBroker(ctx, broker(tc.port), "ilm", messagingPassword, tc.ca)
			if err == nil || dependencyReason(err) != tc.reason {
				t.Errorf("error %v, reason %s, want %s", err, dependencyReason(err), tc.reason)
			}
		})
	}
	for err, want := range map[error]error{
		amqp.ErrCredentials: errBrokerAuth, amqp.ErrSASL: errBrokerAuth, amqp.ErrVhost: errBrokerVhost, io.EOF: errInvalidAnswer,
	} {
		if got := brokerError(err); got != want {
			t.Errorf("brokerError(%v) = %v, want %v", err, got, want)
		}
	}
}

// TestRefusedWizardSaveKeepsDependencyCredentials is a wizard save refused on the mail password that would have
// deleted the credentials of an external database it makes managed.
func TestRefusedWizardSaveKeepsDependencyCredentials(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	mail := validMail()
	mail.Password = ptr(consoleMailPassword)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail,
		Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword))}}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)

	mail.Password, mail.Host = nil, "other-smtp.example.com"
	body.Dependencies = &DependenciesUpdate{Database: &DatabaseDependency{Mode: ModeManaged}}
	rec := h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "mail.password", "")
	if got := string(systemSecret(t, DatabaseSecretName).Data["password"]); got != databasePassword {
		t.Error("a refused save changed the database credentials")
	}
	if getAppliance(t).Spec.Platform.Database == nil {
		t.Error("a refused save changed the database")
	}
}

// A failed update of the Appliance puts the credentials it wrote back, on the Platform page and in the wizard.
func TestFailedPlatformUpdateRestoresTheCredentials(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{},
		Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword))}}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	h.srv.deps.Client = failingUpdateClient{testClient}

	body.Dependencies = &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databaseNext))}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusInternalServerError)
	if got := string(systemSecret(t, DatabaseSecretName).Data["password"]); got != databasePassword {
		t.Error("a failed wizard save changed the database credentials")
	}

	h.srv.deps.Client = testClient
	a := getAppliance(t)
	a.Status.Setup = v1alpha1.SetupComplete
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h.srv.deps.Client = failingUpdateClient{testClient}
	update := platformUpdate(&DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databaseNext))})
	update.Cbom = ptr(externalCBOM(ptr(cbomAccessKey), ptr(cbomSecretKey), nil))
	wantStatus(t, h.request(http.MethodPut, pathPlatform, update, s), http.StatusInternalServerError)
	if got := string(systemSecret(t, DatabaseSecretName).Data["password"]); got != databasePassword {
		t.Error("a failed platform update changed the database credentials")
	}
	if !secretGone(t, CBOMSecretName) {
		t.Error("a failed platform update left the CBOM store's credentials")
	}
}

// A failed wizard save puts the mail credentials back too, whether it changed or removed them.
func TestFailedWizardSaveRestoresTheMailCredentials(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	mail := validMail()
	mail.Password = ptr(consoleMailPassword)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	h.srv.deps.Client = failingUpdateClient{testClient}

	mail.Password = ptr("another-password")
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusInternalServerError)
	if got := string(systemSecret(t, MailSecretName).Data[MailPasswordKey]); got != consoleMailPassword {
		t.Error("a failed wizard save changed the mail credentials")
	}

	body.Mail = nil
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusInternalServerError)
	if secretGone(t, MailSecretName) {
		t.Error("a failed wizard save deleted the mail credentials")
	}
}

// pausedUpdateClient holds the first Update of the Appliance until release is closed, after closing paused.
type pausedUpdateClient struct {
	client.Client
	once            *sync.Once
	paused, release chan struct{}
}

func (c pausedUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*v1alpha1.Appliance); ok {
		c.once.Do(func() {
			close(c.paused)
			<-c.release
		})
	}
	return c.Client.Update(ctx, obj, opts...)
}

// Two wizard saves at once leave the database and its credentials of the same save.
func TestConcurrentWizardSavesKeepTheCredentialsWithTheirDatabase(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	paused, release := make(chan struct{}), make(chan struct{})
	h.srv.deps.Client = pausedUpdateClient{Client: testClient, once: &sync.Once{}, paused: paused, release: release}
	save := func(host, password string, done chan<- int) {
		body := PlatformSettings{HostName: "ilm.example.com", Components: []string{},
			Dependencies: &DependenciesUpdate{Database: externalDatabaseBody(host, ptr(password))}}
		done <- h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s).Code
	}

	first, second := make(chan int, 1), make(chan int, 1)
	go save("a.example.com", databasePassword, first)
	<-paused
	go save("b.example.com", databaseNext, second)
	select {
	case <-second:
		second <- http.StatusAccepted
	case <-time.After(time.Second):
	}
	close(release)
	if a, b := <-first, <-second; a != http.StatusAccepted || b != http.StatusAccepted {
		t.Fatalf("saves answered %d and %d", a, b)
	}

	host := getAppliance(t).Spec.Platform.Database.Host
	password := string(systemSecret(t, DatabaseSecretName).Data["password"])
	if (host == "a.example.com") != (password == databasePassword) {
		t.Errorf("database %s stored with the password of the other save", host)
	}
}

func TestRestoreStartRefusedKeepsNoCredentials(t *testing.T) {
	h, s, f, _, kit := freshRestore(t)
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	f.startErr = restore.ErrBackupRunning
	start := RestoreStart{Inspection: h.checked(), Confirm: true,
		Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword))}}
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusConflict, "backup_running")
	if secretExists(t, DatabaseSecretName) {
		t.Error("a refused restore left the entered credentials")
	}
}

// The credentials of a running restore are not overwritten by another start.
func TestRestoreStartWhileARestoreRunsWritesNoCredentials(t *testing.T) {
	h, s, f, _, kit := freshRestore(t)
	wantStatus(t, h.inspect("upload", restoreName, kit, restorePassphrase, s), http.StatusOK)
	a := getAppliance(t)
	a.Status.Restore = &v1alpha1.RestoreStatus{Backup: restoreName, Step: v1alpha1.RestoreStepDatabase}
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	start := RestoreStart{Inspection: h.checked(), Confirm: true,
		Dependencies: &DependenciesUpdate{Database: externalDatabaseBody("db.example.com", ptr(databasePassword))}}
	wantCode(t, h.request(http.MethodPost, pathRestoreStart, start, s), http.StatusConflict, codeRestoreBusy)
	if secretExists(t, DatabaseSecretName) || len(f.started) != 0 {
		t.Errorf("credentials written or %d restores started", len(f.started))
	}
}

func TestDependencyHostAndNameRefusals(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	for field, db := range map[string]*DatabaseDependency{
		"dependencies.database.host": externalDatabaseBody("::1", ptr("pw")),
		"dependencies.database.name": {Mode: ModeExternal, Host: ptr("db.example.com"), Name: ptr("ilm?socketFactory=x"), Username: ptr("u"), Password: ptr("pw")},
	} {
		rec := h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{},
			Dependencies: &DependenciesUpdate{Database: db}}, s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), field, "")
	}
}

func TestStoredCAIsReportedOnlyWhenStored(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupPart2)
	p := &v1alpha1.PlatformSpec{Database: &v1alpha1.ExternalDatabase{Host: "db", Port: 5432, Name: "ilm", CredentialsRef: DatabaseSecretName,
		TLSMode: "verify-full", CA: &v1alpha1.SecretKeyRef{Name: DatabaseSecretName, Key: "ca.crt"}}}
	if d := dependencyChoices(p); d.Database.CaSet != nil {
		t.Errorf("a backup's choice claims a stored CA: %+v", d.Database)
	}
	d, err := h.srv.dependenciesOf(context.Background(), p)
	if err != nil || d.Database.CaSet == nil || *d.Database.CaSet {
		t.Errorf("no CA is stored, got %+v, %v", d.Database, err)
	}
}
