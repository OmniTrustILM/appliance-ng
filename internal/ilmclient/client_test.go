package ilmclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// registeredClient returns a client of the fake edge with a registered certificate.
func registeredClient(t *testing.T) (*Client, *fakeILM) {
	t.Helper()
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	cert := adminCA.issue(t, commonName)
	ilm.users["uuid-1"] = &fakeUser{uuid: "uuid-1", username: "appliance-manager", fingerprint: fingerprint(cert.Certificate[0])}
	return New(ilm.edgeAddr(), testHost, cert, ilm.roots), ilm
}

func TestOAuth2ProviderWrittenAndRemoved(t *testing.T) {
	ctx := context.Background()
	c, ilm := registeredClient(t)

	got, err := c.GetOAuth2Provider(ctx, "appliance")
	if err != nil || got != nil {
		t.Fatalf("missing provider: %+v, %v", got, err)
	}
	want := OAuth2Provider{
		IssuerURL: "https://idp.example.com/realms/a", ClientID: "ilm", ClientSecret: "s3cret",
		AuthorizationURL: "https://idp.example.com/auth", TokenURL: "https://idp.example.com/token",
		JWKSetURL: "https://idp.example.com/certs", LogoutURL: "https://idp.example.com/logout",
		PostLogoutURL: "https://ilm.test/administrator/", UserInfoURL: "https://idp.example.com/userinfo",
		Scope: []string{"openid", "email"},
	}
	if err := c.PutOAuth2Provider(ctx, "appliance", want); err != nil {
		t.Fatal(err)
	}
	stored := ilm.providers["appliance"]
	if stored["clientSecret"] != "s3cret" || stored["jwkSetUrl"] != want.JWKSetURL || stored["issuerUrl"] != want.IssuerURL {
		t.Fatalf("stored provider %v", stored)
	}
	got, err = c.GetOAuth2Provider(ctx, "appliance")
	if err != nil || got == nil {
		t.Fatalf("written provider: %+v, %v", got, err)
	}
	want.ClientSecret = ""
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("read provider %+v, want %+v", *got, want)
	}

	if err := c.DeleteOAuth2Provider(ctx, "appliance"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ilm.providers["appliance"]; ok {
		t.Fatal("provider not removed")
	}
	for _, r := range []string{
		"PUT /api/v1/settings/authentication/oauth2Providers/appliance",
		"DELETE /api/v1/settings/authentication/oauth2Providers/appliance",
	} {
		if !slices.Contains(ilm.edgeRequests(), r) {
			t.Fatalf("no %s in %v", r, ilm.edgeRequests())
		}
	}
}

func TestClientCertificateRequired(t *testing.T) {
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	// A certificate of another CA is refused at the TLS handshake.
	stranger := newTestCA(t, "stranger").issue(t, commonName)
	err := New(ilm.edgeAddr(), testHost, stranger, ilm.roots).Profile(context.Background())
	if Reason(err) != ReasonAuthFailed {
		t.Fatalf("reason %q of %v, want %s", Reason(err), err, ReasonAuthFailed)
	}
	// A certificate of the administrator CA that no user holds is refused by Core.
	err = New(ilm.edgeAddr(), testHost, adminCA.issue(t, commonName), ilm.roots).Profile(context.Background())
	if Reason(err) != ReasonAuthFailed || !IsUnauthorized(err) {
		t.Fatalf("reason %q of %v, want %s and unauthorized", Reason(err), err, ReasonAuthFailed)
	}
}

func TestILMErrorsAreFixedReasons(t *testing.T) {
	const body = `["secret-marker must be a valid URL"]`
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, ReasonInvalid},
		{http.StatusUnauthorized, ReasonAuthFailed},
		{http.StatusForbidden, ReasonAuthFailed},
		{http.StatusNotFound, ReasonNotFound},
		{http.StatusUnprocessableEntity, ReasonInvalid},
		{http.StatusInternalServerError, ReasonFailed},
		{http.StatusBadGateway, ReasonFailed},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			c, ilm := registeredClient(t)
			ilm.failStatus, ilm.failBody = tc.status, body
			err := c.PutOAuth2Provider(context.Background(), "appliance", OAuth2Provider{JWKSetURL: "https://idp/certs"})
			if Reason(err) != tc.want {
				t.Fatalf("reason %q, want %q", Reason(err), tc.want)
			}
			if strings.Contains(err.Error(), "secret-marker") {
				t.Fatalf("the error quotes ILM's answer: %v", err)
			}
		})
	}
}

func TestNetworkErrorsAreFixedReasons(t *testing.T) {
	adminCA := newTestCA(t, "admin-ca")
	ilm := newFakeILM(t, adminCA)
	cert := adminCA.issue(t, commonName)

	// Nothing listens.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	_ = l.Close()
	if err := New(closed, testHost, cert, ilm.roots).Profile(context.Background()); Reason(err) != ReasonUnreachable {
		t.Fatalf("closed port: reason %q of %v", Reason(err), err)
	}
	// The edge's certificate is not trusted.
	if err := New(ilm.edgeAddr(), testHost, cert, nil).Profile(context.Background()); Reason(err) != ReasonUntrusted {
		t.Fatalf("untrusted edge: reason %q of %v", Reason(err), err)
	}
	// ILM does not answer in time.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := New(ilm.edgeAddr(), testHost, cert, ilm.roots).Profile(ctx); Reason(err) != ReasonTimeout {
		t.Fatalf("timeout: reason %q of %v", Reason(err), err)
	}
	if Reason(nil) != "" || Reason(errors.New("other")) != ReasonFailed {
		t.Fatal("reasons of nil and of an unknown error")
	}
}

func TestSetPlatformSettingKeepsTheOtherFields(t *testing.T) {
	ctx := context.Background()
	c, ilm := registeredClient(t)
	ilm.platform = map[string]any{
		"utils":        map[string]any{"cbomSyncPageSize": float64(1000), "utilsServiceUrl": "https://utils"},
		"certificates": map[string]any{"validationEnabled": true},
	}
	if err := c.SetPlatformSetting(ctx, "utils.cbomRepositoryUrl", "https://ilm.test/cbom-repository"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"utils": map[string]any{
			"cbomSyncPageSize": float64(1000), "utilsServiceUrl": "https://utils", "cbomRepositoryUrl": "https://ilm.test/cbom-repository",
		},
		"certificates": map[string]any{"validationEnabled": true},
	}
	if !reflect.DeepEqual(ilm.platform, want) {
		t.Fatalf("platform settings %v, want %v", ilm.platform, want)
	}
	puts := func() int {
		n := 0
		for _, r := range ilm.edgeRequests() {
			if r == "PUT /api/v1/settings/platform" {
				n++
			}
		}
		return n
	}
	// The same value again is not written.
	if err := c.SetPlatformSetting(ctx, "utils.cbomRepositoryUrl", "https://ilm.test/cbom-repository"); err != nil || puts() != 1 {
		t.Fatalf("same value: %v, %d writes", err, puts())
	}
	// A number already set is not written again, whatever its Go type.
	if err := c.SetPlatformSetting(ctx, "utils.cbomSyncPageSize", 1000); err != nil || puts() != 1 {
		t.Fatalf("same number: %v, %d writes", err, puts())
	}
	if err := c.SetPlatformSetting(ctx, "utils.bad", func() {}); err == nil {
		t.Fatal("a value JSON cannot encode was accepted")
	}
	// nil removes the field.
	if err := c.SetPlatformSetting(ctx, "utils.cbomRepositoryUrl", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := ilm.platform["utils"].(map[string]any)["cbomRepositoryUrl"]; ok {
		t.Fatal("nil did not remove the field")
	}
	// A section ILM does not have yet is created.
	if err := c.SetPlatformSetting(ctx, "other.flag", true); err != nil {
		t.Fatal(err)
	}
	if ilm.platform["other"].(map[string]any)["flag"] != true {
		t.Fatalf("platform settings %v", ilm.platform)
	}
	for _, key := range []string{"utils", ".x", "utils."} {
		if err := c.SetPlatformSetting(ctx, key, "x"); err == nil {
			t.Fatalf("key %q accepted", key)
		}
	}
}

func TestClearPlatformSettingOnlyWhileItHoldsTheValue(t *testing.T) {
	ctx := context.Background()
	c, ilm := registeredClient(t)
	ilm.platform = map[string]any{"utils": map[string]any{"cbomRepositoryUrl": "https://own.example.com/cbom", "utilsServiceUrl": "https://utils"}}
	if err := c.ClearPlatformSetting(ctx, "utils.cbomRepositoryUrl", "http://cbom-repository-service.ilm.svc:8080"); err != nil {
		t.Fatal(err)
	}
	if got := ilm.platform["utils"].(map[string]any)["cbomRepositoryUrl"]; got != "https://own.example.com/cbom" {
		t.Fatalf("a URL someone else set was cleared: %v", got)
	}
	ilm.platform["utils"].(map[string]any)["cbomRepositoryUrl"] = "http://cbom-repository-service.ilm.svc:8080"
	if err := c.ClearPlatformSetting(ctx, "utils.cbomRepositoryUrl", "http://cbom-repository-service.ilm.svc:8080"); err != nil {
		t.Fatal(err)
	}
	if utils := ilm.platform["utils"].(map[string]any); utils["cbomRepositoryUrl"] != nil || utils["utilsServiceUrl"] != "https://utils" {
		t.Fatalf("utils = %v, want the manager's URL cleared and the rest kept", utils)
	}
	if err := c.ClearPlatformSetting(ctx, "utils.x", func() {}); err == nil {
		t.Fatal("a value JSON cannot encode was accepted")
	}
}

func TestDiscoveryChecked(t *testing.T) {
	var doc map[string]any
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/a/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer idp.Close()
	issuer := idp.URL + "/realms/a"
	full := map[string]any{
		"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token",
		"jwks_uri": issuer + "/certs", "end_session_endpoint": issuer + "/logout", "userinfo_endpoint": issuer + "/userinfo",
	}

	doc = full
	if _, err := Discover(context.Background(), idp.Client(), issuer+"/"); err == nil {
		t.Fatal("an issuer that differs from the document's by a slash was accepted")
	}
	d, err := Discover(context.Background(), idp.Client(), issuer)
	if err != nil {
		t.Fatal(err)
	}
	if d.JWKSURI != issuer+"/certs" || d.AuthorizationEndpoint != issuer+"/auth" || d.TokenEndpoint != issuer+"/token" ||
		d.EndSessionEndpoint != issuer+"/logout" || d.UserInfoEndpoint != issuer+"/userinfo" {
		t.Fatalf("discovery %+v", d)
	}

	for _, missing := range []string{"issuer", "jwks_uri", "authorization_endpoint", "token_endpoint"} {
		doc = map[string]any{}
		for k, v := range full {
			if k != missing {
				doc[k] = v
			}
		}
		if _, err := Discover(context.Background(), idp.Client(), issuer); Reason(err) != ReasonInvalidIssuer {
			t.Fatalf("without %s: reason %q of %v", missing, Reason(err), err)
		}
	}
	// ILM sends the client secret to the token endpoint: no endpoint may be plain HTTP.
	for _, endpoint := range []string{"jwks_uri", "authorization_endpoint", "token_endpoint", "end_session_endpoint", "userinfo_endpoint"} {
		doc = maps.Clone(full)
		doc[endpoint] = "http://idp.example.com/" + endpoint
		if _, err := Discover(context.Background(), idp.Client(), issuer); Reason(err) != ReasonInvalidIssuer {
			t.Fatalf("plain HTTP %s: reason %q of %v", endpoint, Reason(err), err)
		}
	}
	if _, err := Discover(context.Background(), idp.Client(), idp.URL+"/nope"); Reason(err) != ReasonInvalidIssuer {
		t.Fatalf("no discovery document: reason %q of %v", Reason(err), err)
	}
	if _, err := Discover(context.Background(), &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}, issuer); Reason(err) != ReasonUntrusted {
		t.Fatalf("untrusted issuer: reason %q of %v", Reason(err), err)
	}
}
