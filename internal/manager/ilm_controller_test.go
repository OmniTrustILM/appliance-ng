package manager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// fakeEdge is the ILM edge with the OAuth2 provider and platform settings endpoints.
type fakeEdge struct {
	mu        sync.Mutex
	srv       *httptest.Server
	providers map[string]map[string]any
	// settings are the platform settings by section; a write replaces the sections it holds.
	settings map[string]any
	requests []string
	// refused, when set, answers every request with 401, as ILM does for a certificate no user holds.
	refused bool
}

func newFakeEdge(t *testing.T) *fakeEdge {
	t.Helper()
	e := &fakeEdge{providers: map[string]map[string]any{}, settings: map[string]any{}}
	const prefix = "/api/v1/settings/authentication/oauth2Providers/"
	e.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.requests = append(e.requests, r.Method+" "+r.URL.Path)
		if e.refused {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/settings/platform" {
			e.platformSettings(w, r)
			return
		}
		name := r.URL.Path[len(prefix):]
		switch r.Method {
		case http.MethodGet:
			if p, ok := e.providers[name]; ok {
				_ = json.NewEncoder(w).Encode(p)
			}
		case http.MethodPut:
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			e.providers[name] = p
		case http.MethodDelete:
			delete(e.providers, name)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeEdge) platformSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(e.settings)
		return
	}
	var sections map[string]any
	_ = json.NewDecoder(r.Body).Decode(&sections)
	for k, v := range sections {
		e.settings[k] = v
	}
}

func (e *fakeEdge) methods() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, r := range e.requests {
		method, _, _ := strings.Cut(r, " ")
		out = append(out, method)
	}
	return out
}

// fakeIdentity returns a client of the fake edge and records where the reconciler asked it to connect.
type fakeIdentity struct {
	edge        *fakeEdge
	err         error
	calls       int
	addr, host  string
	rootsWereOK bool
}

func (f *fakeIdentity) Ensure(_ context.Context, edge, ilmHost string, roots *x509.CertPool) (*ilmclient.Client, error) {
	f.calls++
	f.addr, f.host, f.rootsWereOK = edge, ilmHost, roots != nil
	if f.err != nil {
		return nil, f.err
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.edge.srv.Certificate())
	return ilmclient.New(f.edge.srv.Listener.Addr().String(), "example.com", tls.Certificate{}, pool), nil
}

// fakeIssuer serves the discovery document of an issuer; doc changes the document.
func fakeIssuer(t *testing.T, doc func(issuer string) map[string]any) (*httptest.Server, string) {
	t.Helper()
	var issuer string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/a/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc(issuer))
	}))
	t.Cleanup(srv.Close)
	issuer = srv.URL + "/realms/a"
	return srv, issuer
}

func discoveryDoc(issuer string) map[string]any {
	return map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token",
		"jwks_uri": issuer + "/certs", "end_session_endpoint": issuer + "/logout",
	}
}

// ilmAppliance creates an Appliance whose platform is ready, with the edge certificate's copy and the trust Secret
// of the issuer's CA.
func ilmAppliance(t *testing.T, idp *httptest.Server, identity *v1alpha1.ExternalIdentity) {
	t.Helper()
	ctx := context.Background()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw})
	credentials := map[string][]byte{v1alpha1.CredentialsClientSecretKey: []byte("s3cret")}
	if identity != nil {
		credentials[v1alpha1.CredentialsIssuerKey], credentials[v1alpha1.CredentialsClientIDKey] = []byte(identity.Issuer), []byte(identity.ClientID)
	}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "trust"}, Data: map[string][]byte{"ca.crt": caPEM}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "idp-credentials"}, Data: credentials},
		{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.TLSSecretName}, Data: map[string][]byte{corev1.TLSCertKey: caPEM}},
	} {
		if err := testClient.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "node.example.com", Address: "192.0.2.10/24"},
		Trust:    &v1alpha1.SecretKeyRef{Name: "trust", Key: "ca.crt"},
		Platform: &v1alpha1.PlatformSpec{HostName: "ilm.example.com", Identity: identity},
	})
	a := getAppliance(t)
	setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, reasonReady, "the Platform is available")
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
}

func reconcileILM(r *ILMReconciler) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}})
}

func TestOAuth2ProviderWrittenAndRemovedByTheReconciler(t *testing.T) {
	ctx := context.Background()
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	edge := newFakeEdge(t)
	identity := &fakeIdentity{edge: edge}
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: identity}

	res, err := reconcileILM(r)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != resync {
		t.Fatalf("requeue after %v, want %v", res.RequeueAfter, resync)
	}
	if identity.addr != "192.0.2.10:443" || identity.host != "ilm.example.com" || !identity.rootsWereOK {
		t.Fatalf("edge %q, host %q, roots %v", identity.addr, identity.host, identity.rootsWereOK)
	}
	p := edge.providers[ILMProviderName]
	want := map[string]any{
		"issuerUrl": issuer, "clientId": "ilm", "clientSecret": "s3cret",
		"authorizationUrl": issuer + "/auth", "tokenUrl": issuer + "/token", "jwkSetUrl": issuer + "/certs",
		"logoutUrl": issuer + "/logout", "postLogoutUrl": "https://ilm.example.com/administrator/",
		"scope": []any{"openid", "profile", "email"},
	}
	for k, v := range want {
		if got, _ := json.Marshal(p[k]); string(got) != mustJSON(t, v) {
			t.Errorf("provider %s = %s, want %s", k, got, mustJSON(t, v))
		}
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied)
	if c.Status != metav1.ConditionTrue || c.Reason != reasonApplied {
		t.Fatalf("condition %+v", c)
	}
	if _, ok := p["audiences"]; ok {
		t.Fatalf("audiences sent without any in the spec: %v", p["audiences"])
	}

	a := getAppliance(t)
	a.Spec.Platform.Identity = nil
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if _, ok := edge.providers[ILMProviderName]; ok {
		t.Fatal("the provider was not removed")
	}
	// Without a provider nothing is deleted.
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	deletes := 0
	for _, m := range edge.requests {
		if strings.HasPrefix(m, http.MethodDelete) {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("deleted %d times, want once", deletes)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOAuth2ProviderAudiences(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials", Audiences: []string{"ilm", "account"}})
	edge := newFakeEdge(t)
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}}
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if got, want := mustJSON(t, edge.providers[ILMProviderName]["audiences"]), `["ilm","account"]`; got != want {
		t.Fatalf("audiences %s, want %s", got, want)
	}
}

func TestDiscoveryCheckedFirst(t *testing.T) {
	idp, issuer := fakeIssuer(t, func(issuer string) map[string]any {
		doc := discoveryDoc(issuer)
		doc["issuer"] = "https://somewhere.else"
		return doc
	})
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	edge := newFakeEdge(t)
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}}

	if _, err := reconcileILM(r); err == nil {
		t.Fatal("a wrong discovery document was accepted")
	}
	if slices.Contains(edge.methods(), http.MethodPut) {
		t.Fatal("ILM was written to before the issuer was checked")
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied)
	if c.Status != metav1.ConditionFalse || c.Reason != "InvalidIssuer" || c.Message != messageILMFailed {
		t.Fatalf("condition %+v", c)
	}
}

// An empty client secret would tell ILM to keep the one it stores.
func TestOAuth2ProviderNeedsTheClientSecret(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	s := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: "idp-credentials"}, s); err != nil {
		t.Fatal(err)
	}
	s.Data = map[string][]byte{"other": []byte("x")}
	if err := testClient.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	edge := newFakeEdge(t)
	if _, err := reconcileILM(&ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}}); err == nil {
		t.Fatal("a provider without its client secret was written")
	}
	if slices.Contains(edge.methods(), http.MethodPut) {
		t.Fatal("ILM was written to without the client secret")
	}
}

// A client secret given for another issuer or client ID than the spec's is not sent to ILM.
func TestOAuth2ProviderWaitsForItsCredentials(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	a := getAppliance(t)
	a.Spec.Platform.Identity.ClientID = "other"
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	edge := newFakeEdge(t)
	res, err := reconcileILM(&ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}})
	if err != nil || res.RequeueAfter != addonsRequeue {
		t.Fatalf("result %+v, %v", res, err)
	}
	if slices.Contains(edge.methods(), http.MethodPut) {
		t.Fatal("ILM got a client secret given for another client")
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied)
	if c.Status != metav1.ConditionFalse || c.Reason != reasonCredentialsPending {
		t.Fatalf("condition %+v", c)
	}
}

func TestILMReconcilerToleratesILMDown(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	down := &ilmclient.Error{Op: "GET /api/v1/auth/profile", Reason: ilmclient.ReasonUnreachable}
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{err: down}}

	if _, err := reconcileILM(r); err == nil {
		t.Fatal("no error to back off with")
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied)
	if c.Status != metav1.ConditionFalse || c.Reason != "Unreachable" {
		t.Fatalf("condition %+v", c)
	}
}

func TestILMRefusedCertificateIsAuthFailed(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	edge := newFakeEdge(t)
	edge.refused = true
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}}

	if _, err := reconcileILM(r); err == nil {
		t.Fatal("no error to back off with")
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied)
	if c.Status != metav1.ConditionFalse || c.Reason != "AuthFailed" {
		t.Fatalf("condition %+v", c)
	}
}

func TestILMReconcilerWaits(t *testing.T) {
	ctx := context.Background()
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	identity := &fakeIdentity{edge: newFakeEdge(t)}
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: identity}

	// A restore, also in the steps before it stops ILM, which write the backup's identity Secret.
	for _, step := range []string{v1alpha1.RestoreStepSecrets, v1alpha1.RestoreStepAppliance, v1alpha1.RestoreStepDatabase} {
		a := getAppliance(t)
		a.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b.ilmbackup", Step: step, StartedAt: metav1.Now()}
		if err := testClient.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileILM(r); err != nil {
			t.Fatal(err)
		}
		if c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied); c.Reason != reasonRestoring || identity.calls != 0 {
			t.Fatalf("during the restore step %s: condition %+v, %d calls", step, c, identity.calls)
		}
	}

	// A platform that is not ready.
	a := getAppliance(t)
	a.Status.Restore = nil
	setCondition(a, v1alpha1.ConditionPlatformReady, metav1.ConditionFalse, reasonProgressing, "not yet")
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionILMSettingsApplied); c.Reason != reasonWaitingForPlatform || identity.calls != 0 {
		t.Fatalf("platform not ready: condition %+v, %d calls", c, identity.calls)
	}
}

func TestILMConditionReason(t *testing.T) {
	for reason, want := range map[string]string{
		ilmclient.ReasonAuthFailed: "AuthFailed", ilmclient.ReasonInvalidIssuer: "InvalidIssuer", ilmclient.ReasonFailed: "Failed", "": "Failed",
	} {
		if got := ilmConditionReason(reason); got != want {
			t.Errorf("%q: %q, want %q", reason, got, want)
		}
	}
}

func TestILMRelevantChanges(t *testing.T) {
	base := &v1alpha1.Appliance{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	ready := base.DeepCopy()
	setCondition(ready, v1alpha1.ConditionPlatformReady, metav1.ConditionTrue, reasonReady, "")
	newer := base.DeepCopy()
	newer.Generation = 2
	paused := base.DeepCopy()
	paused.Status.Restore = &v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepStop}
	restoring := base.DeepCopy()
	restoring.Status.Restore = &v1alpha1.RestoreStatus{Step: v1alpha1.RestoreStepSecrets}
	other := base.DeepCopy()
	other.Status.NodeAddress = "192.0.2.10"
	for name, tc := range map[string]struct {
		after *v1alpha1.Appliance
		want  bool
	}{"ready": {ready, true}, "spec": {newer, true}, "restore": {paused, true}, "early restore": {restoring, true}, "other status": {other, false}} {
		if got := ilmRelevant.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.after}); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestILMWatchesTheIdentityCredentials(t *testing.T) {
	idp, issuer := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, &v1alpha1.ExternalIdentity{Issuer: issuer, ClientID: "ilm", CredentialsRef: "idp-credentials"})
	r := &ILMReconciler{Client: testClient}
	secret := func(name string) client.Object {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}}
	}
	if got := r.identityCredentials(context.Background(), secret("idp-credentials")); len(got) != 1 {
		t.Fatalf("credentials Secret: %v", got)
	}
	if got := r.identityCredentials(context.Background(), secret("other")); len(got) != 0 {
		t.Fatalf("other Secret: %v", got)
	}
}

func TestIdentityHTTPClientUsesTheProxy(t *testing.T) {
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Proxy: &v1alpha1.ProxySpec{HTTPS: "http://proxy.example.com:3128", NoProxy: ".internal"}}}
	hc, err := (&ILMReconciler{Client: testClient}).identityHTTPClient(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	proxy := hc.Transport.(*http.Transport).Proxy
	for target, want := range map[string]string{"https://idp.example.com/": "http://proxy.example.com:3128", "https://idp.internal/": ""} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		u, err := proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if u != nil {
			got = u.String()
		}
		if got != want {
			t.Errorf("%s through %q, want %q", target, got, want)
		}
	}
}

func TestEdgeRootsNeedACertificate(t *testing.T) {
	ctx := context.Background()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.TLSSecretName}, Data: map[string][]byte{corev1.TLSCertKey: []byte("junk")}}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, s) })
	if _, err := (&ILMReconciler{APIReader: testClient}).edgeRoots(ctx); err == nil {
		t.Fatal("a Secret without a certificate gave roots")
	}
	if got := ilmHostName(&v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "node.example.com"}}}); got != "node.example.com" {
		t.Fatalf("ILM host %q, want the node's", got)
	}
}

func TestCBOMRepositoryURLWrittenAndCleared(t *testing.T) {
	ctx := context.Background()
	idp, _ := fakeIssuer(t, discoveryDoc)
	ilmAppliance(t, idp, nil)
	edge := newFakeEdge(t)
	edge.settings["utils"] = map[string]any{"utilsServiceUrl": "http://utils:8080"}
	r := &ILMReconciler{Client: testClient, APIReader: testClient, ILM: &fakeIdentity{edge: edge}}
	utils := func() map[string]any {
		edge.mu.Lock()
		defer edge.mu.Unlock()
		section, _ := edge.settings["utils"].(map[string]any)
		return section
	}

	a := getAppliance(t)
	a.Spec.Platform.CBOM = &v1alpha1.CBOMSpec{Enabled: true, Storage: v1alpha1.CBOMStorageAppliance}
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if got := utils(); got["cbomRepositoryUrl"] != "http://cbom-repository-service.ilm.svc:8080" || got["utilsServiceUrl"] != "http://utils:8080" {
		t.Fatalf("utils = %v, want the in-cluster repository URL next to the other settings", got)
	}

	a = getAppliance(t)
	a.Spec.Platform.CBOM.Enabled = false
	if err := testClient.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if got := utils(); got["cbomRepositoryUrl"] != nil || got["utilsServiceUrl"] != "http://utils:8080" {
		t.Fatalf("utils = %v, want the repository URL cleared and the rest kept", got)
	}

	// A URL the administrator set in ILM is not the manager's to clear.
	edge.mu.Lock()
	edge.settings["utils"] = map[string]any{"cbomRepositoryUrl": "https://cbom.example.com"}
	edge.mu.Unlock()
	if _, err := reconcileILM(r); err != nil {
		t.Fatal(err)
	}
	if got := utils(); got["cbomRepositoryUrl"] != "https://cbom.example.com" {
		t.Fatalf("utils = %v, want the administrator's URL kept", got)
	}
}
