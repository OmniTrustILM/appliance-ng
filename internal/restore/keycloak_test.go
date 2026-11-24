package restore

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	fakeToken          = "admin-token"
	ilmClientSecret    = "ilm-client-secret"
	backupILMHost      = "ilm-old.example.com"
	ilmLoginPath       = "/api/login/oauth2/code/internal"
	keycloakClientsURL = "/kc/admin/realms/ilm/clients"
)

// fakeKeycloak is the admin REST API of the managed Keycloak holding the backup's realm of ILM: the ILM client with
// the backup's ILM host name in its URLs, and a built-in client with relative ones. down answers 503 to everything.
type fakeKeycloak struct {
	url string

	mu       sync.Mutex
	clients  []map[string]any
	down     bool
	password string
	requests int
	updates  []string
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	k := &fakeKeycloak{password: "backup-pw", clients: []map[string]any{
		{
			"id": "c1", "clientId": "ilm", "secret": ilmClientSecret, "enabled": true, "notBefore": 0,
			"redirectUris": []any{"https://" + backupILMHost + ilmLoginPath},
			"webOrigins":   []any{"https://" + backupILMHost},
			"attributes":   map[string]any{"post.logout.redirect.uris": "https://" + backupILMHost + "/administrator/##https://other.example.com/"},
		},
		{
			"id": "c2", "clientId": "account", "rootUrl": "${authBaseUrl}", "baseUrl": "/realms/ilm/account/",
			"redirectUris": []any{"/realms/ilm/account/*"}, "webOrigins": []any{"+"},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(srv.Close)
	k.url = srv.URL + "/kc"
	return k
}

func (k *fakeKeycloak) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.requests++
	switch {
	case k.down:
		w.WriteHeader(http.StatusServiceUnavailable)
	case r.Method == http.MethodPost && r.URL.Path == "/kc/realms/master/protocol/openid-connect/token":
		if r.FormValue("grant_type") != "password" || r.FormValue("client_id") != "admin-cli" ||
			r.FormValue("username") != "admin" || r.FormValue("password") != k.password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeToken})
	case r.Header.Get("Authorization") != "Bearer "+fakeToken:
		w.WriteHeader(http.StatusUnauthorized)
	case r.Method == http.MethodGet && r.URL.Path == keycloakClientsURL:
		_ = json.NewEncoder(w).Encode(k.clients)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, keycloakClientsURL+"/"):
		k.update(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (k *fakeKeycloak) update(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, keycloakClientsURL+"/")
	var c map[string]any
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || c["id"] != id {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	i := slices.IndexFunc(k.clients, func(old map[string]any) bool { return old["id"] == id })
	if i < 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	k.clients[i] = c
	k.updates = append(k.updates, id)
	w.WriteHeader(http.StatusNoContent)
}

func (k *fakeKeycloak) client(t *testing.T, clientID string) map[string]any {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.clients, func(c map[string]any) bool { return c["clientId"] == clientID })
	if i < 0 {
		t.Fatalf("no client %s", clientID)
	}
	return k.clients[i]
}

func (k *fakeKeycloak) counts() (requests int, updates []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.requests, slices.Clone(k.updates)
}

func TestFreshRestoreMovesKeycloakToTheNewILMHostName(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	f.wantDone(t)
	ilm := f.keycloak.client(t, "ilm")
	if got := ilm["redirectUris"]; !slices.Equal(toStrings(got), []string{"https://" + newILMHost + ilmLoginPath}) {
		t.Errorf("redirect URIs %v, want the new ILM host name's", got)
	}
	if got := ilm["webOrigins"]; !slices.Equal(toStrings(got), []string{"https://" + newILMHost}) {
		t.Errorf("web origins %v, want the new ILM host name's", got)
	}
	attributes, _ := ilm["attributes"].(map[string]any)
	if got, want := attributes["post.logout.redirect.uris"], "https://"+newILMHost+"/administrator/##https://other.example.com/"; got != want {
		t.Errorf("post-logout redirect URIs %v, want %s", got, want)
	}
	if ilm["secret"] != ilmClientSecret || ilm["enabled"] != true || ilm["notBefore"] != float64(0) {
		t.Errorf("ilm client %v, want its secret and other fields kept", ilm)
	}
	if _, updates := f.keycloak.counts(); !slices.Equal(updates, []string{"c1"}) {
		t.Errorf("updated clients %v, want only the ILM client", updates)
	}
	for _, secret := range []string{"backup-pw", ilmClientSecret, fakeToken} {
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("the log carries %q:\n%s", secret, f.logs)
		}
	}
}

func TestRestoreUnderTheBackupsILMHostNameLeavesKeycloak(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.start(t, f.engine(testClient), Options{})
	f.wantDone(t)
	if requests, _ := f.keycloak.counts(); requests != 0 {
		t.Errorf("%d Keycloak requests, want none under the backup's ILM host name", requests)
	}
}

func TestKeycloakStepWaitsForKeycloakAndRunsAgain(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.keycloak.down = true
	e := f.engine(testClient)
	e.afterStep = func(step string) bool {
		f.ran = append(f.ran, step)
		if step == v1alpha1.RestoreStepStart {
			go func() {
				for requests, _ := f.keycloak.counts(); requests < 2; requests, _ = f.keycloak.counts() {
					time.Sleep(5 * time.Millisecond)
				}
				f.keycloak.mu.Lock()
				f.keycloak.down = false
				f.keycloak.mu.Unlock()
			}()
		}
		return step == v1alpha1.RestoreStepKeycloak
	}
	f.start(t, e, Options{ILMHostName: newILMHost})
	if st := getAppliance(t).Status.Restore; st.Step != v1alpha1.RestoreStepKeycloak || st.Message != "" {
		t.Fatalf("status %+v, want the keycloak step done and recorded", st)
	}

	// Run again, as after a restart: the clients already carry the new host name and stay as they are.
	again := f.engine(testClient)
	if err := again.rehostKeycloak(context.Background(), &job{id: f.key, status: *getAppliance(t).Status.Restore}); err != nil {
		t.Fatal(err)
	}
	if _, updates := f.keycloak.counts(); !slices.Equal(updates, []string{"c1"}) {
		t.Errorf("updated clients %v, want the ILM client once", updates)
	}
}

func TestKeycloakStepFailsWithAFixedReason(t *testing.T) {
	f := newRestoreFixture(t, false)
	f.keycloak.password = "another-pw"
	f.start(t, f.engine(testClient), Options{ILMHostName: newILMHost})
	st := getAppliance(t).Status.Restore
	if st.Step != v1alpha1.RestoreStepKeycloak || st.Message != v1alpha1.RestoreFailed {
		t.Errorf("status %+v, want failed at keycloak", st)
	}
	if n := replicas(t, "ilm-operator-system", "ilm-operator-controller-manager"); n != 1 {
		t.Errorf("operator replicas = %d, want 1", n)
	}
	if strings.Contains(f.logs.String(), "backup-pw") {
		t.Errorf("the log carries the Keycloak administrator's password:\n%s", f.logs)
	}
}

func TestRehostURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://old.example.com/api/login":         "https://new.example.com/api/login",
		"https://old.example.com":                   "https://new.example.com",
		"https://old.example.com:8443/x/*":          "https://new.example.com:8443/x/*",
		"https://older.example.com/api/login":       "https://older.example.com/api/login",
		"https://other.example.com/old.example.com": "https://other.example.com/old.example.com",
		"/realms/ilm/account/*":                     "/realms/ilm/account/*",
		"${authBaseUrl}":                            "${authBaseUrl}",
		"*":                                         "*",
		"+":                                         "+",
	} {
		got, moved := rehostURL(in, "old.example.com", "new.example.com")
		if got != want || moved != (in != want) {
			t.Errorf("rehostURL(%q) = %q, %v; want %q", in, got, moved, want)
		}
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// TestKeycloakURLIsTheServicesClusterIP: the manager runs in the host network without the cluster's DNS, so it calls
// Keycloak at the cluster IP of its Service.
func TestKeycloakURLIsTheServicesClusterIP(t *testing.T) {
	resetCluster(t)
	e := New(Deps{Client: testClient})
	if _, err := e.keycloakURL(context.Background()); err == nil {
		t.Error("keycloakURL without the Service passed")
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ilm", Name: "ilm-keycloak-service"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	mustCreate(t, svc)
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), svc) })
	got, err := e.keycloakURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil || u.Scheme != "http" || u.Host != net.JoinHostPort(svc.Spec.ClusterIP, "8080") || u.Path != "/kc" {
		t.Errorf("Keycloak URL %q, want the Service's cluster IP %s under /kc", got, svc.Spec.ClusterIP)
	}
	if e.deps.HTTPClient == nil || e.deps.HTTPClient.Transport.(*http.Transport).Proxy != nil {
		t.Error("want an HTTP client that uses no proxy")
	}
}
