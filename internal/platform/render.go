// Package platform renders the ILM Platform and Connector objects from the Appliance
// and the connector catalogue of the release manifest.
package platform

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// Name and namespace of the Platform; connectors live in the same namespace.
const (
	Name      = "ilm"
	Namespace = "ilm"
)

// The managed database of the Platform, as operator v1.1.0 names it for a Platform
// called ilm with a managed PgBouncer pooler.
const (
	databaseHost   = "ilm-db-pooler"
	databasePort   = 5432
	databaseName   = "ilm"
	databaseSecret = "ilm-db-app"
	usernameKey    = "username"
	passwordKey    = "password"
)

// TLSSecretName is the name, in the platform namespace, of the copy of the TLS Secret the Appliance references.
const TLSSecretName = "appliance-tls"

// CoreAPIURL is the in-cluster base URL of the Core API, /api included, that operator v1.1.0 registers connectors
// with: it renders Core as Service core on port 8080 in the Platform's namespace.
const CoreAPIURL = "http://core." + Namespace + ".svc.cluster.local:8080/api"

// TrustKey is the key of the trust Secret holding the CA bundle, the operator's default
// for trustedCertificates.
const TrustKey = "ca.crt"

// TLSOptionName is the Traefik TLSOption of the edge's client-certificate login; the edge's Ingress selects it.
const TLSOptionName = "ilm-mtls"

// The Secrets holding the CA operator v1.1.0 issues the generated administrator certificate from: the edge's
// internal CA when the edge provisions it, a dedicated administrator CA otherwise.
const (
	EdgeCASecret  = "ca-keypair"
	AdminCASecret = "admin-ca-keypair"
)

// Other Secrets operator v1.1.0 writes for the Platform that a restore carries: the generated administrator
// certificate and password, the ILM client of the managed Keycloak and that Keycloak's administrator.
const (
	AdminCertificateSecret = "admin-certificate-secret"
	AdminPasswordSecret    = "admin-password"
	OIDCClientSecret       = Name + "-oidc-client"
	KeycloakAdminSecret    = Name + "-keycloak-initial-admin"
)

// AdminUser is the user name operator v1.1.0 registers the ILM administrator under.
const AdminUser = "admin"

// AdminCertificateRegisteredAnnotation holds, on AdminCertificateSecret, the fingerprint of the certificate that ILM's
// AdminUser was last found or made to hold: ILM's own registration never replaces the certificate of an existing user.
const AdminCertificateRegisteredAnnotation = "appliance.otilm.com/admin-certificate-registered"

// The Deployment of the ILM operator.
const (
	OperatorNamespace  = "ilm-operator-system"
	OperatorDeployment = "ilm-operator-controller-manager"
)

// SecretNames names the Secrets already written in the ilm namespace; empty means not present.
type SecretNames struct {
	TLS, Trust, PullSecret, Mail string
	// Database and Messaging hold the credentials of the external database and broker.
	Database, Messaging string
	// DatabaseCA holds the PEM CA of the external database under v1alpha1.CAKey, MessagingCA the broker's CA as a
	// PKCS#12 trust store under TrustStoreKey.
	DatabaseCA, MessagingCA string
}

// Where ILM's components find the CAs of an external database and broker: the Secrets are mounted as directories.
const (
	DatabaseCADir      = "/etc/ilm/db-ca"
	BrokerCADir        = "/etc/ilm/broker-ca"
	TrustStoreKey      = "truststore.p12"
	TrustStorePassword = "ilm-broker-trust-store" // NOSONAR: protects a trust store of public CA certificates, not a secret
)

// database is where ILM and the connectors find the platform database, the Secret with its credentials and how they
// connect: tlsMode is the sslmode, empty for the drivers' default, and caSecret the copy of the CA, empty for none.
type database struct {
	host     string
	port     int32
	name     string
	secret   string
	tlsMode  string
	caSecret string
}

// managedDatabase is the database the operator manages.
var managedDatabase = database{host: databaseHost, port: databasePort, name: databaseName, secret: databaseSecret}

// databaseOf is the database ps chooses; the copies of an external one's credentials and CA must exist.
func databaseOf(ps *v1alpha1.PlatformSpec, secrets SecretNames) (database, error) {
	if ps == nil || ps.Database == nil {
		return managedDatabase, nil
	}
	ext := ps.Database
	if secrets.Database == "" {
		return database{}, fmt.Errorf("the external database's credentials have not been copied to namespace %s yet", Namespace)
	}
	db := database{host: ext.Host, port: ext.Port, name: ext.Name, secret: secrets.Database, tlsMode: ext.TLSMode}
	if ext.CA != nil {
		if secrets.DatabaseCA == "" {
			return database{}, fmt.Errorf("the external database's CA has not been copied to namespace %s yet", Namespace)
		}
		db.caSecret = secrets.DatabaseCA
	}
	return db, nil
}

// jdbcURL is the pgjdbc URL of db with its TLS mode and CA file.
func (db database) jdbcURL() string {
	url := fmt.Sprintf("jdbc:postgresql://%s:%d/%s?characterEncoding=UTF-8", db.host, db.port, db.name)
	if db.tlsMode != "" {
		url += "&sslmode=" + db.tlsMode
	}
	if db.caSecret != "" {
		url += "&sslrootcert=" + db.caFile()
	}
	return url
}

func (db database) caFile() string {
	return DatabaseCADir + "/" + v1alpha1.CAKey
}

// caVolumes mounts the CA of db, none without one.
func (db database) caVolumes() []otilm.SecretRef {
	if db.caSecret == "" {
		return nil
	}
	return []otilm.SecretRef{volumeSecret(db.caSecret, DatabaseCADir)}
}

// volumeSecret mounts the Secret name as the directory path.
func volumeSecret(name, path string) otilm.SecretRef {
	return otilm.SecretRef{Name: name, Type: otilm.RefTypeVolume, MountPath: ptr(path)}
}

// Render returns the Platform and, in the order of the Appliance's components, the
// Connectors for a on the node at nodeIP, which the no-proxy list holds; empty when unknown.
func Render(a *v1alpha1.Appliance, m *release.Manifest, secrets SecretNames, nodeIP string) (*otilm.Platform, []otilm.Connector, error) {
	proxy := outboundProxy(a.Spec.Proxy, nodeIP)
	db, err := databaseOf(a.Spec.Platform, secrets)
	if err != nil {
		return nil, nil, err
	}
	p, err := renderPlatform(a, m, secrets, proxy, db)
	if err != nil {
		return nil, nil, err
	}
	var components []string
	if ps := a.Spec.Platform; ps != nil {
		components = ps.ComponentNames()
	}
	connectors := make([]otilm.Connector, 0, len(components))
	for _, name := range components {
		i := slices.IndexFunc(m.Connectors, func(c release.Connector) bool { return c.Name == name })
		if i < 0 {
			return nil, nil, fmt.Errorf("component %s is not in the connector catalogue", name)
		}
		c, err := renderConnector(m.Connectors[i], db, proxy, a.Spec.Mail, secrets)
		if err != nil {
			return nil, nil, err
		}
		connectors = append(connectors, c)
	}
	return p, connectors, nil
}

// outboundProxy returns p with the no-proxy entries the node itself needs for the node at nodeIP, so that ILM never
// reaches the node, the cluster networks or in-cluster services through the proxy, or nil when p sets nothing.
func outboundProxy(p *v1alpha1.ProxySpec, nodeIP string) *v1alpha1.ProxySpec {
	px := nodeconfig.ProxyOf(p)
	if px == nil {
		return nil
	}
	return &v1alpha1.ProxySpec{HTTP: px.HTTP, HTTPS: px.HTTPS, NoProxy: nodeconfig.NoProxy(nodeIP, px.NoProxy)}
}

func renderPlatform(a *v1alpha1.Appliance, m *release.Manifest, secrets SecretNames, proxy *v1alpha1.ProxySpec, db database) (*otilm.Platform, error) {
	version, hostName := m.Platform.ILMVersion, firstSet(a.Spec.HostName(), m.Platform.HostName)
	if ps := a.Spec.Platform; ps != nil {
		version = firstSet(ps.ILMVersion, version)
		hostName = firstSet(ps.HostName, hostName)
	}
	tls, err := edgeTLS(a.Spec.TLS, secrets.TLS)
	if err != nil {
		return nil, err
	}

	p := defaultPlatform(version, hostName)
	p.Spec.Edge.TLS = tls
	if secrets.Trust != "" {
		p.Spec.Common.TrustedCertificates = otilm.TrustedCertificatesSpec{SecretRef: secrets.Trust, CAKey: TrustKey}
	}
	if proxy != nil {
		p.Spec.Common.Proxy = otilm.OutboundProxySpec{Enabled: true, HTTP: proxy.HTTP, HTTPS: proxy.HTTPS, NoProxy: proxy.NoProxy}
	}
	if secrets.PullSecret != "" {
		p.Spec.Common.Image.PullSecrets = []string{secrets.PullSecret}
	}
	if ps := a.Spec.Platform; ps != nil {
		if err := dependencies(&p.Spec, ps, secrets, db); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// dependencies points spec at the external database db, broker and identity provider ps chooses, and switches utils
// on when ps asks for it. The credentials of the broker, and its trust store, must be copied into the platform
// namespace.
func dependencies(spec *otilm.PlatformSpec, ps *v1alpha1.PlatformSpec, secrets SecretNames, db database) error {
	if ps.Database != nil {
		spec.Database = otilm.DatabaseSpec{Mode: modeExternal, Host: db.host, Port: db.port, Name: db.name,
			Credentials: &otilm.CredentialsRef{SecretRef: db.secret}}
		// Core and scheduler read SPRING_DATASOURCE_URL before the operator's JDBC_URL, whose name the operator's
		// apply refuses to see twice.
		if db.tlsMode != "" || db.caSecret != "" {
			addToCoreAndScheduler(spec, otilm.EnvVar{Name: "SPRING_DATASOURCE_URL", Value: db.jdbcURL()}, db.caVolumes())
		}
	}
	if mq := ps.Messaging; mq != nil {
		if secrets.Messaging == "" {
			return fmt.Errorf("the external messaging's credentials have not been copied to namespace %s yet", Namespace)
		}
		spec.Messaging = otilm.MessagingSpec{Mode: modeExternal, BrokerType: brokerType, Host: mq.Host, Port: mq.Port,
			VirtualHost: mq.VirtualHost, Credentials: &otilm.CredentialsRef{SecretRef: secrets.Messaging}}
		if mq.TLS {
			if secrets.MessagingCA == "" {
				return fmt.Errorf("the external messaging's trust store has not been copied to namespace %s yet", Namespace)
			}
			addToCoreAndScheduler(spec, otilm.EnvVar{Name: "BROKER_URL", Value: brokerURL(mq)},
				[]otilm.SecretRef{volumeSecret(secrets.MessagingCA, BrokerCADir)})
		}
	}
	if ps.Identity != nil {
		// The manager writes the OAuth2 provider into ILM; the operator installs no Keycloak.
		spec.Keycloak = &otilm.KeycloakSpec{Mode: modeExternal}
		// The operator allows the password method only with the managed Keycloak.
		spec.RegisterAdmin.Password = nil
	}
	spec.Utils.Enabled = ps.Utils
	return nil
}

// addToCoreAndScheduler gives core and scheduler, the components that connect to the database and the broker, the
// variable v and the Secrets refs.
func addToCoreAndScheduler(spec *otilm.PlatformSpec, v otilm.EnvVar, refs []otilm.SecretRef) {
	for _, c := range []*otilm.ComponentSpec{&spec.Core.ComponentSpec, &spec.Scheduler.ComponentSpec} {
		c.Env = append(c.Env, v)
		c.SecretRefs = append(c.SecretRefs, refs...)
	}
}

// brokerURL is the AMQPS URL of mq with the mounted trust store; the virtual host is added from the operator's
// BROKER_VIRTUAL_HOST. Core logs the URL, so it holds nothing secret.
func brokerURL(mq *v1alpha1.ExternalMessaging) string {
	return "amqps://" + net.JoinHostPort(mq.Host, strconv.Itoa(int(mq.Port))) +
		"?transport.trustStoreLocation=" + BrokerCADir + "/" + TrustStoreKey + "&transport.trustStoreType=PKCS12" +
		"&transport.trustStorePassword=" + TrustStorePassword
}

// The operator's mode of a dependency the appliance does not run, and the broker the appliance's messaging is.
const (
	modeExternal = "external"
	brokerType   = "rabbitmq"
)

// Unstructured returns p for a server-side apply. Without a trust Secret the trustedCertificates field is left out,
// so that the apply gives up the field instead of setting it to null, which the Platform's schema refuses.
func Unstructured(p *otilm.Platform) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	if err != nil {
		return nil, fmt.Errorf("convert the Platform: %w", err)
	}
	u := &unstructured.Unstructured{Object: content}
	if p.Spec.Common.TrustedCertificates == (otilm.TrustedCertificatesSpec{}) {
		unstructured.RemoveNestedField(u.Object, "spec", "common", "trustedCertificates")
	}
	return u, nil
}

// TLSOption returns the Traefik TLSOption that asks for an optional client certificate and verifies it against
// the CA the operator issues the generated administrator certificate of p from.
func TLSOption(p *otilm.Platform) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "TLSOption",
		"metadata":   map[string]any{"name": TLSOptionName, "namespace": p.Namespace},
		"spec": map[string]any{"clientAuth": map[string]any{
			"clientAuthType": "VerifyClientCertIfGiven",
			"secretNames":    []any{adminCA(p.Spec.Edge)},
		}},
	}}
}

// adminCA mirrors the operator: the edge provisions the internal CA when it is enabled with tls.source internal,
// the default.
func adminCA(edge *otilm.EdgeSpec) string {
	if edge != nil && edge.Enabled && (edge.TLS == nil || edge.TLS.Source == "" || edge.TLS.Source == "internal") {
		return EdgeCASecret
	}
	return AdminCASecret
}

// edgeTLS serves the TLS Secret once one exists and the edge's internal certificate before.
func edgeTLS(spec v1alpha1.TLSSpec, secret string) (*otilm.EdgeTLSSpec, error) {
	if secret != "" {
		return &otilm.EdgeTLSSpec{Source: "secret", SecretRef: ptr(secret)}, nil
	}
	if spec.Source == v1alpha1.TLSSourceSecret {
		return nil, fmt.Errorf("tls source is secret but the TLS Secret has not been copied to namespace %s yet (expected %s)", Namespace, TLSSecretName)
	}
	return &otilm.EdgeTLSSpec{Source: "internal"}, nil
}

// defaultPlatform is the Platform of addons/platform.yaml, plus the password login method for the generated
// administrator password: everything managed, a generated administrator certificate and an Ingress edge on Traefik
// with client-certificate login.
func defaultPlatform(version, hostName string) *otilm.Platform {
	return &otilm.Platform{
		TypeMeta:   metav1.TypeMeta{APIVersion: otilm.GroupVersion.String(), Kind: "Platform"},
		ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: Namespace},
		Spec: otilm.PlatformSpec{
			Version: version,
			Common:  otilm.CommonSpec{HostName: hostName},
			Core:    otilm.CoreSpec{ClientCertHeader: "X-Forwarded-Tls-Client-Cert"},
			Database: otilm.DatabaseSpec{
				Mode:      "managed",
				Managed:   &otilm.ManagedDatabaseSpec{Instances: 1, Version: "18", Storage: otilm.StorageSpec{Size: "8Gi"}},
				PgBouncer: &otilm.PgBouncerSpec{Managed: true},
			},
			Messaging: otilm.MessagingSpec{
				Mode:       "managed",
				BrokerType: brokerType,
				Managed:    &otilm.ManagedMessagingSpec{Replicas: 1, Version: "4.3.1", Storage: otilm.StorageSpec{Size: "2Gi"}},
			},
			Keycloak: &otilm.KeycloakSpec{Mode: "managed", Realm: "ilm", Managed: &otilm.ManagedKeycloakSpec{Instances: 1}},
			RegisterAdmin: &otilm.RegisterAdminSpec{
				Enabled:     true,
				Username:    AdminUser,
				Name:        "Platform Administrator",
				LastName:    "Administrator",
				Email:       "admin@ilm.appliance.internal",
				Certificate: &otilm.AdminCertificateSpec{Source: "generated"},
				Password:    &otilm.AdminPasswordSpec{Enabled: true, SecretRef: AdminPasswordSecret},
			},
			NetworkPolicy: &otilm.NetworkPolicySpec{IngressNamespace: "traefik"},
			Edge: &otilm.EdgeSpec{
				Enabled:   true,
				Type:      "ingress",
				ClassName: ptr("traefik"),
				Annotations: map[string]string{
					"traefik.ingress.kubernetes.io/router.entrypoints": "websecure",
					"traefik.ingress.kubernetes.io/router.tls":         "true",
					"traefik.ingress.kubernetes.io/router.tls.options": "ilm-ilm-mtls@kubernetescrd",
					"traefik.ingress.kubernetes.io/router.middlewares": "ilm-clear-client-certificate@kubernetescrd,ilm-forward-client-certificate@kubernetescrd",
				},
			},
			// Talos' default pod network; Traefik and the node reach Kong from it.
			Gateway: otilm.GatewaySpec{TrustedIPs: []string{"10.244.0.0/16"}}, // NOSONAR: a private pod network range, not an address to connect to
		},
	}
}

func renderConnector(c release.Connector, db database, proxy *v1alpha1.ProxySpec, mail *v1alpha1.MailSpec, secrets SecretNames) (otilm.Connector, error) {
	image := otilm.ImageSpec{}
	image.Repository, image.Tag = splitTag(c.Image)
	if c.Private {
		if secrets.PullSecret == "" {
			return otilm.Connector{}, fmt.Errorf("component %s is private and needs registry credentials", c.Name)
		}
		image.PullSecrets = []string{secrets.PullSecret}
		// Private images are not in the offline cache, so pin what is pulled; the operator ignores the tag.
		image.Digest = c.Digest
	}

	env := plainEnv(c.Env)
	var refs []otilm.SecretRef
	switch c.Database {
	case release.DatabaseJDBC:
		env = append(env, otilm.EnvVar{Name: "JDBC_URL", Value: db.jdbcURL()})
		if c.Schema != "" {
			env = append(env, otilm.EnvVar{Name: "DB_SCHEMA", Value: c.Schema})
		}
		refs = append(refs, envSecret(db.secret, usernameKey, "JDBC_USERNAME", passwordKey, "JDBC_PASSWORD"))
	case release.DatabaseEnv:
		env = append(env,
			otilm.EnvVar{Name: "DATABASE_HOST", Value: db.host},
			otilm.EnvVar{Name: "DATABASE_PORT", Value: strconv.Itoa(int(db.port))},
			otilm.EnvVar{Name: "DATABASE_NAME", Value: db.name},
			otilm.EnvVar{Name: "DATABASE_SCHEMA", Value: c.Schema},
		)
		// libpq reads its TLS settings from the environment.
		env = appendSet(env, "PGSSLMODE", db.tlsMode)
		if db.caSecret != "" {
			env = append(env, otilm.EnvVar{Name: "PGSSLROOTCERT", Value: db.caFile()})
		}
		refs = append(refs, envSecret(db.secret, usernameKey, "DATABASE_USER", passwordKey, "DATABASE_PASSWORD"))
	}
	if c.Database != "" {
		refs = append(refs, db.caVolumes()...)
	}
	settingsEnv, settingsRefs := settings(c, proxy, mail, secrets)
	env, refs = append(env, settingsEnv...), append(refs, settingsRefs...)
	if len(c.Keys) > 0 {
		pairs := make([]string, 0, 2*len(c.Keys))
		for _, key := range c.Keys {
			pairs = append(pairs, key, key)
		}
		refs = append(refs, envSecret(c.Name+"-keys", pairs...))
	}

	connector := otilm.Connector{
		TypeMeta:   metav1.TypeMeta{APIVersion: otilm.GroupVersion.String(), Kind: "Connector"},
		ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: Namespace},
		Spec: otilm.ConnectorSpec{
			Image:      image,
			Service:    otilm.ServiceSpec{Port: c.Port},
			Probes:     probes(c.HealthPath),
			Env:        env,
			SecretRefs: refs,
			// The operator mounts the root file system read-only; Java connectors need a writable /tmp to start.
			Volumes: []otilm.VolumeSpec{{Name: "tmp", MountPath: "/tmp"}},
		},
	}
	if !c.V1Only() {
		connector.Spec.Registration = &otilm.RegistrationSpec{
			PlatformURL: CoreAPIURL,
			Name:        c.DisplayName,
			AuthType:    otilm.AuthTypeNone,
		}
	}
	return connector, nil
}

// ConnectorURL is the in-cluster base URL of the Service operator v1.1.0 creates for the connector c, which a
// connector that is not registered automatically is registered with in ILM.
func ConnectorURL(c release.Connector) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", c.Name, Namespace, c.Port)
}

// settings returns the environment and Secret references of the Appliance areas the connector reads: the proxy,
// the trusted CAs and the mail server.
func settings(c release.Connector, proxy *v1alpha1.ProxySpec, mail *v1alpha1.MailSpec, secrets SecretNames) ([]otilm.EnvVar, []otilm.SecretRef) {
	var env []otilm.EnvVar
	var refs []otilm.SecretRef
	if proxy != nil && slices.Contains(c.Settings, release.SettingProxy) {
		env = appendSet(env, "HTTP_PROXY", proxy.HTTP)
		env = appendSet(env, "HTTPS_PROXY", proxy.HTTPS)
		env = appendSet(env, "NO_PROXY", proxy.NoProxy)
	}
	if secrets.Trust != "" && slices.Contains(c.Settings, release.SettingTrust) {
		refs = append(refs, envSecret(secrets.Trust, TrustKey, "TRUSTED_CERTIFICATES"))
	}
	// Without a mail server the connector gets no mail settings: an Appliance that chose it before the API asked for
	// one still reconciles; the API refuses to choose it anew without one.
	if mail != nil && slices.Contains(c.Settings, release.SettingMail) {
		env = append(env, mailEnv(mail, secrets.Mail != "")...)
		if secrets.Mail != "" {
			refs = append(refs, envSecret(secrets.Mail, usernameKey, "SMTP_USERNAME", passwordKey, "SMTP_PASSWORD"))
		}
	}
	return env, refs
}

// mailEnv is the mail server of the email notification provider's Spring Boot mail settings. SMTP_TLS switches
// STARTTLS on; implicit TLS is the JavaMail property mail.smtp.ssl.enable, which Spring Boot binds from
// sslEnableEnv. The sender address is a setting of each notification instance, not of the connector.
func mailEnv(m *v1alpha1.MailSpec, auth bool) []otilm.EnvVar {
	env := []otilm.EnvVar{
		{Name: "SMTP_HOST", Value: m.Host},
		{Name: "SMTP_PORT", Value: strconv.Itoa(int(m.Port))},
		{Name: "SMTP_AUTH", Value: strconv.FormatBool(auth)},
		{Name: "SMTP_TLS", Value: strconv.FormatBool(m.Security == v1alpha1.MailSecuritySTARTTLS)},
	}
	if m.Security == v1alpha1.MailSecurityTLS {
		env = append(env, otilm.EnvVar{Name: sslEnableEnv, Value: "true"})
	}
	return env
}

// sslEnableEnv sets the JavaMail property mail.smtp.ssl.enable through Spring Boot's spring.mail.properties.
const sslEnableEnv = "SPRING_MAIL_PROPERTIES_MAIL_SMTP_SSL_ENABLE"

// probes uses the operator's default timings with the connector's health paths.
func probes(liveness string) *otilm.ProbeSpec {
	readiness := liveness
	if base, ok := strings.CutSuffix(liveness, "/liveness"); ok {
		readiness = base + "/readiness"
	}
	return &otilm.ProbeSpec{
		Liveness:  &otilm.ProbeConfig{Path: liveness, InitialDelaySeconds: 15, PeriodSeconds: 10, FailureThreshold: 3},
		Readiness: &otilm.ProbeConfig{Path: readiness, InitialDelaySeconds: 5, PeriodSeconds: 10, FailureThreshold: 3},
		Startup:   &otilm.ProbeConfig{Path: liveness, PeriodSeconds: 10, FailureThreshold: 45},
	}
}

// envSecret maps Secret keys to environment variables; pairs alternate key and variable.
func envSecret(name string, pairs ...string) otilm.SecretRef {
	ref := otilm.SecretRef{Name: name, Type: otilm.RefTypeEnv}
	for i := 0; i+1 < len(pairs); i += 2 {
		ref.Keys = append(ref.Keys, otilm.RefKeyMapping{SecretKey: pairs[i], EnvVar: ptr(pairs[i+1])})
	}
	return ref
}

// plainEnv returns env sorted by name, so the rendered object is stable.
func plainEnv(env map[string]string) []otilm.EnvVar {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	vars := make([]otilm.EnvVar, 0, len(names))
	for _, name := range names {
		vars = append(vars, otilm.EnvVar{Name: name, Value: env[name]})
	}
	return vars
}

func appendSet(env []otilm.EnvVar, name, value string) []otilm.EnvVar {
	if value == "" {
		return env
	}
	return append(env, otilm.EnvVar{Name: name, Value: value})
}

// splitTag splits an image reference into its repository and tag.
func splitTag(ref string) (repository, tag string) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || i < strings.LastIndex(ref, "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func ptr[T any](v T) *T { return &v }
