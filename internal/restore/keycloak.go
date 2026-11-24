package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// The managed Keycloak as operator v1.1.0 and the Keycloak operator make it: its Service in the platform namespace,
// which serves plain HTTP behind the ILM gateway under a base path, and the realm of ILM.
const (
	keycloakService  = keycloakName + "-service"
	keycloakPort     = "8080"
	keycloakBasePath = "/kc"
	keycloakRealm    = platform.Name
	// keycloakAdminClient is the master realm's client of Keycloak's administration tools.
	keycloakAdminClient = "admin-cli"
	// logoutURIsAttribute holds a client's post-logout redirect URIs, logoutURIsSeparator apart.
	logoutURIsAttribute = "post.logout.redirect.uris"
	logoutURIsSeparator = "##"
	keycloakTimeout     = 30 * time.Second
	// maxKeycloakAnswer caps an answer of Keycloak read into memory.
	maxKeycloakAnswer = 8 << 20
)

// errKeycloakUnavailable marks a Keycloak that does not answer, or answers with a server error, as while it starts.
var errKeycloakUnavailable = errors.New("keycloak is not available")

// The fields of a Keycloak client that hold URLs, which may name the ILM host.
var clientURLFields = []string{"rootUrl", "baseUrl", "adminUrl", "redirectUris", "webOrigins"}

// keycloakServiceURL is the base URL of the managed Keycloak in the cluster.
func keycloakServiceURL() string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(keycloakService+"."+platform.Namespace+".svc", keycloakPort), Path: keycloakBasePath}
	return u.String()
}

// keycloakHTTPClient reaches Keycloak directly, never through a proxy of the environment.
func keycloakHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: keycloakTimeout, Transport: transport}
}

// rehostKeycloak moves the URLs of the Keycloak clients of ILM from the backup's ILM host name to this appliance's.
// The realm comes back with the database, and the operator imports its own realm only once, so without this the
// clients keep the backup's host name and Keycloak refuses sign-in under the new one. Client secrets stay as they are.
func (e *Engine) rehostKeycloak(ctx context.Context, j *job) error {
	members, err := readMembers(ctx, j, func(name string) bool { return name == applianceMember })
	if err != nil {
		return err
	}
	backedUp, err := applianceSpec(members)
	if err != nil {
		return err
	}
	a, err := e.appliance(ctx)
	if err != nil {
		return err
	}
	from, to := ilmHost(backedUp), ilmHost(a.Spec)
	if from == to {
		return nil
	}
	admin, err := e.liveSecret(ctx, platform.Namespace, platform.KeycloakAdminSecret)
	if err != nil {
		return err
	}
	if admin == nil {
		return fmt.Errorf("secret %s/%s not found", platform.Namespace, platform.KeycloakAdminSecret)
	}
	kc := keycloak{base: e.deps.KeycloakURL, client: e.deps.HTTPClient, user: string(admin.Data["username"]), password: string(admin.Data["password"])}
	return e.waitFor(ctx, "Keycloak", func(ctx context.Context) (bool, error) {
		moved, err := kc.rehost(ctx, from, to)
		if errors.Is(err, errKeycloakUnavailable) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		e.deps.Log.Info("moved the Keycloak clients to the ILM host name", "from", from, "to", to, "clients", moved)
		return true, nil
	})
}

// ilmHost is the ILM host name of an appliance with spec: its own, else the node's.
func ilmHost(spec v1alpha1.ApplianceSpec) string {
	if spec.Platform != nil && spec.Platform.HostName != "" {
		return spec.Platform.HostName
	}
	return spec.Node.HostName
}

// keycloak calls the admin REST API of Keycloak at base as the master realm's administrator. Its errors name the
// request and the HTTP status, never a credential or an answer.
type keycloak struct {
	base, user, password string
	client               *http.Client
}

// rehost moves the URLs of every client of the ILM realm from the host from to the host to and returns how many
// clients changed. A client whose URLs do not name from is left alone, so rehost can run again.
func (k keycloak) rehost(ctx context.Context, from, to string) (int, error) {
	token, err := k.token(ctx)
	if err != nil {
		return 0, err
	}
	clientsPath := "/admin/realms/" + keycloakRealm + "/clients"
	var clients []map[string]any
	if err := k.call(ctx, http.MethodGet, clientsPath, token, nil, &clients); err != nil {
		return 0, err
	}
	moved := 0
	for _, c := range clients {
		if !rehostClient(c, from, to) {
			continue
		}
		id, _ := c["id"].(string)
		if err := k.call(ctx, http.MethodPut, clientsPath+"/"+url.PathEscape(id), token, c, nil); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// token signs in to the master realm and returns the access token.
func (k keycloak) token(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {keycloakAdminClient}, "username": {k.user}, "password": {k.password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.base+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := k.do(req, &answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", errors.New("keycloak answered the sign-in without a token")
	}
	return answer.AccessToken, nil
}

// call sends body as JSON to the admin path with token and decodes the answer into out, when set.
func (k keycloak) call(ctx context.Context, method, path, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return k.do(req, out)
}

func (k keycloak) do(req *http.Request, out any) error {
	res, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s", errKeycloakUnavailable, req.Method, req.URL.Path)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %s %s answered HTTP %d", errKeycloakUnavailable, req.Method, req.URL.Path, res.StatusCode)
	case res.StatusCode >= http.StatusMultipleChoices:
		return fmt.Errorf("keycloak: %s %s answered HTTP %d", req.Method, req.URL.Path, res.StatusCode)
	case out == nil:
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, maxKeycloakAnswer))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("keycloak: %s %s: the answer is not the expected JSON", req.Method, req.URL.Path)
	}
	return nil
}

// rehostClient moves the URLs of the client c, a Keycloak ClientRepresentation, from the host from to the host to,
// and tells whether any changed.
func rehostClient(c map[string]any, from, to string) bool {
	changed := false
	for _, field := range clientURLFields {
		if v, ok := rehostValue(c[field], from, to); ok {
			c[field], changed = v, true
		}
	}
	attributes, _ := c["attributes"].(map[string]any)
	return rehostLogoutURIs(attributes, from, to) || changed
}

// rehostLogoutURIs moves the post-logout redirect URIs among a client's attributes from the host from to the host to,
// and tells whether any changed.
func rehostLogoutURIs(attributes map[string]any, from, to string) bool {
	uris, ok := attributes[logoutURIsAttribute].(string)
	if !ok {
		return false
	}
	list := strings.Split(uris, logoutURIsSeparator)
	changed := false
	for i, u := range list {
		if moved, ok := rehostURL(u, from, to); ok {
			list[i], changed = moved, true
		}
	}
	if changed {
		attributes[logoutURIsAttribute] = strings.Join(list, logoutURIsSeparator)
	}
	return changed
}

// rehostValue moves a URL, or each URL of a list, from the host from to the host to, and tells whether any changed.
func rehostValue(v any, from, to string) (any, bool) {
	switch v := v.(type) {
	case string:
		return rehostURL(v, from, to)
	case []any:
		changed := false
		for i, item := range v {
			if moved, ok := rehostValue(item, from, to); ok {
				v[i], changed = moved, true
			}
		}
		return v, changed
	}
	return v, false
}

// rehostURL replaces the host of the absolute URL s when it is from, keeping everything else, such as a port or a
// wildcard path, and tells whether it did.
func rehostURL(s, from, to string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() != from {
		return s, false
	}
	moved := strings.Replace(s, "//"+from, "//"+to, 1)
	return moved, moved != s
}
