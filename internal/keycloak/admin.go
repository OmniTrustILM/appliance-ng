// Package keycloak calls the admin REST API of the managed Keycloak.
package keycloak

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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// The managed Keycloak as operator v1.1.0 and the Keycloak operator make it: its Service in the platform namespace,
// which serves plain HTTP behind the ILM gateway under a base path and admits any source on that port, and the realm
// of ILM.
const (
	serviceName = platform.Name + "-keycloak-service"
	servicePort = "8080"
	basePath    = "/kc"
	// Realm is the realm of ILM.
	Realm = platform.Name
	// adminClient is the master realm's client of Keycloak's administration tools.
	adminClient = "admin-cli"
	timeout     = 30 * time.Second
	// maxAnswer caps an answer of Keycloak read into memory.
	maxAnswer = 8 << 20
)

// ErrUnavailable marks a Keycloak that does not answer, or answers with a server error, as while it starts.
var ErrUnavailable = errors.New("keycloak is not available")

// URL is the base URL of the managed Keycloak: override when set, else the cluster IP of its Service. The manager runs
// in the host network, where the cluster's DNS does not answer.
func URL(ctx context.Context, reader client.Reader, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	svc := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: platform.Namespace, Name: serviceName}, svc); err != nil {
		return "", fmt.Errorf("read service %s/%s: %w", platform.Namespace, serviceName, err)
	}
	ip := svc.Spec.ClusterIP
	if ip == "" || ip == corev1.ClusterIPNone {
		return "", fmt.Errorf("service %s/%s has no cluster IP", platform.Namespace, serviceName)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(ip, servicePort), Path: basePath}
	return u.String(), nil
}

// HTTPClient reaches Keycloak directly, never through a proxy of the environment.
func HTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: timeout, Transport: transport}
}

// Admin calls the admin REST API of Keycloak at Base as the master realm's administrator. Its errors name the
// request and the HTTP status, never a credential or an answer.
type Admin struct {
	Base, User, Password string
	Client               *http.Client
}

// Token signs in to the master realm and returns the access token.
func (a Admin) Token(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"password"}, "client_id": {adminClient}, "username": {a.User}, "password": {a.Password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Base+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.do(req, &answer); err != nil {
		return "", err
	}
	if answer.AccessToken == "" {
		return "", errors.New("keycloak answered the sign-in without a token")
	}
	return answer.AccessToken, nil
}

// Call sends body as JSON to the admin path with token and decodes the answer into out, when set.
func (a Admin) Call(ctx context.Context, method, path, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.do(req, out)
}

func (a Admin) do(req *http.Request, out any) error {
	res, err := a.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s", ErrUnavailable, req.Method, req.URL.Path)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %s %s answered HTTP %d", ErrUnavailable, req.Method, req.URL.Path, res.StatusCode)
	case res.StatusCode >= http.StatusMultipleChoices:
		return fmt.Errorf("keycloak: %s %s answered HTTP %d", req.Method, req.URL.Path, res.StatusCode)
	case out == nil:
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, maxAnswer))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("keycloak: %s %s: the answer is not the expected JSON", req.Method, req.URL.Path)
	}
	return nil
}

// SetPassword sets the permanent password of the user username of realm to password.
func (a Admin) SetPassword(ctx context.Context, realm, username, password string) error {
	token, err := a.Token(ctx)
	if err != nil {
		return err
	}
	usersPath := "/admin/realms/" + url.PathEscape(realm) + "/users"
	query := url.Values{"username": {username}, "exact": {"true"}}
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := a.Call(ctx, http.MethodGet, usersPath+"?"+query.Encode(), token, nil, &users); err != nil {
		return err
	}
	id := ""
	for _, u := range users {
		if u.Username == username {
			id = u.ID
		}
	}
	if id == "" {
		return fmt.Errorf("keycloak: realm %s has no user %s", realm, username)
	}
	credential := map[string]any{"type": "password", "value": password, "temporary": false}
	return a.Call(ctx, http.MethodPut, usersPath+"/"+url.PathEscape(id)+"/reset-password", token, credential, nil)
}
