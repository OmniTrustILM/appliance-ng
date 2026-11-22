package api

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/auth"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	// platformPath is the path of the platform settings.
	platformPath = auth.APIBase + "/platform"

	fieldComponents = "components"
)

// GetPlatform describes the platform and every component of the catalogue: whether it is chosen and whether it
// still needs registry credentials or the mail server.
func (s *Server) GetPlatform(ctx context.Context, _ GetPlatformRequestObject) (GetPlatformResponseObject, error) {
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return GetPlatform409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	out := PlatformState{IlmVersion: installedVersion(a.Spec.Platform, s.deps.Release)}
	var chosen []string
	if p := a.Spec.Platform; p != nil {
		out.HostName, chosen = p.HostName, p.Components
	}
	out.Components = make([]PlatformComponent, len(s.deps.Release.Connectors))
	for i, c := range s.deps.Release.Connectors {
		out.Components[i] = PlatformComponent{
			Name:             c.Name,
			Title:            c.DisplayName,
			Private:          c.Private,
			Chosen:           slices.Contains(chosen, c.Name),
			NeedsCredentials: c.Private && !hasCredentials(a.Spec.Registries, c),
			NeedsMail:        sendsMail(c) && a.Spec.Mail == nil,
		}
	}
	return GetPlatform200JSONResponse(out), nil
}

// PutPlatform replaces the platform host name and its components. The ILM version stays the installed one.
func (s *Server) PutPlatform(ctx context.Context, req PutPlatformRequestObject) (PutPlatformResponseObject, error) {
	if req.Body == nil {
		return PutPlatform400JSONResponse{BadRequestJSONResponse: BadRequestJSONResponse{Code: "invalid", Message: "Enter the platform settings."}}, nil
	}
	platform, ferr := parsePlatform(req.Body.HostName, req.Body.Components, s.deps.Release)
	if ferr != nil {
		return PutPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	paths, ferr, err := s.updatePlatform(ctx, platform, req.Body.IlmVersion)
	switch {
	case apierrors.IsNotFound(err):
		return PutPlatform409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case err != nil:
		return nil, err
	case ferr != nil:
		return PutPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("platform settings saved", "hostName", platform.HostName, "components", platform.Components, "paths", paths)
	return PutPlatform202Response{}, nil
}

// updatePlatform sets the host name and components of platform on the Appliance and returns the changed paths. It
// refuses a version other than the installed one, and components that lack what they need on the Appliance as it
// is written, and a host name the uploaded certificate in use does not cover.
func (s *Server) updatePlatform(ctx context.Context, platform v1alpha1.PlatformSpec, version *string) ([]string, *fieldError, error) {
	var paths []string
	var ferr *fieldError
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		platform.ILMVersion = installedVersion(a.Spec.Platform, s.deps.Release)
		if version != nil && *version != platform.ILMVersion {
			ferr = &fieldError{"ilmVersion", fmt.Sprintf("The ILM version is %s and cannot be changed.", platform.ILMVersion)}
			return nil
		}
		if ferr = s.componentsNeed(platform.Components, a.Spec.Registries, a.Spec.Mail != nil); ferr != nil {
			return nil
		}
		if paths = platformPaths(a.Spec.Platform, &platform); len(paths) == 0 {
			return nil
		}
		hosts := certs.Hosts(a)
		a.Spec.Platform = &platform
		host, err := s.uncoveredHost(ctx, a, hosts)
		if err != nil {
			return err
		}
		if host != "" {
			ferr = uncoveredHostError(host)
			return nil
		}
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	return paths, ferr, err
}

// componentsNeed refuses the first of components, all in the catalogue, that needs credentials for its registry
// that registries lack, or the mail server when mail is not set.
func (s *Server) componentsNeed(components []string, registries []v1alpha1.RegistrySpec, mail bool) *fieldError {
	for _, name := range components {
		c, _ := catalogueEntry(s.deps.Release, name)
		switch {
		case c.Private && !hasCredentials(registries, c):
			return &fieldError{fieldComponents, fmt.Sprintf("The component %q needs credentials for the registry %s.", name, c.RegistryHost())}
		case sendsMail(c) && !mail:
			return &fieldError{fieldComponents, fmt.Sprintf("The component %q sends mail and needs the mail server settings.", name)}
		}
	}
	return nil
}

// installedVersion is the ILM version of p, or the one rel ships before the platform is set.
func installedVersion(p *v1alpha1.PlatformSpec, rel *release.Manifest) string {
	if p != nil && p.ILMVersion != "" {
		return p.ILMVersion
	}
	return rel.Platform.ILMVersion
}

// catalogueEntry returns the connector name of the catalogue of rel.
func catalogueEntry(rel *release.Manifest, name string) (release.Connector, bool) {
	i := slices.IndexFunc(rel.Connectors, func(c release.Connector) bool { return c.Name == name })
	if i < 0 {
		return release.Connector{}, false
	}
	return rel.Connectors[i], true
}

// hasCredentials reports whether one of registries has credentials for the registry of c's image.
func hasCredentials(registries []v1alpha1.RegistrySpec, c release.Connector) bool {
	i := registryIndex(registries, c.RegistryHost())
	return i >= 0 && registries[i].CredentialsRef != ""
}

// sendsMail reports whether c reads the mail server settings.
func sendsMail(c release.Connector) bool {
	return slices.Contains(c.Settings, release.SettingMail)
}

// chosenComponent returns the display name of the first chosen component of p that match accepts, or "" when none
// does.
func (s *Server) chosenComponent(p *v1alpha1.PlatformSpec, match func(release.Connector) bool) string {
	if p == nil {
		return ""
	}
	for _, name := range p.Components {
		if c, ok := catalogueEntry(s.deps.Release, name); ok && match(c) {
			return c.DisplayName
		}
	}
	return ""
}
