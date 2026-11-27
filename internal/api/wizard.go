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
	MailUsernameKey = v1alpha1.CredentialsUsernameKey
	MailPasswordKey = v1alpha1.CredentialsPasswordKey

	maxCredentialLength = 1024

	fieldMailUsername = "mail.username"
)

var (
	errNotSetUp      = errors.New("the Appliance does not exist yet")
	errSetupComplete = errors.New("setup is complete")
	errRestoring     = errors.New("a restore is pending")
	// errPasswordNeeded refuses to keep a stored password for another server or user name.
	errPasswordNeeded = errors.New("the stored password belongs to another server or user")
)

// codeRestorePending refuses what a restore from a backup would overwrite.
const (
	codeRestorePending    = "restore_pending"
	messageRestorePending = "The appliance is waiting for a backup to restore; restore it, or set up a new appliance."
)

func restorePending() ConflictJSONResponse {
	return ConflictJSONResponse{Code: codeRestorePending, Message: messageRestorePending}
}

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
	platform, ferr := parsePlatform(req.Body.HostName, req.Body.Components, s.deps.Release)
	var deps *dependenciesInput
	if ferr == nil {
		deps, ferr = parseDependencies(req.Body.Dependencies)
	}
	if ferr == nil {
		var m *mailInput
		if m, ferr = parseMail(req.Body.Mail); ferr == nil {
			return s.savePlatform(ctx, platform, m, deps, req.Body.Utils)
		}
	}
	return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
}

func (s *Server) savePlatform(ctx context.Context, platform v1alpha1.PlatformSpec, m *mailInput, deps *dependenciesInput, utils *bool) (PutWizardPlatformResponseObject, error) {
	s.credentialsMu.Lock()
	defer s.credentialsMu.Unlock()
	a, err := s.openAppliance(ctx)
	if err != nil {
		return wizardClosed(err)
	}
	if ferr := s.componentsNeed(platform.ComponentNames(), a.Spec.Registries, m != nil); ferr != nil {
		return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	// Everything is checked before a Secret is written: the mail credentials refuse before they write.
	plan, ferr, err := s.planDependencyCredentials(ctx, deps, a.Spec.Platform)
	if ferr != nil {
		return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	if err != nil {
		return nil, err
	}
	storedMail, err := s.systemSecret(ctx, MailSecretName)
	if err != nil {
		return nil, err
	}
	undo := func() {
		s.undoCredentials(ctx, plan)
		s.putBack(context.WithoutCancel(ctx), MailSecretName, storedMail)
	}
	credentialsChanged, err := s.writeMailCredentials(ctx, m, a.Spec.Mail)
	if errors.Is(err, errPasswordNeeded) {
		return PutWizardPlatform400JSONResponse{BadRequestJSONResponse: badRequestField("mail.password",
			"Enter the password again: the mail server or user name changed.")}, nil
	}
	if err != nil {
		return nil, err
	}
	credentialPaths, err := s.writeCredentials(ctx, plan)
	if err != nil {
		undo()
		return nil, err
	}
	var paths []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.openAppliance(ctx)
		if err != nil {
			return err
		}
		before := a.Spec.DeepCopy()
		mergePlatform(&platform, before.Platform, deps, utils)
		a.Spec.Platform = &platform
		a.Spec.Mail = nil
		if m != nil {
			a.Spec.Mail = &m.spec
		}
		paths = changedPaths(before, &a.Spec, credentialsChanged)
		paths = append(paths, credentialPaths...)
		slices.Sort(paths)
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	if err != nil {
		undo()
		return wizardClosed(err)
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("platform settings saved", "hostName", platform.HostName, "components", platform.ComponentNames(), "mail", m != nil, "paths", paths)
	return PutWizardPlatform202Response{}, nil
}

// openAppliance reads the Appliance and fails with errNotSetUp, errSetupComplete or errRestoring when part 2 is not open.
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
	case a.Status.Setup == v1alpha1.SetupRestore:
		return nil, errRestoring
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
	case errors.Is(err, errRestoring):
		return PutWizardPlatform409JSONResponse{ConflictJSONResponse: restorePending()}, nil
	case errors.Is(err, errNotSetUp):
		return PutWizardPlatform409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	return nil, err
}

// parsePlatform validates the host name and the components of a request; the ILM version is the one rel ships.
func parsePlatform(hostName string, components []string, rel *release.Manifest) (v1alpha1.PlatformSpec, *fieldError) {
	host := strings.ToLower(strings.TrimSpace(hostName))
	if !hostname.IsFQDN(host) {
		return v1alpha1.PlatformSpec{}, &fieldError{"hostName", "Enter a fully qualified host name, such as ilm.example.com, not an IP address."}
	}
	if components == nil {
		return v1alpha1.PlatformSpec{}, &fieldError{fieldComponents, "Choose the components to install."}
	}
	chosen := make([]v1alpha1.ComponentSpec, 0, len(components))
	for _, name := range components {
		if _, ok := catalogueEntry(rel, name); !ok {
			return v1alpha1.PlatformSpec{}, &fieldError{fieldComponents, fmt.Sprintf("The component %q is not in the catalogue.", name)}
		}
		if !slices.ContainsFunc(chosen, func(c v1alpha1.ComponentSpec) bool { return c.Name == name }) {
			chosen = append(chosen, v1alpha1.ComponentSpec{Name: name})
		}
	}
	return v1alpha1.PlatformSpec{ILMVersion: rel.Platform.ILMVersion, HostName: host, Components: chosen}, nil
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
	if !plainAddress(m.From) {
		return nil, &fieldError{"mail.from", "Enter the sender address, such as ilm@example.com."}
	}
	in := &mailInput{
		spec:     v1alpha1.MailSpec{Host: host, Port: int32(m.Port), Security: string(m.Security), From: m.From}, //nolint:gosec // the port is checked above
		password: m.Password,
	}
	if m.Username != nil {
		in.username = *m.Username
	}
	if ferr := mailCredentialProblem(in); ferr != nil {
		return nil, ferr
	}
	if in.username != "" {
		in.spec.CredentialsRef = MailSecretName
	}
	return in, nil
}

// mailCredentialProblem refuses the user name and password of in, or is nil when they are fine.
func mailCredentialProblem(in *mailInput) *fieldError {
	switch {
	case !printable(in.username):
		return &fieldError{fieldMailUsername, "Enter a user name of printable characters."}
	case in.password != nil && len(*in.password) > maxCredentialLength:
		return &fieldError{"mail.password", "The password is too long."}
	case in.username == "" && in.password != nil && *in.password != "":
		return &fieldError{fieldMailUsername, "Enter the user name the password belongs to."}
	case in.username != "" && in.spec.Security == v1alpha1.MailSecurityNone:
		// Neither the test mail nor the platform sends a password over an unencrypted connection.
		return &fieldError{fieldMailUsername, "Credentials need security starttls or tls; choose one, or remove the user name."}
	}
	return nil
}

// plainAddress reports whether s is a mail address alone, without a display name.
func plainAddress(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Name == "" && addr.Address == s
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
		return s.deleteMailCredentials(ctx, secret, exists)
	}

	data, err := mailSecretData(secret, m, previous)
	if err != nil {
		return false, err
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

// deleteMailCredentials deletes the credentials Secret when it exists and reports whether it did.
func (s *Server) deleteMailCredentials(ctx context.Context, secret *corev1.Secret, exists bool) (bool, error) {
	if !exists {
		return false, nil
	}
	if err := s.deps.Client.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("delete secret %s/%s: %w", SystemNamespace, MailSecretName, err)
	}
	return true, nil
}

// mailSecretData returns the data of the credentials Secret for m, keeping the stored password when m has none and
// it was stored for the same user name and mail server.
func mailSecretData(secret *corev1.Secret, m *mailInput, previous *v1alpha1.MailSpec) (map[string][]byte, error) {
	return credentialsData(secret, m.username, m.password, sameServer(previous, &m.spec))
}

// credentialsData returns the data of a credentials Secret for username and password. Without a password the one in
// stored, which may be nil, is kept, but only when it was stored for username and sameServer says that the
// credentials are for the server they were stored for; otherwise it fails with errPasswordNeeded.
func credentialsData(stored *corev1.Secret, username string, password *string, sameServer bool) (map[string][]byte, error) {
	data := map[string][]byte{MailUsernameKey: []byte(username)}
	if password != nil {
		data[MailPasswordKey] = []byte(*password)
	} else if stored != nil && len(stored.Data[MailPasswordKey]) > 0 {
		if string(stored.Data[MailUsernameKey]) != username || !sameServer {
			return nil, errPasswordNeeded
		}
		data[MailPasswordKey] = stored.Data[MailPasswordKey]
	}
	return data, nil
}

// sameServer reports whether a and b name the same mail server, reached the same way.
func sameServer(a, b *v1alpha1.MailSpec) bool {
	return a != nil && a.Host == b.Host && a.Port == b.Port && a.Security == b.Security
}

// pathMailCredentials is the JSON path of the mail credentials reference.
const pathMailCredentials = "spec.mail.credentialsRef"

// pathMailSpec is the JSON path of the mail server.
const pathMailSpec = "spec.mail"

// changedPaths lists the JSON paths the wizard changed from before to after, sorted; credentials says the
// credentials Secret changed.
func changedPaths(before, after *v1alpha1.ApplianceSpec, credentials bool) []string {
	paths := append(allPlatformPaths(before.Platform, after.Platform), mailPaths(before.Mail, after.Mail, credentials)...)
	slices.Sort(paths)
	return paths
}

// allPlatformPaths lists the JSON paths of the platform, its dependencies included, that changed from before to
// after, which is set.
func allPlatformPaths(before, after *v1alpha1.PlatformSpec) []string {
	if before == nil {
		return []string{"spec.platform"}
	}
	return append(platformPaths(before, after), dependencyPaths(before, after)...)
}

// platformPaths lists the JSON paths of the platform that changed from before to after, which is set, sorted.
func platformPaths(before, after *v1alpha1.PlatformSpec) []string {
	if before == nil {
		return []string{"spec.platform"}
	}
	var paths []string
	paths = appendChanged(paths, !slices.Equal(before.Components, after.Components), "spec.platform.components")
	paths = appendChanged(paths, before.HostName != after.HostName, "spec.platform.hostName")
	paths = appendChanged(paths, before.ILMVersion != after.ILMVersion, "spec.platform.ilmVersion")
	return paths
}

// mailPaths lists the JSON paths of the mail server that changed from before to after, sorted; credentials says the
// credentials Secret changed.
func mailPaths(before, after *v1alpha1.MailSpec, credentials bool) []string {
	switch {
	case after == nil && before == nil:
		return nil
	case after == nil:
		return []string{pathMailSpec}
	case before == nil:
		return appendChanged([]string{pathMailSpec}, credentials, pathMailCredentials)
	}
	var paths []string
	paths = appendChanged(paths, before.CredentialsRef != after.CredentialsRef || credentials, pathMailCredentials)
	paths = appendChanged(paths, before.From != after.From, "spec.mail.from")
	paths = appendChanged(paths, before.Host != after.Host, "spec.mail.host")
	paths = appendChanged(paths, before.Port != after.Port, "spec.mail.port")
	paths = appendChanged(paths, before.Security != after.Security, "spec.mail.security")
	return paths
}

func appendChanged(paths []string, changed bool, path string) []string {
	if changed {
		return append(paths, path)
	}
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
