package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// The Secrets in SystemNamespace that hold the credentials of the external dependencies.
const (
	DatabaseSecretName  = "appliance-database"
	MessagingSecretName = "appliance-messaging"
	IdentitySecretName  = "appliance-identity"
)

const (
	defaultDatabasePort  = 5432
	defaultMessagingPort = 5672
	defaultAMQPSPort     = 5671
	defaultVirtualHost   = "/"
	maxIdentityField     = 255
	maxIssuer            = 2048
	maxScopes            = 32
	maxAudiences         = 16
)

// databaseName are the database names whose characters are safe in a JDBC URL.
var databaseName = regexp.MustCompile(`^[A-Za-z0-9_$.-]{1,63}$`)

// dependencyKinds are the dependencies in the order they are checked and reported.
var dependencyKinds = []string{v1alpha1.DependencyDatabase, v1alpha1.DependencyMessaging, v1alpha1.DependencyIdentity}

// dependencyCredentials are the credentials of an external dependency: a user name, empty for the identity
// provider, a password or client secret, nil to keep the stored one, and the PEM CA, nil to keep the stored one.
type dependencyCredentials struct {
	username string
	secret   *string
	ca       *string
}

// dependenciesInput is a validated choice of dependencies. given lists the kinds the request chooses; the spec holds
// the external ones of them, a kind it leaves nil is managed.
type dependenciesInput struct {
	given       []string
	spec        v1alpha1.PlatformSpec
	credentials map[string]dependencyCredentials
	// settings names the fields of the errors as those of a test's settings instead of a choice of dependencies.
	settings bool
}

func (in *dependenciesInput) chooses(kind string) bool {
	return in != nil && slices.Contains(in.given, kind)
}

// parseDependencies validates the dependencies of a request; nil chooses none.
func parseDependencies(d *DependenciesUpdate) (*dependenciesInput, *fieldError) {
	in := &dependenciesInput{credentials: map[string]dependencyCredentials{}}
	if d == nil {
		return in, nil
	}
	if d.Database != nil {
		in.given = append(in.given, v1alpha1.DependencyDatabase)
		if err := in.parseDatabase(d.Database); err != nil {
			return nil, err
		}
	}
	if d.Messaging != nil {
		in.given = append(in.given, v1alpha1.DependencyMessaging)
		if err := in.parseMessaging(d.Messaging); err != nil {
			return nil, err
		}
	}
	if d.Identity != nil {
		in.given = append(in.given, v1alpha1.DependencyIdentity)
		if err := in.parseIdentity(d.Identity); err != nil {
			return nil, err
		}
	}
	return in, nil
}

func (in *dependenciesInput) field(kind, name string) string {
	if in.settings {
		return "settings." + name
	}
	return "dependencies." + kind + "." + name
}

// mode refuses a mode other than managed and external and tells whether mode is external.
func (in *dependenciesInput) mode(kind string, mode DependencyMode) (bool, *fieldError) {
	if !mode.Valid() {
		return false, &fieldError{in.field(kind, "mode"), "Choose managed or external."}
	}
	return mode == ModeExternal, nil
}

func (in *dependenciesInput) parseDatabase(d *DatabaseDependency) *fieldError {
	const kind = v1alpha1.DependencyDatabase
	external, ferr := in.mode(kind, d.Mode)
	if ferr != nil || !external {
		return ferr
	}
	host, port, ferr := in.server(kind, d.Host, d.Port, defaultDatabasePort)
	if ferr != nil {
		return ferr
	}
	name := strings.TrimSpace(deref(d.Name))
	if !databaseName.MatchString(name) {
		return &fieldError{in.field(kind, "name"), "Enter the database name: at most 63 letters, digits and _ $ . -."}
	}
	creds, ferr := in.userCredentials(kind, d.Username, d.Password)
	if ferr != nil {
		return ferr
	}
	mode := ""
	if d.TlsMode != nil {
		if !d.TlsMode.Valid() {
			return &fieldError{in.field(kind, "tlsMode"), "Choose disable, require or verify-full."}
		}
		mode = string(*d.TlsMode)
	}
	if creds.ca, ferr = in.parseCA(kind, d.Ca); ferr != nil {
		return ferr
	}
	in.spec.Database = &v1alpha1.ExternalDatabase{Host: host, Port: port, Name: name, CredentialsRef: DatabaseSecretName, TLSMode: mode}
	if mode == v1alpha1.DatabaseTLSVerifyFull {
		in.spec.Database.CA = &v1alpha1.SecretKeyRef{Name: DatabaseSecretName, Key: v1alpha1.CAKey}
	}
	in.credentials[kind] = creds
	return nil
}

// parseCA validates a PEM CA bundle; nil or empty keeps the stored one.
func (in *dependenciesInput) parseCA(kind string, ca *string) (*string, *fieldError) {
	if ca == nil || strings.TrimSpace(*ca) == "" {
		return nil, nil
	}
	if len(*ca) > maxSecretLength || !x509.NewCertPool().AppendCertsFromPEM([]byte(*ca)) {
		return nil, &fieldError{in.field(kind, "ca"), "The CA bundle holds no PEM certificate."}
	}
	return ca, nil
}

func (in *dependenciesInput) parseMessaging(m *MessagingDependency) *fieldError {
	const kind = v1alpha1.DependencyMessaging
	external, ferr := in.mode(kind, m.Mode)
	if ferr != nil || !external {
		return ferr
	}
	defaultPort := defaultMessagingPort
	if m.Tls != nil && *m.Tls {
		defaultPort = defaultAMQPSPort
	}
	host, port, ferr := in.server(kind, m.Host, m.Port, defaultPort)
	if ferr != nil {
		return ferr
	}
	vhost := deref(m.VirtualHost)
	if vhost == "" {
		// The operator passes an external broker's virtual host through as it is; RabbitMQ's default is /.
		vhost = defaultVirtualHost
	}
	if len(vhost) > maxIdentityField || !printable(vhost) {
		return &fieldError{in.field(kind, "virtualHost"), "Enter a virtual host of printable characters, at most 255."}
	}
	creds, ferr := in.userCredentials(kind, m.Username, m.Password)
	if ferr != nil {
		return ferr
	}
	if creds.ca, ferr = in.parseCA(kind, m.Ca); ferr != nil {
		return ferr
	}
	in.spec.Messaging = &v1alpha1.ExternalMessaging{Host: host, Port: port, VirtualHost: vhost, CredentialsRef: MessagingSecretName}
	if m.Tls != nil && *m.Tls {
		in.spec.Messaging.TLS = true
		in.spec.Messaging.CA = &v1alpha1.SecretKeyRef{Name: MessagingSecretName, Key: v1alpha1.CAKey}
	}
	in.credentials[kind] = creds
	return nil
}

func (in *dependenciesInput) parseIdentity(i *IdentityDependency) *fieldError {
	const kind = v1alpha1.DependencyIdentity
	external, ferr := in.mode(kind, i.Mode)
	if ferr != nil || !external {
		return ferr
	}
	issuer := strings.TrimSpace(deref(i.Issuer))
	if u, err := url.Parse(issuer); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" ||
		u.User != nil || len(issuer) > maxIssuer {
		return &fieldError{in.field(kind, "issuer"), "Enter the issuer URL, starting with https://."}
	}
	clientID := strings.TrimSpace(deref(i.ClientId))
	if clientID == "" || len(clientID) > maxIdentityField || !printable(clientID) {
		return &fieldError{in.field(kind, "clientId"), "Enter the client ID."}
	}
	scopes, ok := tokens(i.Scopes, maxScopes)
	if !ok {
		return &fieldError{in.field(kind, "scopes"), "Enter at most 32 scopes without spaces."}
	}
	audiences, ok := tokens(i.Audiences, maxAudiences)
	if !ok {
		return &fieldError{in.field(kind, "audiences"), "Enter at most 16 audiences without spaces."}
	}
	if i.ClientSecret != nil && len(*i.ClientSecret) > maxCredentialLength {
		return &fieldError{in.field(kind, "clientSecret"), "The client secret is too long."}
	}
	in.spec.Identity = &v1alpha1.ExternalIdentity{
		Issuer: issuer, ClientID: clientID, Scopes: scopes, Audiences: audiences, CredentialsRef: IdentitySecretName,
	}
	in.credentials[kind] = dependencyCredentials{secret: emptyAsNil(i.ClientSecret)}
	return nil
}

// tokens trims the values, drops empty and repeated ones and tells whether at most limit are left, each printable
// without spaces and not too long.
func tokens(values *[]string, limit int) ([]string, bool) {
	var out []string
	if values != nil {
		for _, v := range *values {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out, len(out) <= limit && !slices.ContainsFunc(out, func(s string) bool {
		return len(s) > maxIdentityField || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
	})
}

// server validates the host and port of an external dependency; a port left out is defaultPort.
func (in *dependenciesInput) server(kind string, host *string, port *int, defaultPort int) (string, int32, *fieldError) {
	h := strings.ToLower(strings.TrimSpace(deref(host)))
	// The appliance's network is IPv4; an IPv6 literal would need brackets in some of the URLs ILM builds.
	if !validHost(h) || strings.Contains(h, ":") {
		return "", 0, &fieldError{in.field(kind, "host"), "Enter the host name or IPv4 address of the server."}
	}
	p := defaultPort
	if port != nil {
		p = *port
	}
	if p < 1 || p > 65535 {
		return "", 0, &fieldError{in.field(kind, "port"), "Enter a port between 1 and 65535."}
	}
	return h, int32(p), nil //nolint:gosec // checked to be a port above
}

// userCredentials validates a user name and a password.
func (in *dependenciesInput) userCredentials(kind string, username, password *string) (dependencyCredentials, *fieldError) {
	user := deref(username)
	switch {
	case user == "" || !printable(user):
		return dependencyCredentials{}, &fieldError{in.field(kind, "username"), "Enter a user name of printable characters."}
	case password != nil && len(*password) > maxCredentialLength:
		return dependencyCredentials{}, &fieldError{in.field(kind, "password"), "The password is too long."}
	}
	return dependencyCredentials{username: user, secret: emptyAsNil(password)}, nil
}

func emptyAsNil(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

func secretField(kind string) string {
	if kind == v1alpha1.DependencyIdentity {
		return "clientSecret"
	}
	return "password"
}

// dependencySecret names the credentials Secret of kind and the key of its password or client secret.
func dependencySecret(kind string) (name, key string) {
	switch kind {
	case v1alpha1.DependencyDatabase:
		return DatabaseSecretName, v1alpha1.CredentialsPasswordKey
	case v1alpha1.DependencyMessaging:
		return MessagingSecretName, v1alpha1.CredentialsPasswordKey
	default:
		return IdentitySecretName, v1alpha1.CredentialsClientSecretKey
	}
}

// serverLocation is where an external dependency is and how it is reached: host, port, database or virtual host and
// TLS setting; issuer and client ID for the identity provider.
type serverLocation struct {
	host string
	port int32
	name string
	tls  string
}

// location is where the external dependency kind of p is, nil when it is managed. Credentials stored for one
// location are not sent to another, nor over another TLS setting.
func location(p *v1alpha1.PlatformSpec, kind string) any {
	if p == nil {
		return nil
	}
	switch kind {
	case v1alpha1.DependencyDatabase:
		if db := p.Database; db != nil {
			return serverLocation{db.Host, db.Port, db.Name, db.TLSMode}
		}
	case v1alpha1.DependencyMessaging:
		if mq := p.Messaging; mq != nil {
			return serverLocation{mq.Host, mq.Port, mq.VirtualHost, strconv.FormatBool(mq.TLS)}
		}
	default:
		if id := p.Identity; id != nil {
			return serverLocation{host: id.Issuer, name: id.ClientID}
		}
	}
	return nil
}

// needsCA reports whether the external dependency kind of p verifies its server against a stored CA.
func needsCA(p *v1alpha1.PlatformSpec, kind string) bool {
	switch kind {
	case v1alpha1.DependencyDatabase:
		return p.Database != nil && p.Database.CA != nil
	case v1alpha1.DependencyMessaging:
		return p.Messaging != nil && p.Messaging.CA != nil
	}
	return false
}

// modeSwitch refuses, once setup is complete, a dependency of in that switches between managed and external.
func (in *dependenciesInput) modeSwitch(current *v1alpha1.PlatformSpec) *fieldError {
	for _, kind := range in.given {
		if (location(current, kind) == nil) != (location(&in.spec, kind) == nil) {
			return &fieldError{in.field(kind, "mode"), fmt.Sprintf("Switching the %s between managed and external moves its data: "+
				"take a backup, set up a fresh appliance with the new choice and restore the backup onto it.", kind)}
		}
	}
	return nil
}

// apply sets the dependencies in chooses on p and keeps those of current it leaves out.
func (in *dependenciesInput) apply(p, current *v1alpha1.PlatformSpec) {
	if current == nil {
		current = &v1alpha1.PlatformSpec{}
	}
	p.Database, p.Messaging, p.Identity = current.Database, current.Messaging, current.Identity
	if in.chooses(v1alpha1.DependencyDatabase) {
		p.Database = in.spec.Database
	}
	if in.chooses(v1alpha1.DependencyMessaging) {
		p.Messaging = in.spec.Messaging
	}
	if in.chooses(v1alpha1.DependencyIdentity) {
		p.Identity = in.spec.Identity
	}
}

// credentialsData is the data of the credentials Secret of the external kind of in: the given password or client
// secret, or the stored one when it was stored for the same server, TLS setting and user name, as current chose it;
// otherwise a field error asks for it. It holds the CA when kind verifies its server: the given one, else the stored
// one. The client secret of the identity provider is stored with the issuer and client ID it is given for.
func (in *dependenciesInput) credentialsData(kind string, stored *corev1.Secret, current *v1alpha1.PlatformSpec) (map[string][]byte, *fieldError) {
	data, ferr := in.caData(kind, stored)
	if ferr != nil {
		return nil, ferr
	}
	if id := in.spec.Identity; kind == v1alpha1.DependencyIdentity {
		data[v1alpha1.CredentialsIssuerKey], data[v1alpha1.CredentialsClientIDKey] = []byte(id.Issuer), []byte(id.ClientID)
	}
	creds := in.credentials[kind]
	_, key := dependencySecret(kind)
	if creds.username != "" {
		data[v1alpha1.CredentialsUsernameKey] = []byte(creds.username)
	}
	if creds.secret != nil {
		data[key] = []byte(*creds.secret)
		return data, nil
	}
	same := stored != nil && len(stored.Data[key]) > 0 && string(stored.Data[v1alpha1.CredentialsUsernameKey]) == creds.username &&
		location(current, kind) == location(&in.spec, kind)
	if !same {
		message := "Enter the password: the server, its TLS setting or the user name changed, or none is stored."
		if kind == v1alpha1.DependencyIdentity {
			message = "Enter the client secret: the issuer or client ID changed, or none is stored."
		}
		return nil, &fieldError{in.field(kind, secretField(kind)), message}
	}
	data[key] = stored.Data[key]
	return data, nil
}

// caData holds the CA of the external kind of in when it verifies its server: the given one, else the stored one,
// which is not secret; without either a field error asks for it.
func (in *dependenciesInput) caData(kind string, stored *corev1.Secret) (map[string][]byte, *fieldError) {
	data := map[string][]byte{}
	if !needsCA(&in.spec, kind) {
		return data, nil
	}
	switch ca := in.credentials[kind].ca; {
	case ca != nil:
		data[v1alpha1.CAKey] = []byte(*ca)
	case stored != nil && len(stored.Data[v1alpha1.CAKey]) > 0:
		data[v1alpha1.CAKey] = stored.Data[v1alpha1.CAKey]
	default:
		return nil, &fieldError{in.field(kind, "ca"), "Upload the CA certificate that signs the server's certificate."}
	}
	return data, nil
}

// credentialsPlan is what writing the credentials Secrets of a choice of dependencies changes, read and checked
// before anything is written: the stored Secret and the data to write, nil to delete it, by kind.
type credentialsPlan struct {
	kinds  []string
	stored map[string]*corev1.Secret
	data   map[string]map[string][]byte
}

// planDependencyCredentials reads the credentials Secret of every dependency in chooses and checks that every
// external one has its secret and CA; current is the platform the stored credentials were given for.
func (s *Server) planDependencyCredentials(ctx context.Context, in *dependenciesInput, current *v1alpha1.PlatformSpec) (*credentialsPlan, *fieldError, error) {
	plan := &credentialsPlan{kinds: in.given, stored: map[string]*corev1.Secret{}, data: map[string]map[string][]byte{}}
	for _, kind := range in.given {
		name, _ := dependencySecret(kind)
		secret, err := s.systemSecret(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		plan.stored[kind] = secret
		if location(&in.spec, kind) == nil {
			continue
		}
		d, ferr := in.credentialsData(kind, secret, current)
		if ferr != nil {
			return nil, ferr, nil
		}
		plan.data[kind] = d
	}
	return plan, nil, nil
}

// writeCredentials writes the credentials Secrets of plan, deleting those of managed dependencies, and returns the
// JSON paths of the credentials that changed.
func (s *Server) writeCredentials(ctx context.Context, plan *credentialsPlan) ([]string, error) {
	var paths []string
	for _, kind := range plan.kinds {
		name, _ := dependencySecret(kind)
		changed, err := s.putDependencySecret(ctx, name, plan.stored[kind], plan.data[kind])
		if err != nil {
			return nil, err
		}
		paths = appendChanged(paths, changed, "spec.platform."+kind+".credentialsRef")
	}
	return paths, nil
}

// undoCredentials puts the credentials Secrets of plan back as they were stored, after what they were written for
// was refused. A failure is logged with its Kubernetes reason only.
func (s *Server) undoCredentials(ctx context.Context, plan *credentialsPlan) {
	ctx = context.WithoutCancel(ctx)
	for _, kind := range plan.kinds {
		name, _ := dependencySecret(kind)
		s.putBack(ctx, name, plan.stored[kind])
	}
}

// putBack writes the credentials Secret name back as stored, or deletes it when none was. A failure is logged with
// its Kubernetes reason only.
func (s *Server) putBack(ctx context.Context, name string, stored *corev1.Secret) {
	current, err := s.systemSecret(ctx, name)
	if err == nil {
		var data map[string][]byte
		if stored != nil {
			data = stored.Data
		}
		_, err = s.putDependencySecret(ctx, name, current, data)
	}
	if err != nil {
		s.deps.Log.Error("could not put back the credentials", "secret", name, "failure", apierrors.ReasonForError(err))
	}
}

// putDependencySecret writes data to the Secret name, or deletes it when data is nil, and tells whether it changed.
func (s *Server) putDependencySecret(ctx context.Context, name string, stored *corev1.Secret, data map[string][]byte) (bool, error) {
	switch {
	case data == nil && stored == nil:
		return false, nil
	case data == nil:
		return true, s.deleteSystemSecret(ctx, name)
	case stored != nil && maps.EqualFunc(stored.Data, data, bytes.Equal):
		return false, nil
	}
	// A copy: stored stays as it was, for undoCredentials.
	secret := stored.DeepCopy()
	if secret == nil {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: name}, Type: corev1.SecretTypeOpaque}
	}
	return true, s.putSystemSecret(ctx, secret, name, corev1.SecretTypeOpaque, data)
}

// dependencyPaths lists the JSON paths of the dependencies, utils and the CBOM repository that changed from before to
// after.
func dependencyPaths(before, after *v1alpha1.PlatformSpec) []string {
	if before == nil {
		before = &v1alpha1.PlatformSpec{}
	}
	var paths []string
	for _, kind := range dependencyKinds {
		paths = appendChanged(paths, !equalDependency(before, after, kind), "spec.platform."+kind)
	}
	paths = appendChanged(paths, before.Utils != after.Utils, "spec.platform.utils")
	return appendChanged(paths, !reflect.DeepEqual(before.CBOM, after.CBOM), "spec.platform.cbom")
}

func equalDependency(a, b *v1alpha1.PlatformSpec, kind string) bool {
	switch kind {
	case v1alpha1.DependencyDatabase:
		return reflect.DeepEqual(a.Database, b.Database)
	case v1alpha1.DependencyMessaging:
		return reflect.DeepEqual(a.Messaging, b.Messaging)
	}
	return reflect.DeepEqual(a.Identity, b.Identity)
}

// dependencyChoices describes the dependencies of p without their credentials.
func dependencyChoices(p *v1alpha1.PlatformSpec) Dependencies {
	out := Dependencies{
		Database: DatabaseDependency{Mode: ModeManaged}, Messaging: MessagingDependency{Mode: ModeManaged},
		Identity: IdentityDependency{Mode: ModeManaged},
	}
	if p == nil {
		return out
	}
	if db := p.Database; db != nil {
		port := int(db.Port)
		out.Database = DatabaseDependency{Mode: ModeExternal, Host: &db.Host, Port: &port, Name: &db.Name}
		if db.TLSMode != "" {
			mode := DatabaseDependencyTlsMode(db.TLSMode)
			out.Database.TlsMode = &mode
		}
	}
	if mq := p.Messaging; mq != nil {
		port := int(mq.Port)
		out.Messaging = MessagingDependency{Mode: ModeExternal, Host: &mq.Host, Port: &port, VirtualHost: &mq.VirtualHost, Tls: &mq.TLS}
	}
	if id := p.Identity; id != nil {
		scopes, audiences := nonNil(id.Scopes), nonNil(id.Audiences)
		out.Identity = IdentityDependency{Mode: ModeExternal, Issuer: &id.Issuer, ClientId: &id.ClientID, Scopes: &scopes, Audiences: &audiences}
	}
	return out
}

// dependenciesOf describes the dependencies of p with the user names of their credentials and whether their
// password or client secret and CA are stored, never their values.
func (s *Server) dependenciesOf(ctx context.Context, p *v1alpha1.PlatformSpec) (Dependencies, error) {
	out := dependencyChoices(p)
	// stored reads the credentials of kind: the user name, whether the password or client secret is set, and
	// whether a CA is.
	stored := func(kind string) (string, *bool, *bool, error) {
		name, key := dependencySecret(kind)
		secret, err := s.systemSecret(ctx, name)
		if err != nil {
			return "", nil, nil, err
		}
		set, ca := secret != nil && len(secret.Data[key]) > 0, secret != nil && len(secret.Data[v1alpha1.CAKey]) > 0
		if secret == nil {
			return "", &set, &ca, nil
		}
		return string(secret.Data[v1alpha1.CredentialsUsernameKey]), &set, &ca, nil
	}
	var err error
	if out.Database.Mode == ModeExternal {
		var user string
		if user, out.Database.PasswordSet, out.Database.CaSet, err = stored(v1alpha1.DependencyDatabase); err != nil {
			return out, err
		}
		out.Database.Username = optional(user)
	}
	if out.Messaging.Mode == ModeExternal {
		var user string
		if user, out.Messaging.PasswordSet, out.Messaging.CaSet, err = stored(v1alpha1.DependencyMessaging); err != nil {
			return out, err
		}
		out.Messaging.Username = optional(user)
	}
	if out.Identity.Mode == ModeExternal {
		if _, out.Identity.ClientSecretSet, _, err = stored(v1alpha1.DependencyIdentity); err != nil {
			return out, err
		}
	}
	return out, nil
}
