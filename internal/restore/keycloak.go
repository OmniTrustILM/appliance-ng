package restore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/keycloak"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// logoutURIsAttribute holds a Keycloak client's post-logout redirect URIs, logoutURIsSeparator apart.
const (
	logoutURIsAttribute = "post.logout.redirect.uris"
	logoutURIsSeparator = "##"
)

// The fields of a Keycloak client that hold URLs, which may name the ILM host.
var clientURLFields = []string{"rootUrl", "baseUrl", "adminUrl", "redirectUris", "webOrigins"}

// rehostKeycloak moves the URLs of the Keycloak clients of ILM from the backup's ILM host name to this appliance's.
// The realm comes back with the database, and the operator imports its own realm only once, so without this the
// clients keep the backup's host name and Keycloak refuses sign-in under the new one. Client secrets stay as they are.
// An external identity provider is not the appliance's to change.
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
	if from == to || a.Spec.Platform != nil && a.Spec.Platform.Identity != nil {
		return nil
	}
	admin, err := e.liveSecret(ctx, platform.Namespace, platform.KeycloakAdminSecret)
	if err != nil {
		return err
	}
	if admin == nil {
		return fmt.Errorf("secret %s/%s not found", platform.Namespace, platform.KeycloakAdminSecret)
	}
	base, err := keycloak.URL(ctx, e.deps.Client, e.deps.KeycloakURL)
	if err != nil {
		return err
	}
	kc := keycloak.Admin{Base: base, Client: e.deps.HTTPClient, User: string(admin.Data["username"]), Password: string(admin.Data["password"])}
	return e.waitFor(ctx, "Keycloak", func(ctx context.Context) (bool, error) {
		moved, err := rehost(ctx, kc, from, to)
		if errors.Is(err, keycloak.ErrUnavailable) {
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
	return spec.HostName()
}

// rehost moves the URLs of every client of the ILM realm from the host from to the host to and returns how many
// clients changed. A client whose URLs do not name from is left alone, so rehost can run again.
func rehost(ctx context.Context, k keycloak.Admin, from, to string) (int, error) {
	token, err := k.Token(ctx)
	if err != nil {
		return 0, err
	}
	clientsPath := "/admin/realms/" + keycloak.Realm + "/clients"
	var clients []map[string]any
	if err := k.Call(ctx, http.MethodGet, clientsPath, token, nil, &clients); err != nil {
		return 0, err
	}
	moved := 0
	for _, c := range clients {
		if !rehostClient(c, from, to) {
			continue
		}
		id, _ := c["id"].(string)
		if err := k.Call(ctx, http.MethodPut, clientsPath+"/"+url.PathEscape(id), token, c, nil); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
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
