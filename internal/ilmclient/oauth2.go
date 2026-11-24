package ilmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OAuth2Provider is an OAuth2 provider of ILM's authentication settings. ILM never returns the client secret, and an
// empty one on a write keeps the stored secret.
type OAuth2Provider struct {
	IssuerURL        string   `json:"issuerUrl,omitempty"`
	ClientID         string   `json:"clientId,omitempty"`
	ClientSecret     string   `json:"clientSecret,omitempty"`
	AuthorizationURL string   `json:"authorizationUrl,omitempty"`
	TokenURL         string   `json:"tokenUrl,omitempty"`
	JWKSetURL        string   `json:"jwkSetUrl,omitempty"`
	LogoutURL        string   `json:"logoutUrl,omitempty"`
	PostLogoutURL    string   `json:"postLogoutUrl,omitempty"`
	UserInfoURL      string   `json:"userInfoUrl,omitempty"`
	Scope            []string `json:"scope,omitempty"`
	// Audiences are the audiences ILM accepts in the tokens; none accepts any token of the issuer.
	Audiences []string `json:"audiences,omitempty"`
}

func providerPath(name string) string {
	return "/api/v1/settings/authentication/oauth2Providers/" + url.PathEscape(name)
}

// GetOAuth2Provider reads the provider name, or returns nil when ILM has none of that name: ILM then answers with an
// empty body.
func (c *Client) GetOAuth2Provider(ctx context.Context, name string) (*OAuth2Provider, error) {
	path := providerPath(name)
	answer, err := c.call(ctx, http.MethodGet, path, nil)
	if err != nil || len(bytes.TrimSpace(answer)) == 0 {
		return nil, err
	}
	p := &OAuth2Provider{}
	if err := decode(http.MethodGet+" "+path, answer, p); err != nil {
		return nil, err
	}
	return p, nil
}

// PutOAuth2Provider writes the provider name.
func (c *Client) PutOAuth2Provider(ctx context.Context, name string, p OAuth2Provider) error {
	_, err := c.call(ctx, http.MethodPut, providerPath(name), p)
	return err
}

// DeleteOAuth2Provider removes the provider name; removing one that does not exist succeeds.
func (c *Client) DeleteOAuth2Provider(ctx context.Context, name string) error {
	_, err := c.call(ctx, http.MethodDelete, providerPath(name), nil)
	return err
}

// Discovery is what ILM needs of an issuer's OpenID discovery document.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
	UserInfoEndpoint      string `json:"userinfo_endpoint"`
}

// Discover fetches the discovery document of issuer with hc and checks that it names exactly issuer and the endpoints
// ILM signs users in with. ILM checks none of this when the provider is written.
func Discover(ctx context.Context, hc *http.Client, issuer string) (*Discovery, error) {
	const op = "discover the issuer"
	invalid := func(why string) error { return &Error{Op: op, Reason: ReasonInvalidIssuer, cause: errors.New(why)} }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, invalid("the issuer is not a URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &Error{Op: op, Reason: networkReason(err), cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Op: op, Status: resp.StatusCode, Reason: ReasonInvalidIssuer}
	}
	d := &Discovery{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxDiscovery)).Decode(d); err != nil {
		return nil, invalid("the discovery document is not JSON")
	}
	switch {
	case d.Issuer != issuer:
		return nil, invalid("the discovery document names another issuer")
	case d.JWKSURI == "" || d.AuthorizationEndpoint == "" || d.TokenEndpoint == "":
		return nil, invalid("the discovery document lacks an endpoint")
	case !allHTTPS(d.JWKSURI, d.AuthorizationEndpoint, d.TokenEndpoint, d.EndSessionEndpoint, d.UserInfoEndpoint):
		return nil, invalid("the discovery document names an endpoint without https")
	}
	return d, nil
}

// allHTTPS tells whether each of the endpoints given is an https URL; ILM sends the client secret to one of them.
func allHTTPS(endpoints ...string) bool {
	for _, e := range endpoints {
		if u, err := url.Parse(e); e != "" && (err != nil || u.Scheme != "https") {
			return false
		}
	}
	return true
}
