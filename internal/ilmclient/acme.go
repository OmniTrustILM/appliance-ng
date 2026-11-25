package ilmclient

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
)

const (
	raProfilesPath   = "/api/v1/raProfiles"
	vaultProfilesAll = "/api/v1/vaultProfiles/list"
	acmeProfilesPath = "/api/v1/acmeProfiles"
	// vaultProfilesPage is the page size of the vault profile list, more than an appliance's ILM holds.
	vaultProfilesPage = 1000
)

// RAProfile is an RA profile of ILM.
type RAProfile struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// VaultProfile is an enabled vault profile of ILM and the vault it belongs to.
type VaultProfile struct {
	UUID, Name, VaultUUID string
}

// PreauthorizedIdentifier is an identifier an ACME profile issues for without a challenge.
type PreauthorizedIdentifier struct {
	// Type is dns or ip.
	Type  string `json:"type"`
	Value string `json:"value"`
	// MatchType is exact or subdomain.
	MatchType string `json:"matchType"`
}

// ACMEProfile is an ACME profile that issues through RAProfileUUID, only for Identifiers and only to accounts bound
// with one of the EAB secrets.
type ACMEProfile struct {
	Name           string
	RAProfileUUID  string
	EABSecretUUIDs []string
	Identifiers    []PreauthorizedIdentifier
}

// ListRAProfiles returns the RA profiles of ILM.
func (c *Client) ListRAProfiles(ctx context.Context) ([]RAProfile, error) {
	var profiles []RAProfile
	if err := c.read(ctx, http.MethodGet, raProfilesPath, nil, &profiles); err != nil {
		return nil, err
	}
	return profiles, nil
}

// ListVaultProfiles returns the enabled vault profiles of ILM.
func (c *Client) ListVaultProfiles(ctx context.Context) ([]VaultProfile, error) {
	var page struct {
		Items []struct {
			UUID          string `json:"uuid"`
			Name          string `json:"name"`
			Enabled       bool   `json:"enabled"`
			VaultInstance struct {
				UUID string `json:"uuid"`
			} `json:"vaultInstance"`
		} `json:"items"`
	}
	search := map[string]int{"itemsPerPage": vaultProfilesPage, "pageNumber": 1}
	if err := c.read(ctx, http.MethodPost, vaultProfilesAll, search, &page); err != nil {
		return nil, err
	}
	var profiles []VaultProfile
	for _, p := range page.Items {
		if p.Enabled {
			profiles = append(profiles, VaultProfile{UUID: p.UUID, Name: p.Name, VaultUUID: p.VaultInstance.UUID})
		}
	}
	return profiles, nil
}

// GenerateEABKey asks ILM for a new HMAC key of an ACME external account binding, base64url-encoded. ILM does not
// keep it; CreateSecretKey stores it.
func (c *Client) GenerateEABKey(ctx context.Context) (string, error) {
	var generated struct {
		Key string `json:"key"`
	}
	if err := c.read(ctx, http.MethodPost, acmeProfilesPath+"/eabKeys", nil, &generated); err != nil {
		return "", err
	}
	return generated.Key, nil
}

// CreateSecretKey stores key as the secret key name in the vault profile and returns the secret's UUID.
func (c *Client) CreateSecretKey(ctx context.Context, vaultUUID, vaultProfileUUID, name, key string) (string, error) {
	path := "/api/v1/vaults/" + url.PathEscape(vaultUUID) + "/vaultProfiles/" + url.PathEscape(vaultProfileUUID) + "/secrets"
	body := struct {
		Name   string `json:"name"`
		Secret struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"secret"`
	}{Name: name}
	body.Secret.Type, body.Secret.Content = "secretKey", key
	var created struct {
		UUID string `json:"uuid"`
	}
	if err := c.read(ctx, http.MethodPost, path, body, &created); err != nil {
		return "", err
	}
	return created.UUID, nil
}

// SecretExists reports whether ILM still holds the secret uuid; ILM answers 404 for one that is gone.
func (c *Client) SecretExists(ctx context.Context, uuid string) (bool, error) {
	_, err := c.call(ctx, http.MethodGet, "/api/v1/secrets/"+url.PathEscape(uuid), nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// acmeProfileRequest is the body that creates an ACME profile, without the name the one that edits it. An edit
// replaces the profile's settings, so it sends all of them.
type acmeProfileRequest struct {
	Name                        string                    `json:"name,omitempty"`
	RAProfileUUID               string                    `json:"raProfileUuid"`
	IssueCertificateAttributes  []any                     `json:"issueCertificateAttributes"`
	RevokeCertificateAttributes []any                     `json:"revokeCertificateAttributes"`
	RequireContact              bool                      `json:"requireContact"`
	RequireTermsOfService       bool                      `json:"requireTermsOfService"`
	EABSecretUUIDs              []string                  `json:"eabSecretUuids"`
	Identifiers                 []PreauthorizedIdentifier `json:"preauthorizedIdentifiers"`
	AuthorizationMode           string                    `json:"identifierAuthorizationMode"`
}

// acmeProfileItem is an ACME profile as ILM lists it.
type acmeProfileItem struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// acmeProfileDetail is what ILM answers about an ACME profile, as far as EnsureACMEProfile sets it.
type acmeProfileDetail struct {
	RAProfile *struct {
		UUID string `json:"uuid"`
	} `json:"raProfile"`
	EABSecretUUIDs    []string                  `json:"eabSecretUuids"`
	Identifiers       []PreauthorizedIdentifier `json:"preauthorizedIdentifiers"`
	AuthorizationMode string                    `json:"identifierAuthorizationMode"`
}

// EnsureACMEProfile creates the ACME profile p.Name, or updates it when its RA profile, EAB secrets or identifiers
// differ from p, enables it and returns its UUID. The profile issues only for p's identifiers.
func (c *Client) EnsureACMEProfile(ctx context.Context, p ACMEProfile) (string, error) {
	want := acmeProfileRequest{
		RAProfileUUID: p.RAProfileUUID, IssueCertificateAttributes: []any{}, RevokeCertificateAttributes: []any{},
		EABSecretUUIDs: slices.Sorted(slices.Values(p.EABSecretUUIDs)), Identifiers: sortedIdentifiers(p.Identifiers),
		AuthorizationMode: "preauthorizedOnly",
	}
	var listed []acmeProfileItem
	if err := c.read(ctx, http.MethodGet, acmeProfilesPath, nil, &listed); err != nil {
		return "", err
	}
	var uuid string
	enabled := false
	if i := slices.IndexFunc(listed, func(l acmeProfileItem) bool { return l.Name == p.Name }); i >= 0 {
		uuid, enabled = listed[i].UUID, listed[i].Enabled
		if err := c.updateACMEProfile(ctx, uuid, want); err != nil {
			return "", err
		}
	} else {
		create := want
		create.Name = p.Name
		var created struct {
			UUID string `json:"uuid"`
		}
		if err := c.read(ctx, http.MethodPost, acmeProfilesPath, create, &created); err != nil {
			return "", err
		}
		uuid = created.UUID
	}
	if !enabled {
		if _, err := c.call(ctx, http.MethodPatch, acmeProfilesPath+"/"+url.PathEscape(uuid)+"/enable", nil); err != nil {
			return "", err
		}
	}
	return uuid, nil
}

// updateACMEProfile writes want to the profile uuid unless it already holds it.
func (c *Client) updateACMEProfile(ctx context.Context, uuid string, want acmeProfileRequest) error {
	path := acmeProfilesPath + "/" + url.PathEscape(uuid)
	var current acmeProfileDetail
	if err := c.read(ctx, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	if current.RAProfile != nil && current.RAProfile.UUID == want.RAProfileUUID &&
		current.AuthorizationMode == want.AuthorizationMode &&
		slices.Equal(slices.Sorted(slices.Values(current.EABSecretUUIDs)), want.EABSecretUUIDs) &&
		slices.Equal(sortedIdentifiers(current.Identifiers), want.Identifiers) {
		return nil
	}
	_, err := c.call(ctx, http.MethodPut, path, want)
	return err
}

func sortedIdentifiers(ids []PreauthorizedIdentifier) []PreauthorizedIdentifier {
	return slices.SortedFunc(slices.Values(ids), func(a, b PreauthorizedIdentifier) int {
		return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.Value, b.Value), cmp.Compare(a.MatchType, b.MatchType))
	})
}

// read calls path and decodes the answer into out.
func (c *Client) read(ctx context.Context, method, path string, body, out any) error {
	answer, err := c.call(ctx, method, path, body)
	if err != nil {
		return err
	}
	return decode(method+" "+path, answer, out)
}
