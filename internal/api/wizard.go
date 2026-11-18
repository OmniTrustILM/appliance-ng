package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/netip"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/hostname"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// The Secret in SystemNamespace that holds the SMTP credentials spec.mail.credentialsRef names.
const (
	MailSecretName  = "appliance-mail"
	MailUsernameKey = "username"
	MailPasswordKey = "password"

	maxCredentialLength = 1024
)

var (
	errNotSetUp      = errors.New("the Appliance does not exist yet")
	errSetupComplete = errors.New("setup is complete")
	// errPasswordNeeded refuses to keep a stored SMTP password for another mail server or user name.
	errPasswordNeeded = errors.New("the stored mail password belongs to another server or user")
)

// fieldError is a request field that failed validation.
type fieldError struct{ field, message string }

// mailInput is the validated mail settings of a request; username empty means no credentials.
type mailInput struct {
	spec     v1alpha1.MailSpec
	username string
	password *string
}

// PutWizardPlatform sets the platform and the mail server until setup is complete. The ILM version is the one
// the release ships, and the SMTP credentials go to a Secret that spec.mail references.
func (s *Server) PutWizardPlatform(ctx context.Context, req PutWizardPlatformRequestObject) (PutWizardPlatformResponseObject, error) {
	if req.Body == nil {
		return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: BadRequestJSONResponse{Code: "invalid", Message: "Enter the platform settings."}}, nil
	}
	platform, ferr := parsePlatform(*req.Body, s.deps.Release)
	if ferr == nil {
		var m *mailInput
		if m, ferr = parseMail(req.Body.Mail); ferr == nil {
			return s.savePlatform(ctx, platform, m)
		}
	}
	return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
}

func (s *Server) savePlatform(ctx context.Context, platform v1alpha1.PlatformSpec, m *mailInput) (PutWizardPlatformResponseObject, error) {
	a, err := s.openAppliance(ctx)
	if err != nil {
		return wizardClosed(err)
	}
	credentialsChanged, err := s.writeMailCredentials(ctx, m, a.Spec.Mail)
	if errors.Is(err, errPasswordNeeded) {
		return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField("mail.password",
			"Enter the password again: the mail server or user name changed.")}, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.openAppliance(ctx)
		if err != nil {
			return err
		}
		before := a.Spec.DeepCopy()
		a.Spec.Platform = &platform
		a.Spec.Mail = nil
		if m != nil {
			a.Spec.Mail = &m.spec
		}
		paths = changedPaths(before, &a.Spec, credentialsChanged)
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	if err != nil {
		return wizardClosed(err)
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("platform settings saved", "hostName", platform.HostName, "components", platform.Components, "mail", m != nil, "paths", paths)
	return PutWizardPlatform202Response{}, nil
}

// openAppliance reads the Appliance and fails with errNotSetUp or errSetupComplete when part 2 is not open.
func (s *Server) openAppliance(ctx context.Context) (*v1alpha1.Appliance, error) {
	a := &v1alpha1.Appliance{}
	err := s.deps.Client.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a)
	switch {
	case apierrors.IsNotFound(err):
		return nil, errNotSetUp
	case err != nil:
		return nil, fmt.Errorf("read the Appliance: %w", err)
	case a.Status.Setup == v1alpha1.SetupComplete:
		return nil, errSetupComplete
	}
	return a, nil
}

// wizardClosed answers 409 when part 2 is not open and fails otherwise.
func wizardClosed(err error) (PutWizardPlatformResponseObject, error) {
	switch {
	case errors.Is(err, errSetupComplete):
		return PutWizardPlatform409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "setup_complete", Message: "Setup is complete; change the settings in the console.",
		}}, nil
	case errors.Is(err, errNotSetUp):
		return PutWizardPlatform409JSONResponse{ConflictJSONResponse: ConflictJSONResponse{
			Code: "not_set_up", Message: "The appliance has not finished the first part of setup yet.",
		}}, nil
	}
	return nil, err
}

// parsePlatform validates the host name and components of body.
func parsePlatform(body PlatformSettings, rel *release.Manifest) (v1alpha1.PlatformSpec, *fieldError) {
	host := strings.ToLower(strings.TrimSpace(body.HostName))
	if !hostname.IsFQDN(host) {
		return v1alpha1.PlatformSpec{}, &fieldError{"hostName", "Enter a fully qualified host name, such as ilm.example.com, not an IP address."}
	}
	if body.Components == nil {
		return v1alpha1.PlatformSpec{}, &fieldError{"components", "Choose the components to install."}
	}
	components := make([]string, 0, len(body.Components))
	for _, name := range body.Components {
		i := slices.IndexFunc(rel.Connectors, func(c release.Connector) bool { return c.Name == name })
		switch {
		case i < 0:
			return v1alpha1.PlatformSpec{}, &fieldError{"components", fmt.Sprintf("The component %q is not in the catalogue.", name)}
		case rel.Connectors[i].Private:
			return v1alpha1.PlatformSpec{}, &fieldError{"components", fmt.Sprintf(
				"The component %q needs registry credentials, which this version of the appliance does not support yet.", name)}
		case !slices.Contains(components, name):
			components = append(components, name)
		}
	}
	return v1alpha1.PlatformSpec{ILMVersion: rel.Platform.ILMVersion, HostName: host, Components: components}, nil
}

// parseMail validates the mail settings; nil means no mail server.
func parseMail(m *Mail) (*mailInput, *fieldError) {
	if m == nil {
		return nil, nil
	}
	host := strings.ToLower(strings.TrimSpace(m.Host))
	if _, err := netip.ParseAddr(host); err != nil && len(validation.IsDNS1123Subdomain(host)) > 0 {
		return nil, &fieldError{"mail.host", "Enter the host name or IP address of the mail server."}
	}
	if m.Port < 1 || m.Port > 65535 {
		return nil, &fieldError{"mail.port", "Enter a port between 1 and 65535."}
	}
	if !m.Security.Valid() {
		return nil, &fieldError{"mail.security", "Choose none, starttls or tls."}
	}
	if addr, err := mail.ParseAddress(m.From); err != nil || addr.Name != "" || addr.Address != m.From {
		return nil, &fieldError{"mail.from", "Enter the sender address, such as ilm@example.com."}
	}
	in := &mailInput{
		spec:     v1alpha1.MailSpec{Host: host, Port: int32(m.Port), Security: string(m.Security), From: m.From}, //nolint:gosec // the port is checked above
		password: m.Password,
	}
	if m.Username != nil {
		in.username = *m.Username
	}
	if !printable(in.username) {
		return nil, &fieldError{"mail.username", "Enter a user name of printable characters."}
	}
	if in.password != nil && len(*in.password) > maxCredentialLength {
		return nil, &fieldError{"mail.password", "The password is too long."}
	}
	if in.username == "" && in.password != nil && *in.password != "" {
		return nil, &fieldError{"mail.username", "Enter the user name the password belongs to."}
	}
	if in.username != "" {
		in.spec.CredentialsRef = MailSecretName
	}
	return in, nil
}

func printable(s string) bool {
	return len(s) <= maxCredentialLength && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) })
}

// writeMailCredentials writes the SMTP credentials of m to MailSecretName, or deletes the Secret when m has no user
// name. It reports whether the Secret changed. When m has no password the stored one is kept, but only for the user
// name and the mail server of previous that it was given for, so that it is never sent elsewhere; otherwise it
// fails with errPasswordNeeded.
func (s *Server) writeMailCredentials(ctx context.Context, m *mailInput, previous *v1alpha1.MailSpec) (bool, error) {
	key := client.ObjectKey{Namespace: SystemNamespace, Name: MailSecretName}
	secret := &corev1.Secret{}
	// Read past the cache, which may not have a Secret written a moment ago yet.
	err := s.deps.Reader.Get(ctx, key, secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("read secret %s/%s: %w", SystemNamespace, MailSecretName, err)
	}
	if m == nil || m.username == "" {
		if !exists {
			return false, nil
		}
		if err := s.deps.Client.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("delete secret %s/%s: %w", SystemNamespace, MailSecretName, err)
		}
		return true, nil
	}

	data := map[string][]byte{MailUsernameKey: []byte(m.username)}
	if m.password != nil {
		data[MailPasswordKey] = []byte(*m.password)
	} else if old, ok := secret.Data[MailPasswordKey]; ok && len(old) > 0 {
		if string(secret.Data[MailUsernameKey]) != m.username || !sameServer(previous, &m.spec) {
			return false, errPasswordNeeded
		}
		data[MailPasswordKey] = old
	}
	if !exists {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: MailSecretName}, Type: corev1.SecretTypeOpaque, Data: data}
		if err := s.deps.Client.Create(ctx, secret, client.FieldOwner(FieldManager)); err != nil {
			return false, fmt.Errorf("create secret %s/%s: %w", SystemNamespace, MailSecretName, err)
		}
		return true, nil
	}
	if maps.EqualFunc(secret.Data, data, func(a, b []byte) bool { return string(a) == string(b) }) {
		return false, nil
	}
	secret.Data = data
	if err := s.deps.Client.Update(ctx, secret, client.FieldOwner(FieldManager)); err != nil {
		return false, fmt.Errorf("update secret %s/%s: %w", SystemNamespace, MailSecretName, err)
	}
	return true, nil
}

// sameServer reports whether a and b name the same mail server, reached the same way.
func sameServer(a, b *v1alpha1.MailSpec) bool {
	return a != nil && a.Host == b.Host && a.Port == b.Port && a.Security == b.Security
}

// changedPaths lists the JSON paths the wizard changed from before to after, sorted; credentials says the
// credentials Secret changed.
func changedPaths(before, after *v1alpha1.ApplianceSpec, credentials bool) []string {
	var paths []string
	add := func(changed bool, path string) {
		if changed {
			paths = append(paths, path)
		}
	}
	if b, a := before.Platform, after.Platform; b == nil {
		paths = append(paths, "spec.platform")
	} else {
		add(b.ILMVersion != a.ILMVersion, "spec.platform.ilmVersion")
		add(b.HostName != a.HostName, "spec.platform.hostName")
		add(!slices.Equal(b.Components, a.Components), "spec.platform.components")
	}
	switch b, a := before.Mail, after.Mail; {
	case b == nil && a == nil:
	case b == nil || a == nil:
		paths = append(paths, "spec.mail")
	default:
		add(b.Host != a.Host, "spec.mail.host")
		add(b.Port != a.Port, "spec.mail.port")
		add(b.Security != a.Security, "spec.mail.security")
		add(b.From != a.From, "spec.mail.from")
		add(b.CredentialsRef != a.CredentialsRef, "spec.mail.credentialsRef")
	}
	if credentials && after.Mail != nil && !slices.Contains(paths, "spec.mail.credentialsRef") {
		paths = append(paths, "spec.mail.credentialsRef")
	}
	slices.Sort(paths)
	return paths
}

// GetCatalogue returns the components the release ships.
func (s *Server) GetCatalogue(context.Context, GetCatalogueRequestObject) (GetCatalogueResponseObject, error) {
	items := make([]CatalogueItem, len(s.deps.Release.Connectors))
	for i, c := range s.deps.Release.Connectors {
		items[i] = CatalogueItem{Name: c.Name, Title: c.DisplayName, Private: c.Private}
	}
	return GetCatalogue200JSONResponse{Items: items}, nil
}
