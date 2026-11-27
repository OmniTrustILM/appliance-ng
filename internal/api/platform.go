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
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
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
	out := PlatformState{IlmVersion: installedVersion(a.Spec.Platform, s.deps.Release), SetupComplete: a.Status.Setup == v1alpha1.SetupComplete}
	var chosen []string
	if p := a.Spec.Platform; p != nil {
		out.HostName, chosen, out.Utils = p.HostName, p.ComponentNames(), p.Utils
	}
	if out.Dependencies, err = s.dependenciesOf(ctx, a.Spec.Platform); err != nil {
		return nil, err
	}
	if out.Cbom, err = s.cbomOf(ctx, a.Spec.Platform); err != nil {
		return nil, err
	}
	out.Components = make([]PlatformComponent, len(s.deps.Release.Connectors))
	for i, c := range s.deps.Release.Connectors {
		out.Components[i] = PlatformComponent{
			Name:             c.Name,
			Title:            c.DisplayName,
			Registered:       true,
			Private:          c.Private,
			Chosen:           slices.Contains(chosen, c.Name),
			NeedsCredentials: c.Private && !hasCredentials(a.Spec.Registries, c),
			NeedsMail:        sendsMail(c) && a.Spec.Mail == nil,
		}
		if c.V1Only() {
			url := platform.ConnectorURL(c)
			out.Components[i].Url = &url
		}
		if out.Components[i].Chosen {
			out.Components[i].State, out.Components[i].Problem = connectorState(a.Status.Connectors, c.Name)
		}
	}
	return GetPlatform200JSONResponse(out), nil
}

// connectorState returns the state and the problem the manager reports for the connector name in states; nil for
// those it does not report.
func connectorState(states []v1alpha1.ConnectorState, name string) (*PlatformComponentState, *string) {
	i := slices.IndexFunc(states, func(c v1alpha1.ConnectorState) bool { return c.Name == name })
	if i < 0 {
		return nil, nil
	}
	state := PlatformComponentState(states[i].State)
	if states[i].Problem == "" {
		return &state, nil
	}
	return &state, &states[i].Problem
}

// PutPlatform replaces the platform host name and its components, and the dependencies and utils the request
// chooses. The ILM version stays the installed one.
func (s *Server) PutPlatform(ctx context.Context, req PutPlatformRequestObject) (PutPlatformResponseObject, error) {
	if req.Body == nil {
		return PutPlatform400JSONResponse{BadRequestJSONResponse: BadRequestJSONResponse{Code: "invalid", Message: "Enter the platform settings."}}, nil
	}
	spec, ferr := parsePlatform(req.Body.HostName, req.Body.Components, s.deps.Release)
	var deps *dependenciesInput
	var cbom *cbomInput
	if ferr == nil {
		deps, ferr = parseDependencies(req.Body.Dependencies)
	}
	if ferr == nil {
		cbom, ferr = parseCBOM(req.Body.Cbom)
	}
	if ferr != nil {
		return PutPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	paths, ferr, err := s.updatePlatform(ctx, spec, req.Body.IlmVersion, deps, req.Body.Utils, cbom)
	switch {
	case apierrors.IsNotFound(err):
		return PutPlatform409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	case err != nil:
		return nil, err
	case ferr != nil:
		return PutPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("platform settings saved", "hostName", spec.HostName, "components", spec.ComponentNames(), "paths", paths)
	return PutPlatform202Response{}, nil
}

// updatePlatform sets the host name and components of platform, the dependencies deps chooses with their
// credentials, utils when it is set and the CBOM repository cbom chooses with its credentials on the Appliance and
// returns the changed paths. It refuses a version other than the installed one, components that lack what they need
// on the Appliance as it is written, a host name the uploaded certificate in use does not cover, and once setup is
// complete a dependency that switches between managed and external.
func (s *Server) updatePlatform(ctx context.Context, platform v1alpha1.PlatformSpec, version *string, deps *dependenciesInput, utils *bool, cbom *cbomInput) ([]string, *fieldError, error) {
	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()
	var paths []string
	var ferr *fieldError
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var err error
		paths, ferr, err = s.tryUpdatePlatform(ctx, platform, version, deps, utils, cbom)
		return err
	})
	return paths, ferr, err
}

// tryUpdatePlatform is one attempt of updatePlatform on the Appliance as it is read now. The credentials it wrote are
// put back when the update fails, so that a retry finds them as they were stored.
func (s *Server) tryUpdatePlatform(ctx context.Context, platform v1alpha1.PlatformSpec, version *string, deps *dependenciesInput, utils *bool, cbom *cbomInput) ([]string, *fieldError, error) {
	a, err := s.appliance(ctx)
	if err != nil {
		return nil, nil, err
	}
	before := a.Spec.Platform
	if ferr := s.platformRefusal(a, &platform, version, deps); ferr != nil {
		return nil, ferr, nil
	}
	mergePlatform(&platform, before, deps, utils)
	hosts := certs.Hosts(a)
	a.Spec.Platform = &platform
	host, err := s.uncoveredHost(ctx, a, hosts)
	if err != nil || host != "" {
		return nil, uncoveredHostRefusal(host), err
	}
	plans, ferr, err := s.planPlatformCredentials(ctx, &platform, before, deps, cbom)
	if ferr != nil || err != nil {
		return nil, ferr, err
	}
	credentials, err := s.writePlatformCredentials(ctx, plans)
	if err != nil {
		s.undoPlatformCredentials(ctx, plans)
		return nil, nil, err
	}
	paths := append(allPlatformPaths(before, &platform), credentials...)
	if len(paths) == 0 {
		return nil, nil, nil
	}
	slices.Sort(paths)
	if err := s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager)); err != nil {
		s.undoPlatformCredentials(ctx, plans)
		return nil, nil, err
	}
	return paths, nil, nil
}

// platformRefusal sets the installed ILM version on platform and refuses another version, components that lack what
// they need on a, and once setup is complete a dependency that switches between managed and external.
func (s *Server) platformRefusal(a *v1alpha1.Appliance, platform *v1alpha1.PlatformSpec, version *string, deps *dependenciesInput) *fieldError {
	platform.ILMVersion = installedVersion(a.Spec.Platform, s.deps.Release)
	if version != nil && *version != platform.ILMVersion {
		return &fieldError{"ilmVersion", fmt.Sprintf("The ILM version is %s and cannot be changed.", platform.ILMVersion)}
	}
	if ferr := s.componentsNeed(platform.ComponentNames(), a.Spec.Registries, a.Spec.Mail != nil); ferr != nil {
		return ferr
	}
	if a.Status.Setup == v1alpha1.SetupComplete {
		return deps.modeSwitch(a.Spec.Platform)
	}
	return nil
}

// uncoveredHostRefusal refuses host, which the uploaded certificate in use does not cover; nil for none.
func uncoveredHostRefusal(host string) *fieldError {
	if host == "" {
		return nil
	}
	return uncoveredHostError(host)
}

// mergePlatform completes platform with what before keeps: the dependencies deps leaves out, utils unless it is
// set, and the CBOM repository.
func mergePlatform(platform, before *v1alpha1.PlatformSpec, deps *dependenciesInput, utils *bool) {
	deps.apply(platform, before)
	platform.Utils = before != nil && before.Utils
	if utils != nil {
		platform.Utils = *utils
	}
	platform.CBOM = nil
	if before != nil {
		platform.CBOM = before.CBOM
	}
}

// platformCredentials are the credentials Secrets an update of the platform writes: those of the dependencies and,
// when the CBOM repository is chosen, of its external store.
type platformCredentials struct {
	dependencies *credentialsPlan
	cbom         *cbomCredentials
}

// planPlatformCredentials checks and plans the credentials of an update of platform from before, with the CBOM
// repository cbom chooses set on platform; nothing is written.
func (s *Server) planPlatformCredentials(ctx context.Context, platform, before *v1alpha1.PlatformSpec, deps *dependenciesInput, cbom *cbomInput) (platformCredentials, *fieldError, error) {
	var plans platformCredentials
	if cbom != nil {
		c, ferr, err := s.planCBOMCredentials(ctx, cbom, platform.CBOM)
		if ferr != nil || err != nil {
			return plans, ferr, err
		}
		plans.cbom = c
		platform.CBOM = &cbom.spec
	}
	d, ferr, err := s.planDependencyCredentials(ctx, deps, before)
	plans.dependencies = d
	return plans, ferr, err
}

// writePlatformCredentials writes the credentials of plans and returns the JSON paths of those that changed.
func (s *Server) writePlatformCredentials(ctx context.Context, plans platformCredentials) ([]string, error) {
	paths, err := s.writeCredentials(ctx, plans.dependencies)
	if err != nil || plans.cbom == nil {
		return paths, err
	}
	changed, err := s.putDependencySecret(ctx, CBOMSecretName, plans.cbom.stored, plans.cbom.data)
	return appendChanged(paths, changed, "spec.platform.cbom.credentialsRef"), err
}

// undoPlatformCredentials puts the credentials of plans back as they were stored.
func (s *Server) undoPlatformCredentials(ctx context.Context, plans platformCredentials) {
	s.undoCredentials(ctx, plans.dependencies)
	if plans.cbom != nil {
		s.putBack(context.WithoutCancel(ctx), CBOMSecretName, plans.cbom.stored)
	}
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
	for _, name := range p.ComponentNames() {
		if c, ok := catalogueEntry(s.deps.Release, name); ok && match(c) {
			return c.DisplayName
		}
	}
	return ""
}
