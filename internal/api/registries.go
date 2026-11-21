package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	// registriesPath is the path of the registries; each registry is below it.
	registriesPath = auth.APIBase + "/registries"
	// registrySecretPrefix starts the name of a registry's credentials Secret in SystemNamespace.
	registrySecretPrefix = "appliance-registry-"
	// registrySecretHashLength is how many hex digits of the SHA-256 of the host follow the prefix.
	registrySecretHashLength = 12
	// pathRegistriesSpec is the JSON path of the registries, which every change of a registry records.
	pathRegistriesSpec = "spec.registries"

	fieldPassword = "password"
)

var (
	registryNotFound    = NotFoundJSONResponse{Code: codeNotFound, Message: "The registry is not configured."}
	errRegistryNotFound = errors.New("registry not configured")
)

// registryInUseError refuses removing a registry that a chosen private component pulls from.
type registryInUseError struct{ component string }

func (e *registryInUseError) Error() string { return e.component + " pulls from the registry" }

// GetRegistries lists the registries with the user names of their credentials.
func (s *Server) GetRegistries(ctx context.Context, _ GetRegistriesRequestObject) (GetRegistriesResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetRegistries409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]RegistryCredentials, len(a.Spec.Registries))
	for i, r := range a.Spec.Registries {
		if items[i], err = s.registryCredentials(ctx, r); err != nil {
			return nil, err
		}
	}
	return GetRegistries200JSONResponse{Items: items}, nil
}

// GetRegistry describes a registry and the user name of its credentials.
func (s *Server) GetRegistry(ctx context.Context, req GetRegistryRequestObject) (GetRegistryResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetRegistry409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	host, ok := parseRegistryHost(req.Host)
	i := registryIndex(a.Spec.Registries, host)
	if !ok || i < 0 {
		return GetRegistry404JSONResponse{NotFoundJSONResponse: registryNotFound}, nil
	}
	out, err := s.registryCredentials(ctx, a.Spec.Registries[i])
	if err != nil {
		return nil, err
	}
	return GetRegistry200JSONResponse(out), nil
}

// PutRegistry adds a registry or changes its credentials, which go to the registry's Secret in SystemNamespace.
func (s *Server) PutRegistry(ctx context.Context, req PutRegistryRequestObject) (PutRegistryResponseObject, error) {
	host, ferr := parseRegistryUpdate(req.Host, req.Body)
	if ferr != nil {
		return PutRegistry400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutRegistry409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	ferr, err = s.saveRegistry(ctx, a, host, req.Body)
	switch {
	case err != nil:
		return nil, err
	case ferr != nil:
		return PutRegistry400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	return PutRegistry202Response{}, nil
}

// saveRegistry writes the credentials of body for host and references them from the registry of the Appliance. The
// stored password is kept without one in body only for the user name it was stored for.
func (s *Server) saveRegistry(ctx context.Context, a *v1alpha1.Appliance, host string, body *RegistryCredentialsUpdate) (*fieldError, error) {
	name := registrySecretName(host)
	stored, err := s.systemSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	i := registryIndex(a.Spec.Registries, host)
	data, err := credentialsData(stored, body.Username, body.Password, i >= 0 && a.Spec.Registries[i].CredentialsRef == name)
	if errors.Is(err, errPasswordNeeded) || err == nil && len(data[v1alpha1.CredentialsPasswordKey]) == 0 {
		return &fieldError{fieldPassword, "Enter the password: a stored one is kept only for the same user name."}, nil
	}
	if err != nil {
		return nil, err
	}
	credentialsChanged := stored == nil || !maps.EqualFunc(stored.Data, data, func(a, b []byte) bool { return string(a) == string(b) })
	if credentialsChanged {
		if err := s.putSystemSecret(ctx, stored, name, corev1.SecretTypeOpaque, data); err != nil {
			return nil, err
		}
	}
	specChanged, err := s.referenceCredentials(ctx, host, name)
	if err != nil {
		return nil, err
	}
	changed := credentialsChanged || specChanged
	if changed {
		recordPaths(ctx, []string{pathRegistriesSpec})
	}
	s.deps.Log.Info("registry credentials saved", "host", host, "changed", changed)
	return nil, nil
}

// referenceCredentials points the registry host of the Appliance, added when missing, at the credentials Secret name
// and deletes the registry Secret it referenced before. It reports whether the registries changed.
func (s *Server) referenceCredentials(ctx context.Context, host, name string) (bool, error) {
	entry := v1alpha1.RegistrySpec{Host: host, CredentialsRef: name}
	var replaced []string
	changed, err := s.editRegistries(ctx, func(list []v1alpha1.RegistrySpec, _ *v1alpha1.PlatformSpec) ([]v1alpha1.RegistrySpec, error) {
		replaced = nil
		if i := registryIndex(list, host); i >= 0 {
			replaced = append(replaced, list[i].CredentialsRef)
			list[i] = entry
			return list, nil
		}
		return append(list, entry), nil
	})
	if err != nil {
		return false, err
	}
	return changed, s.deleteRegistrySecrets(ctx, slices.DeleteFunc(replaced, func(ref string) bool { return ref == name }))
}

// DeleteRegistry removes a registry from the Appliance and deletes its credentials Secret, unless a chosen private
// component pulls from it.
func (s *Server) DeleteRegistry(ctx context.Context, req DeleteRegistryRequestObject) (DeleteRegistryResponseObject, error) {
	host, ok := parseRegistryHost(req.Host)
	var removed []string
	// The Appliance stops referencing the credentials before they go.
	_, err := s.editRegistries(ctx, func(list []v1alpha1.RegistrySpec, p *v1alpha1.PlatformSpec) ([]v1alpha1.RegistrySpec, error) {
		if !ok || registryIndex(list, host) < 0 {
			return nil, errRegistryNotFound
		}
		if component := s.chosenComponent(p, func(c release.Connector) bool { return c.Private && c.RegistryHost() == host }); component != "" {
			return nil, &registryInUseError{component}
		}
		removed = []string{registrySecretName(host)}
		return slices.DeleteFunc(list, func(r v1alpha1.RegistrySpec) bool {
			if sameRegistry(r, host) {
				removed = append(removed, r.CredentialsRef)
				return true
			}
			return false
		}), nil
	})
	var inUse *registryInUseError
	switch {
	case apierrors.IsNotFound(err):
		return DeleteRegistry409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case errors.Is(err, errRegistryNotFound):
		return DeleteRegistry404JSONResponse{NotFoundJSONResponse: registryNotFound}, nil
	case errors.As(err, &inUse):
		return DeleteRegistry409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "registry_in_use", Message: "The " + inUse.component + " pulls from this registry; remove it from the components first.",
		}}, nil
	case err != nil:
		return nil, err
	}
	if err := s.deleteRegistrySecrets(ctx, removed); err != nil {
		return nil, err
	}
	recordPaths(ctx, []string{pathRegistriesSpec})
	s.deps.Log.Info("registry removed", "host", host)
	return DeleteRegistry202Response{}, nil
}

// editRegistries replaces the registries of the Appliance by edit's result, given a copy of them and the platform,
// and reports whether they changed. An error of edit leaves the Appliance as it is and is returned.
func (s *Server) editRegistries(ctx context.Context, edit func([]v1alpha1.RegistrySpec, *v1alpha1.PlatformSpec) ([]v1alpha1.RegistrySpec, error)) (bool, error) {
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		next, err := edit(slices.Clone(a.Spec.Registries), a.Spec.Platform)
		if err != nil {
			return err
		}
		if changed = !slices.Equal(next, a.Spec.Registries); !changed {
			return nil
		}
		a.Spec.Registries = next
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	return changed, err
}

// deleteRegistrySecrets deletes the Secrets of refs that are registry credentials Secrets; other Secrets were not
// written by the API and stay.
func (s *Server) deleteRegistrySecrets(ctx context.Context, refs []string) error {
	for _, ref := range refs {
		if !strings.HasPrefix(ref, registrySecretPrefix) {
			continue
		}
		if err := s.deleteSystemSecret(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// registryCredentials describes r with the user name of its credentials; the password only as set or not set.
func (s *Server) registryCredentials(ctx context.Context, r v1alpha1.RegistrySpec) (RegistryCredentials, error) {
	out := RegistryCredentials{Host: r.Host}
	if r.CredentialsRef == "" {
		return out, nil
	}
	secret, err := s.systemSecret(ctx, r.CredentialsRef)
	if err != nil || secret == nil {
		return out, err
	}
	out.Username, out.PasswordSet = string(secret.Data[v1alpha1.CredentialsUsernameKey]), len(secret.Data[v1alpha1.CredentialsPasswordKey]) > 0
	return out, nil
}

// parseRegistryUpdate validates the host and the credentials of a registry update and returns the host in its
// canonical form.
func parseRegistryUpdate(rawHost string, body *RegistryCredentialsUpdate) (string, *fieldError) {
	host, ok := parseRegistryHost(rawHost)
	switch {
	case !ok:
		return "", &fieldError{"host", "Enter the registry's host name or IP address with an optional port, such as registry.example.com:5000, without https:// or a path."}
	case body == nil:
		return "", &fieldError{"username", "Enter the registry credentials."}
	case body.Username == "" || !printable(body.Username):
		return "", &fieldError{"username", "Enter a user name of printable characters."}
	case body.Password != nil && len(*body.Password) > maxCredentialLength:
		return "", &fieldError{fieldPassword, "The password is too long."}
	}
	return host, nil
}

// parseRegistryHost returns the canonical form of a registry host: a lowercase host name or an IP address, an IPv6
// address in brackets, with an optional port. Anything else, such as a scheme, a path or a user, is refused.
func parseRegistryHost(raw string) (string, bool) {
	name, port := strings.ToLower(strings.TrimSpace(raw)), ""
	if h, p, err := net.SplitHostPort(name); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		name, port = h, ":"+strconv.Itoa(n)
	}
	if addr, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")); err == nil && addr.Zone() == "" {
		name = addr.String()
		if addr.Is6() {
			name = "[" + name + "]"
		}
		return name + port, true
	}
	if len(validation.IsDNS1123Subdomain(name)) > 0 {
		return "", false
	}
	return name + port, true
}

// registrySecretName is the Secret in SystemNamespace with the credentials of the registry host.
func registrySecretName(host string) string {
	sum := sha256.Sum256([]byte(host))
	return registrySecretPrefix + hex.EncodeToString(sum[:])[:registrySecretHashLength]
}

// registryIndex is the index of the registry host, in canonical form, in list, or -1.
func registryIndex(list []v1alpha1.RegistrySpec, host string) int {
	return slices.IndexFunc(list, func(r v1alpha1.RegistrySpec) bool { return sameRegistry(r, host) })
}

// sameRegistry reports whether r is the registry host, in canonical form; r's host may be written in another form.
func sameRegistry(r v1alpha1.RegistrySpec, host string) bool {
	canonical, ok := parseRegistryHost(r.Host)
	return ok && canonical == host
}
