package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/internal/recovery"
)

const (
	// markerGroup is the ILM group the backup holds and the restore must bring back.
	markerGroup = "restore-marker"
	groupsPath  = "/api/v1/groups"
	// tokenPath is the token endpoint of the ILM realm of the managed Keycloak, behind the ILM edge.
	tokenPath = "/kc/realms/ilm/protocol/openid-connect/token"
	// authPath is the authorization endpoint of the same realm, and loginCallbackPath the path of the redirect URI
	// ILM signs in with.
	authPath          = "/kc/realms/ilm/protocol/openid-connect/auth"
	loginCallbackPath = "/api/login/oauth2/code/internal"
	ilmClientID       = "ilm"
	// The Secret in which the ILM operator keeps the secret of the ILM client of Keycloak.
	oidcNamespace = "ilm"
	oidcSecret    = "ilm-oidc-client"
	oidcSecretKey = "clientSecret"
	// noServiceAccount is Keycloak's description of an accepted secret on a client without a service account.
	noServiceAccount = "not enabled to retrieve service account"
)

// ilmMarker creates the marker group, or checks it and the Keycloak client after a restore.
func ilmMarker(ctx context.Context, o options) error {
	password, err := readSecret(o.credentialPasswordFile)
	if err != nil {
		return err
	}
	pfx, err := os.ReadFile(o.credential)
	if err != nil {
		return err
	}
	cert, err := credentialCertificate(pfx, password)
	if err != nil {
		return err
	}
	c, err := managerClient(ctx, o)
	if err != nil {
		return err
	}
	ui, err := uiClient(ctx, c, o, cert)
	if err != nil {
		return err
	}
	if o.create {
		return waitUntil(ctx, registerTimeout, func() error { return createMarker(ctx, ui, o.uiURL) })
	}
	return checkAfterRestore(ctx, ui, o, password)
}

// checkAfterRestore reads the ILM client's secret with the kit's kubeconfig, checks the marker and Keycloak with it
// and checks that no manager log holds it or the credential's password.
func checkAfterRestore(ctx context.Context, ui *http.Client, o options, password string) error {
	passphrase, err := readSecret(o.passphraseFile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(o.kitFile)
	if err != nil {
		return err
	}
	k, err := recovery.Open(data, passphrase)
	if err != nil {
		return fmt.Errorf("open the recovery kit: %w", err)
	}
	cl := forwarded{o.kubeAddr, o.apid}
	step("read the ILM client's secret")
	secret, err := cl.secretValue(ctx, k.Kubeconfig, oidcNamespace, oidcSecret, oidcSecretKey)
	if err != nil {
		return err
	}
	if o.clientSecretOut != "" {
		if err := writeSecret(o.clientSecretOut, []byte(secret)); err != nil {
			return err
		}
	}
	if err := waitUntil(ctx, registerTimeout, func() error { return checkRestored(ctx, ui, o.uiURL, o.hostName, secret) }); err != nil {
		return err
	}
	step("manager logs")
	return cl.checkManager(ctx, k.Kubeconfig, secret, password)
}

// createMarker creates the marker group unless it exists.
func createMarker(ctx context.Context, ui *http.Client, base string) error {
	step("create the marker group")
	groups, err := listGroups(ctx, ui, base)
	if err != nil {
		return err
	}
	if !slices.Contains(groups, markerGroup) {
		body := map[string]string{"name": markerGroup, "description": "created before the backup by the boot test"}
		if _, _, err := ilmRequest(ctx, ui, http.MethodPost, base+groupsPath, "application/json", body); err != nil {
			return err
		}
	}
	fmt.Printf("drive: ILM group %s exists\n", markerGroup)
	return nil
}

// checkRestored checks with the backup's administrator credential that ILM holds the marker group, that the ILM
// realm's token endpoint accepts the ILM client's secret and refuses another one, and that Keycloak takes the
// redirect URI of the ILM host name host.
func checkRestored(ctx context.Context, ui *http.Client, base, host, secret string) error {
	step("marker group")
	groups, err := listGroups(ctx, ui, base)
	if err != nil {
		return err
	}
	if !slices.Contains(groups, markerGroup) {
		return fmt.Errorf("ILM lists %d group(s), none of them %s", len(groups), markerGroup)
	}
	fmt.Printf("drive: the backup's administrator certificate reads ILM group %s\n", markerGroup)
	step("Keycloak client")
	accepted, answer, err := clientSecretAccepted(ctx, ui, base, secret)
	if err != nil {
		return err
	}
	if !accepted {
		return fmt.Errorf("keycloak refuses the ILM client's secret: %s", answer)
	}
	fmt.Printf("drive: keycloak accepts the ILM client's secret (%s)\n", answer)
	accepted, answer, err = clientSecretAccepted(ctx, ui, base, "not-"+secret)
	if err != nil {
		return err
	}
	if accepted {
		return fmt.Errorf("keycloak accepts another secret too (%s), so the check tells nothing", answer)
	}
	fmt.Printf("drive: keycloak refuses another secret (%s)\n", answer)
	return checkRedirect(ctx, ui, base, host)
}

// checkRedirect checks that Keycloak takes the ILM client's redirect URI on the ILM host name host and refuses one on
// another host name.
func checkRedirect(ctx context.Context, ui *http.Client, base, host string) error {
	step("Keycloak redirect URI")
	accepted, status, err := redirectAccepted(ctx, ui, base, "https://"+host+loginCallbackPath)
	if err != nil {
		return err
	}
	if !accepted {
		return fmt.Errorf("keycloak refuses the redirect URI of %s: HTTP %d", host, status)
	}
	fmt.Printf("drive: keycloak accepts the redirect URI of %s\n", host)
	accepted, status, err = redirectAccepted(ctx, ui, base, "https://not-"+host+loginCallbackPath)
	if err != nil {
		return err
	}
	if accepted {
		return fmt.Errorf("keycloak accepts the redirect URI of another host name too (HTTP %d), so the check tells nothing", status)
	}
	fmt.Printf("drive: keycloak refuses the redirect URI of another host name (HTTP %d)\n", status)
	return nil
}

// redirectAccepted asks the authorization endpoint to sign in to the ILM client and come back to redirect, and tells
// whether Keycloak took the redirect URI: it then shows its login page, and else answers that the redirect URI is
// invalid with HTTP 400.
func redirectAccepted(ctx context.Context, ui *http.Client, base, redirect string) (bool, int, error) {
	query := url.Values{"client_id": {ilmClientID}, "response_type": {"code"}, "scope": {"openid"}, "redirect_uri": {redirect}}
	status, _, err := ilmRequest(ctx, ui, http.MethodGet, base+authPath+"?"+query.Encode(), "", nil)
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		err = nil // a refused redirect URI is an answer
	}
	if err != nil {
		return false, 0, err
	}
	return status == http.StatusOK, status, nil
}

// clientSecretAccepted asks the token endpoint for a client_credentials token of the ILM client with secret and
// tells whether Keycloak accepted the secret, with its answer without the token. The ILM client has no service
// account, so an accepted secret is answered with unauthorized_client and a description that says so; a refused one
// with invalid_client, or unauthorized_client for invalid credentials.
func clientSecretAccepted(ctx context.Context, ui *http.Client, base, secret string) (bool, string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {ilmClientID}, "client_secret": {secret}}
	status, data, err := ilmRequest(ctx, ui, http.MethodPost, base+tokenPath, "application/x-www-form-urlencoded", form.Encode())
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		err = nil // a refused grant is an answer
	}
	if err != nil {
		return false, "", err
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return false, "", fmt.Errorf("the token endpoint answered HTTP %d without JSON", status)
	}
	if answer.AccessToken != "" {
		return true, "a token", nil
	}
	described := fmt.Sprintf("HTTP %d %s: %s", status, answer.Error, answer.Description)
	accepted := answer.Error == "unauthorized_client" && strings.Contains(strings.ToLower(answer.Description), noServiceAccount)
	return accepted, described, nil
}

func listGroups(ctx context.Context, ui *http.Client, base string) ([]string, error) {
	_, data, err := ilmRequest(ctx, ui, http.MethodGet, base+groupsPath, "", nil)
	if err != nil {
		return nil, err
	}
	var groups []struct{ Name string }
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("GET %s: decode the answer: %w", groupsPath, err)
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names, nil
}

// ilmRequest sends body, a string as it is or else as JSON, to the ILM and returns the status and the answer. An
// answer that is not 2xx is also an apiError, which never quotes the request.
func ilmRequest(ctx context.Context, ui *http.Client, method, target, contentType string, body any) (int, []byte, error) {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, err
	}
	path := req.URL.Path
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := ui.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, data, &apiError{method: method, path: path, status: res.StatusCode}
	}
	return res.StatusCode, data, nil
}

// secretValue returns the value of key in the Secret with kubeconfig. Its errors never quote the value.
func (f forwarded) secretValue(ctx context.Context, kubeconfig []byte, namespace, name, key string) (string, error) {
	cs, err := f.clientset(kubeconfig)
	if err != nil {
		return "", err
	}
	s, err := cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read secret %s/%s: %w", namespace, name, err)
	}
	value := strings.TrimSpace(string(s.Data[key]))
	if value == "" {
		return "", fmt.Errorf("secret %s/%s holds no %s", namespace, name, key)
	}
	return value, nil
}
