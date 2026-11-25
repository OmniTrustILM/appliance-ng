package ilmclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// answer is what the routed server sends for one route.
type answer struct {
	status int
	body   string
}

// routedClient returns a client of a server that answers each "METHOD path" from routes, 404 otherwise, and
// records the requests.
func routedClient(t *testing.T, routes map[string]answer) (*Client, func() []connectorCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []connectorCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, connectorCall{r.Method, r.URL.RequestURI(), string(data)})
		mu.Unlock()
		a, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			a = answer{status: http.StatusNotFound}
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return newLocal(srv.URL), func() []connectorCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]connectorCall(nil), calls...)
	}
}

func TestListRAProfiles(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{"GET /api/v1/raProfiles": {http.StatusOK,
		`[{"uuid":"r1","name":"web","enabled":true,"authorityInstanceName":"x"},{"uuid":"r2","name":"old","enabled":false}]`}})
	got, err := c.ListRAProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []RAProfile{{UUID: "r1", Name: "web", Enabled: true}, {UUID: "r2", Name: "old"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("profiles %+v, want %+v", got, want)
	}
	if want := []connectorCall{{http.MethodGet, "/api/v1/raProfiles", ""}}; !reflect.DeepEqual(calls(), want) {
		t.Errorf("calls %+v, want %+v", calls(), want)
	}
}

func TestListVaultProfilesKeepsTheEnabledOnes(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{"POST /api/v1/vaultProfiles/list": {http.StatusOK, `{"items":[
		{"uuid":"p1","name":"hashicorp","enabled":true,"vaultInstance":{"uuid":"v1","name":"vault"}},
		{"uuid":"p2","name":"off","enabled":false,"vaultInstance":{"uuid":"v1","name":"vault"}}],"totalItems":2}`}})
	got, err := c.ListVaultProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []VaultProfile{{UUID: "p1", Name: "hashicorp", VaultUUID: "v1"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("profiles %+v, want %+v", got, want)
	}
	want := []connectorCall{{http.MethodPost, "/api/v1/vaultProfiles/list", `{"itemsPerPage":1000,"pageNumber":1}`}}
	if !reflect.DeepEqual(calls(), want) {
		t.Errorf("calls %+v, want %+v", calls(), want)
	}
}

const eabKey = "c2VjcmV0LWVhYi1rZXk"

func TestGenerateEABKeyAndStoreIt(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{
		"POST /api/v1/acmeProfiles/eabKeys":               {http.StatusOK, `{"key":"` + eabKey + `"}`},
		"POST /api/v1/vaults/v1/vaultProfiles/p1/secrets": {http.StatusCreated, `{"uuid":"s1","name":"appliance-acme-eab"}`},
	})
	ctx := context.Background()
	key, err := c.GenerateEABKey(ctx)
	if err != nil || key != eabKey {
		t.Fatalf("key %q, err %v", key, err)
	}
	uuid, err := c.CreateSecretKey(ctx, "v1", "p1", "appliance-acme-eab", key)
	if err != nil || uuid != "s1" {
		t.Fatalf("uuid %q, err %v", uuid, err)
	}
	want := []connectorCall{
		{http.MethodPost, "/api/v1/acmeProfiles/eabKeys", ""},
		{http.MethodPost, "/api/v1/vaults/v1/vaultProfiles/p1/secrets",
			`{"name":"appliance-acme-eab","secret":{"type":"secretKey","content":"` + eabKey + `"}}`},
	}
	if !reflect.DeepEqual(calls(), want) {
		t.Errorf("calls %+v, want %+v", calls(), want)
	}
}

func TestEABKeyNeverInErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		c, _ := routedClient(t, map[string]answer{
			"POST /api/v1/acmeProfiles/eabKeys":               {http.StatusOK, `not json ` + eabKey},
			"POST /api/v1/vaults/v1/vaultProfiles/p1/secrets": {status, `["could not store ` + eabKey + `"]`},
		})
		ctx := context.Background()
		_, genErr := c.GenerateEABKey(ctx)
		_, createErr := c.CreateSecretKey(ctx, "v1", "p1", "n", eabKey)
		for _, err := range []error{genErr, createErr} {
			if err == nil || strings.Contains(err.Error(), eabKey) {
				t.Errorf("status %d: error %v", status, err)
			}
		}
		if (status == http.StatusInternalServerError) == IsRefusal(createErr) {
			t.Errorf("status %d: refusal %v", status, IsRefusal(createErr))
		}
	}
}

func TestSecretExists(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{
		"GET /api/v1/secrets/s1": {http.StatusOK, `{"uuid":"s1"}`},
		"GET /api/v1/secrets/s3": {http.StatusInternalServerError, ""},
	})
	ctx := context.Background()
	for uuid, want := range map[string]bool{"s1": true, "s2": false} {
		if got, err := c.SecretExists(ctx, uuid); err != nil || got != want {
			t.Errorf("SecretExists(%s) = %v, %v; want %v", uuid, got, err, want)
		}
	}
	if got, err := c.SecretExists(ctx, "s3"); err == nil || got {
		t.Errorf("SecretExists after a failure = %v, %v; want the error", got, err)
	}
	if n := len(calls()); n != 3 {
		t.Errorf("%d calls, want 3", n)
	}
}

var applianceProfile = ACMEProfile{
	Name: "appliance", RAProfileUUID: "r1", EABSecretUUIDs: []string{"s1"},
	Identifiers: []PreauthorizedIdentifier{
		{Type: "dns", Value: "pki.example.com", MatchType: "exact"},
		{Type: "ip", Value: "192.0.2.10", MatchType: "exact"},
		{Type: "dns", Value: "appliance.example.com", MatchType: "exact"},
	},
}

// profileBody is what EnsureACMEProfile sends for applianceProfile, after the name, when it is given.
const profileBody = `"raProfileUuid":"r1","issueCertificateAttributes":[],"revokeCertificateAttributes":[],` +
	`"requireContact":false,"requireTermsOfService":false,"eabSecretUuids":["s1"],"preauthorizedIdentifiers":[` +
	`{"type":"dns","value":"appliance.example.com","matchType":"exact"},` +
	`{"type":"dns","value":"pki.example.com","matchType":"exact"},` +
	`{"type":"ip","value":"192.0.2.10","matchType":"exact"}],"identifierAuthorizationMode":"preauthorizedOnly"}`

func TestEnsureACMEProfileCreatesAndEnables(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{
		"GET /api/v1/acmeProfiles":             {http.StatusOK, `[{"uuid":"a0","name":"other","enabled":true}]`},
		"POST /api/v1/acmeProfiles":            {http.StatusCreated, `{"uuid":"a1"}`},
		"PATCH /api/v1/acmeProfiles/a1/enable": {http.StatusNoContent, ""},
	})
	uuid, err := c.EnsureACMEProfile(context.Background(), applianceProfile)
	if err != nil || uuid != "a1" {
		t.Fatalf("uuid %q, err %v", uuid, err)
	}
	want := []connectorCall{
		{http.MethodGet, "/api/v1/acmeProfiles", ""},
		{http.MethodPost, "/api/v1/acmeProfiles", `{"name":"appliance",` + profileBody},
		{http.MethodPatch, "/api/v1/acmeProfiles/a1/enable", ""},
	}
	if !reflect.DeepEqual(calls(), want) {
		t.Errorf("calls %+v\nwant %+v", calls(), want)
	}
}

// profileDetail is ILM's answer for applianceProfile, its identifiers in another order.
const profileDetail = `{"uuid":"a1","name":"appliance","enabled":true,"raProfile":{"uuid":"r1","name":"web"},
	"eabSecretUuids":["s1"],"identifierAuthorizationMode":"preauthorizedOnly","preauthorizedIdentifiers":[
	{"type":"ip","value":"192.0.2.10","matchType":"exact","allowWildcard":false},
	{"type":"dns","value":"pki.example.com","matchType":"exact","allowWildcard":false},
	{"type":"dns","value":"appliance.example.com","matchType":"exact","allowWildcard":false}]}`

func TestEnsureACMEProfileLeavesAMatchingOneAlone(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{
		"GET /api/v1/acmeProfiles":    {http.StatusOK, `[{"uuid":"a1","name":"appliance","enabled":true}]`},
		"GET /api/v1/acmeProfiles/a1": {http.StatusOK, profileDetail},
	})
	uuid, err := c.EnsureACMEProfile(context.Background(), applianceProfile)
	if err != nil || uuid != "a1" {
		t.Fatalf("uuid %q, err %v", uuid, err)
	}
	for _, call := range calls() {
		if call.method != http.MethodGet {
			t.Errorf("wrote %+v to a matching profile", call)
		}
	}
}

func TestEnsureACMEProfileUpdatesChangedIdentifiers(t *testing.T) {
	c, calls := routedClient(t, map[string]answer{
		"GET /api/v1/acmeProfiles":             {http.StatusOK, `[{"uuid":"a1","name":"appliance","enabled":false}]`},
		"GET /api/v1/acmeProfiles/a1":          {http.StatusOK, strings.Replace(profileDetail, "192.0.2.10", "192.0.2.99", 1)},
		"PUT /api/v1/acmeProfiles/a1":          {http.StatusOK, profileDetail},
		"PATCH /api/v1/acmeProfiles/a1/enable": {http.StatusNoContent, ""},
	})
	if _, err := c.EnsureACMEProfile(context.Background(), applianceProfile); err != nil {
		t.Fatal(err)
	}
	got := calls()
	want := []connectorCall{
		{http.MethodPut, "/api/v1/acmeProfiles/a1", "{" + profileBody},
		{http.MethodPatch, "/api/v1/acmeProfiles/a1/enable", ""},
	}
	if len(got) != 4 || !reflect.DeepEqual(got[2:], want) {
		t.Errorf("calls %+v\nwant the reads and %+v", got, want)
	}
}

func TestEnsureACMEProfileRefusalIsReported(t *testing.T) {
	c, _ := routedClient(t, map[string]answer{
		"GET /api/v1/acmeProfiles":  {http.StatusOK, `[]`},
		"POST /api/v1/acmeProfiles": {http.StatusBadRequest, `["RA Profile r1 not found"]`},
	})
	_, err := c.EnsureACMEProfile(context.Background(), applianceProfile)
	if !IsRefusal(err) || strings.Contains(err.Error(), "not found") {
		t.Errorf("err %v, want a refusal without ILM's answer", err)
	}
}
