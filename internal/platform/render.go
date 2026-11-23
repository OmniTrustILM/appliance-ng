// Package platform renders the ILM Platform and Connector objects from the Appliance
// and the connector catalogue of the release manifest.
package platform

import (
	"fmt"
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

// SecretNames names the Secrets already written in the ilm namespace; empty means not present.
type SecretNames struct {
	TLS, Trust, PullSecret, Mail string
}

// Render returns the Platform and, in the order of the Appliance's components, the
// Connectors for a on the node at nodeIP, which the no-proxy list holds; empty when unknown.
func Render(a *v1alpha1.Appliance, m *release.Manifest, secrets SecretNames, nodeIP string) (*otilm.Platform, []otilm.Connector, error) {
	proxy := outboundProxy(a.Spec.Proxy, nodeIP)
	p, err := renderPlatform(a, m, secrets, proxy)
	if err != nil {
		return nil, nil, err
	}
	var components []string
	if a.Spec.Platform != nil {
		components = a.Spec.Platform.Components
	}
	connectors := make([]otilm.Connector, 0, len(components))
	for _, name := range components {
		i := slices.IndexFunc(m.Connectors, func(c release.Connector) bool { return c.Name == name })
		if i < 0 {
			return nil, nil, fmt.Errorf("component %s is not in the connector catalogue", name)
		}
		c, err := renderConnector(m.Connectors[i], proxy, a.Spec.Mail, secrets)
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

func renderPlatform(a *v1alpha1.Appliance, m *release.Manifest, secrets SecretNames, proxy *v1alpha1.ProxySpec) (*otilm.Platform, error) {
	version, hostName := m.Platform.ILMVersion, firstSet(a.Spec.Node.HostName, m.Platform.HostName)
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
	return p, nil
}

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

// defaultPlatform is the Platform of addons/platform.yaml: everything managed, a generated
// administrator certificate and an Ingress edge on Traefik with client-certificate login.
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
				BrokerType: "rabbitmq",
				Managed:    &otilm.ManagedMessagingSpec{Replicas: 1, Version: "4.3.1", Storage: otilm.StorageSpec{Size: "2Gi"}},
			},
			Keycloak: &otilm.KeycloakSpec{Mode: "managed", Realm: "ilm", Managed: &otilm.ManagedKeycloakSpec{Instances: 1}},
			RegisterAdmin: &otilm.RegisterAdminSpec{
				Enabled:     true,
				Username:    "admin",
				Name:        "Platform Administrator",
				LastName:    "Administrator",
				Email:       "admin@ilm.appliance.internal",
				Certificate: &otilm.AdminCertificateSpec{Source: "generated"},
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

func renderConnector(c release.Connector, proxy *v1alpha1.ProxySpec, mail *v1alpha1.MailSpec, secrets SecretNames) (otilm.Connector, error) {
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
		env = append(env, otilm.EnvVar{
			Name:  "JDBC_URL",
			Value: fmt.Sprintf("jdbc:postgresql://%s:%d/%s?characterEncoding=UTF-8", databaseHost, databasePort, databaseName),
		})
		if c.Schema != "" {
			env = append(env, otilm.EnvVar{Name: "DB_SCHEMA", Value: c.Schema})
		}
		refs = append(refs, envSecret(databaseSecret, usernameKey, "JDBC_USERNAME", passwordKey, "JDBC_PASSWORD"))
	case release.DatabaseEnv:
		env = append(env,
			otilm.EnvVar{Name: "DATABASE_HOST", Value: databaseHost},
			otilm.EnvVar{Name: "DATABASE_PORT", Value: strconv.Itoa(databasePort)},
			otilm.EnvVar{Name: "DATABASE_NAME", Value: databaseName},
			otilm.EnvVar{Name: "DATABASE_SCHEMA", Value: c.Schema},
		)
		refs = append(refs, envSecret(databaseSecret, usernameKey, "DATABASE_USER", passwordKey, "DATABASE_PASSWORD"))
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

	return otilm.Connector{
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
			Registration: &otilm.RegistrationSpec{
				PlatformURL: CoreAPIURL,
				Name:        c.DisplayName,
				AuthType:    otilm.AuthTypeNone,
			},
		},
	}, nil
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
