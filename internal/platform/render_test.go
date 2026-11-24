package platform

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func loadManifest(t *testing.T) *release.Manifest {
	t.Helper()
	m, err := release.Load("../../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func defaultAppliance() *v1alpha1.Appliance {
	return &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "ilm.appliance.internal"},
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
		Platform: &v1alpha1.PlatformSpec{ILMVersion: "2.20.0", HostName: "ilm.appliance.internal"},
	}}
}

func TestRenderDefaultMatchesAddonsPlatform(t *testing.T) {
	data, err := os.ReadFile("../../addons/platform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var want otilm.Platform
	if err := yaml.UnmarshalStrict(data, &want); err != nil {
		t.Fatal(err)
	}

	got, connectors, err := Render(defaultAppliance(), loadManifest(t), SecretNames{}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Spec, want.Spec) {
		t.Fatalf("spec differs\n got: %+v\nwant: %+v", got.Spec, want.Spec)
	}
	if got.Name != "ilm" || got.Namespace != "ilm" || got.Kind != "Platform" || got.APIVersion != "otilm.com/v1alpha1" {
		t.Errorf("unexpected object identity %v %s/%s", got.TypeMeta, got.Namespace, got.Name)
	}
	if len(connectors) != 0 {
		t.Errorf("want no connectors, got %d", len(connectors))
	}
}

func TestRenderFallsBackToNodeAndManifestDefaults(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform = nil
	a.Spec.Node.HostName = "ilm.example.com"

	got, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Version != "2.20.0" || got.Spec.Common.HostName != "ilm.example.com" {
		t.Fatalf("version %q host %q", got.Spec.Version, got.Spec.Common.HostName)
	}
}

func TestRenderAppliesApplianceSettings(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.HostName = "pki.example.com"
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTP: "http://proxy:3128", HTTPS: "http://proxy:3128", NoProxy: ".svc"}
	secrets := SecretNames{TLS: "ilm-tls", Trust: "ilm-trust", PullSecret: "ilm-registry"}

	got, _, err := Render(a, loadManifest(t), secrets, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	spec := got.Spec
	if spec.Common.HostName != "pki.example.com" {
		t.Errorf("host name %q", spec.Common.HostName)
	}
	if tls := spec.Edge.TLS; tls.Source != "secret" || tls.SecretRef == nil || *tls.SecretRef != "ilm-tls" {
		t.Errorf("edge tls %+v", tls)
	}
	if tc := spec.Common.TrustedCertificates; tc.SecretRef != "ilm-trust" || tc.CAKey != "ca.crt" {
		t.Errorf("trusted certificates %+v", tc)
	}
	want := otilm.OutboundProxySpec{Enabled: true, HTTP: "http://proxy:3128", HTTPS: "http://proxy:3128", NoProxy: internalNoProxy + ",.svc"}
	if spec.Common.Proxy != want {
		t.Errorf("proxy %+v", spec.Common.Proxy)
	}
	if !reflect.DeepEqual(spec.Common.Image.PullSecrets, []string{"ilm-registry"}) {
		t.Errorf("pull secrets %v", spec.Common.Image.PullSecrets)
	}
}

func TestRenderEmptyProxyLeavesPlatformProxyUnset(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Proxy = &v1alpha1.ProxySpec{}

	got, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Common.Proxy != (otilm.OutboundProxySpec{}) {
		t.Errorf("proxy %+v", got.Spec.Common.Proxy)
	}
}

func TestRenderSecretTLSNeedsSecret(t *testing.T) {
	a := defaultAppliance()
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "uploaded"}

	if _, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP); err == nil {
		t.Fatal("want an error when the TLS Secret is missing")
	}
}

func TestRenderComponents(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"x509-compliance-provider", "ejbca-ng-connector"}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	if len(connectors) != 2 {
		t.Fatalf("want 2 connectors, got %d", len(connectors))
	}
	x509, ejbca := connectors[0], connectors[1]
	if x509.Name != "x509-compliance-provider" || x509.Namespace != "ilm" || x509.Kind != "Connector" {
		t.Errorf("unexpected identity %v %s/%s", x509.TypeMeta, x509.Namespace, x509.Name)
	}
	if img := x509.Spec.Image; img.Repository != "hub.omnitrustregistry.com/ilm/x509-compliance-provider" ||
		img.Tag != "1.3.1" || len(img.PullSecrets) != 0 {
		t.Errorf("x509 image %+v", img)
	}
	if x509.Spec.Service.Port != 8080 || len(x509.Spec.SecretRefs) != 0 {
		t.Errorf("x509 port %d secretRefs %v", x509.Spec.Service.Port, x509.Spec.SecretRefs)
	}
	if p := x509.Spec.Probes; p.Liveness.Path != "/v1/health" || p.Readiness.Path != "/v1/health" || p.Startup.Path != "/v1/health" {
		t.Errorf("x509 probes %+v %+v %+v", p.Liveness, p.Readiness, p.Startup)
	}

	if ejbca.Spec.Image.Tag != "1.13.0" || ejbca.Spec.Service.Port != 8080 {
		t.Errorf("ejbca image %+v port %d", ejbca.Spec.Image, ejbca.Spec.Service.Port)
	}
	if p := ejbca.Spec.Probes; p.Liveness.Path != "/health/liveness" || p.Readiness.Path != "/health/readiness" {
		t.Errorf("ejbca probes %+v %+v", p.Liveness, p.Readiness)
	}
	env := envMap(ejbca.Spec.Env)
	if env["JDBC_URL"] != "jdbc:postgresql://ilm-db-pooler:5432/ilm?characterEncoding=UTF-8" || env["PORT"] != "8080" {
		t.Errorf("ejbca env %v", env)
	}
	want := []otilm.SecretRef{{Name: "ilm-db-app", Type: otilm.RefTypeEnv, Keys: []otilm.RefKeyMapping{
		{SecretKey: "username", EnvVar: ptr("JDBC_USERNAME")},
		{SecretKey: "password", EnvVar: ptr("JDBC_PASSWORD")},
	}}}
	if !reflect.DeepEqual(ejbca.Spec.SecretRefs, want) {
		t.Errorf("ejbca secretRefs %+v", ejbca.Spec.SecretRefs)
	}
	wantRegistration := &otilm.RegistrationSpec{PlatformURL: "http://core.ilm.svc.cluster.local:8080/api", Name: "EJBCA NG Connector", AuthType: otilm.AuthTypeNone}
	if !reflect.DeepEqual(ejbca.Spec.Registration, wantRegistration) {
		t.Errorf("ejbca registration %+v, want %+v", ejbca.Spec.Registration, wantRegistration)
	}
	for _, c := range []struct {
		connector otilm.Connector
		name      string
	}{{x509, "X.509 Compliance Provider"}, {ejbca, "EJBCA NG Connector"}} {
		// The operator mounts the root file system read-only, so the connector writes temporary files to a volume.
		if want := []otilm.VolumeSpec{{Name: "tmp", MountPath: "/tmp"}}; !reflect.DeepEqual(c.connector.Spec.Volumes, want) {
			t.Errorf("%s volumes %+v, want %+v", c.connector.Name, c.connector.Spec.Volumes, want)
		}
	}
}

func TestRenderV1WithoutRegistration(t *testing.T) {
	m := loadManifest(t)
	for _, name := range []string{"x509-compliance-provider", "email-notification-provider"} {
		i := slices.IndexFunc(m.Connectors, func(c release.Connector) bool { return c.Name == name })
		if i < 0 || !m.Connectors[i].V1Only() {
			t.Fatalf("%s is not marked interface v1 in the catalogue", name)
		}
		c, err := renderConnector(m.Connectors[i], managedDatabase, nil, nil, SecretNames{})
		if err != nil {
			t.Fatal(err)
		}
		if c.Spec.Registration != nil {
			t.Errorf("%s registration %+v, want none", name, c.Spec.Registration)
		}
	}
	c := release.Connector{Name: "x", Image: "r/x:1", Port: 9000}
	if got, want := ConnectorURL(c), "http://x.ilm.svc.cluster.local:9000"; got != want {
		t.Errorf("ConnectorURL = %s, want %s", got, want)
	}
}

// testNodeIP is the node address, which the no-proxy lists hold.
const testNodeIP = "192.0.2.10"

// internalNoProxy is the no-proxy list of the node itself.
const internalNoProxy = "localhost,127.0.0.1,192.0.2.10,10.244.0.0/16,10.96.0.0/12,.svc,.cluster.local"

func TestRenderWithoutProxySettings(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"ejbca-ng-connector"}
	a.Spec.Proxy = &v1alpha1.ProxySpec{}
	p, connectors, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Common.Proxy.Enabled {
		t.Errorf("proxy %+v enabled without any setting", p.Spec.Common.Proxy)
	}
	if _, set := envMap(connectors[0].Spec.Env)["NO_PROXY"]; set {
		t.Error("NO_PROXY rendered without a proxy")
	}
}

func TestRenderConnectorSettings(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"ejbca-ng-connector", "hashicorp-vault-connector", "software-cryptography-provider"}
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTPS: "http://proxy:3128"}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{Trust: "ilm-trust"}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	ejbca, vault, soft := connectors[0], connectors[1], connectors[2]

	env := envMap(ejbca.Spec.Env)
	if env["HTTPS_PROXY"] != "http://proxy:3128" {
		t.Errorf("ejbca proxy env %v", env)
	}
	if _, set := env["HTTP_PROXY"]; set {
		t.Errorf("empty HTTP_PROXY rendered: %v", env)
	}
	if env["NO_PROXY"] != internalNoProxy {
		t.Errorf("ejbca NO_PROXY %q, want the entries the node needs", env["NO_PROXY"])
	}
	if !hasSecretRef(ejbca, "ilm-trust", "ca.crt", "TRUSTED_CERTIFICATES") {
		t.Errorf("ejbca trust secretRefs %+v", ejbca.Spec.SecretRefs)
	}

	env = envMap(vault.Spec.Env)
	for k, v := range map[string]string{
		"DATABASE_HOST": "ilm-db-pooler", "DATABASE_PORT": "5432", "DATABASE_NAME": "ilm",
		"DATABASE_SCHEMA": "hvault", "DATABASE_SSL_MODE": "disable",
	} {
		if env[k] != v {
			t.Errorf("vault %s = %q, want %q", k, env[k], v)
		}
	}
	if !hasSecretRef(vault, "ilm-db-app", "username", "DATABASE_USER") ||
		!hasSecretRef(vault, "ilm-db-app", "password", "DATABASE_PASSWORD") {
		t.Errorf("vault secretRefs %+v", vault.Spec.SecretRefs)
	}
	if _, set := envMap(vault.Spec.Env)["TRUSTED_CERTIFICATES"]; set || hasSecretRef(vault, "ilm-trust", "ca.crt", "TRUSTED_CERTIFICATES") {
		t.Error("trust rendered for a connector without the trust setting")
	}

	if !hasSecretRef(soft, "software-cryptography-provider-keys", "ENCRYPTION_KEY", "ENCRYPTION_KEY") {
		t.Errorf("software cryptography secretRefs %+v", soft.Spec.SecretRefs)
	}
}

func TestRenderPrivateNeedsPullSecret(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"x509-compliance-provider", "timestamp-formatting-connector"}

	_, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)

	if err == nil || !strings.Contains(err.Error(), "timestamp-formatting-connector") {
		t.Fatalf("want an error naming the private component, got %v", err)
	}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{PullSecret: "ilm-registry"}, testNodeIP)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(connectors[1].Spec.Image.PullSecrets, []string{"ilm-registry"}) {
		t.Errorf("pull secrets %v", connectors[1].Spec.Image.PullSecrets)
	}
	if connectors[0].Spec.Image.Digest != "" {
		t.Errorf("public connector digest %q, want none", connectors[0].Spec.Image.Digest)
	}
	if got := connectors[1].Spec.Image.Digest; !strings.HasPrefix(got, "sha256:") {
		t.Errorf("private connector digest %q, want a sha256 digest", got)
	}
}

func TestRenderUnknownComponent(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"no-such-connector"}

	if _, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP); err == nil || !strings.Contains(err.Error(), "no-such-connector") {
		t.Fatalf("want an error naming the component, got %v", err)
	}
}

func envMap(env []otilm.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

func hasSecretRef(c otilm.Connector, name, key, envVar string) bool {
	for _, ref := range c.Spec.SecretRefs {
		if ref.Name != name || ref.Type != otilm.RefTypeEnv {
			continue
		}
		for _, k := range ref.Keys {
			if k.SecretKey == key && k.EnvVar != nil && *k.EnvVar == envVar {
				return true
			}
		}
	}
	return false
}

func TestSplitTag(t *testing.T) {
	for ref, want := range map[string][2]string{
		"ghcr.io/org/app:1.2.3":  {"ghcr.io/org/app", "1.2.3"},
		"localhost:5000/org/app": {"localhost:5000/org/app", ""},
		"busybox":                {"busybox", ""},
		"busybox:1":              {"busybox", "1"},
		"registry.example/app":   {"registry.example/app", ""},
	} {
		if repository, tag := splitTag(ref); repository != want[0] || tag != want[1] {
			t.Errorf("splitTag(%q) = %q, %q, want %q, %q", ref, repository, tag, want[0], want[1])
		}
	}
}

func TestTLSOptionTrustsTheAdministratorCA(t *testing.T) {
	tests := map[string]struct {
		secrets SecretNames
		want    string
	}{
		"internal TLS": {SecretNames{}, "ca-keypair"},
		"secret TLS":   {SecretNames{TLS: TLSSecretName}, "admin-ca-keypair"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p, _, err := Render(defaultAppliance(), loadManifest(t), tt.secrets, testNodeIP)
			if err != nil {
				t.Fatal(err)
			}
			got := TLSOption(p)
			if got.GetKind() != "TLSOption" || got.GetAPIVersion() != "traefik.io/v1alpha1" || got.GetNamespace() != Namespace || got.GetName() != TLSOptionName {
				t.Errorf("identity %s %s %s/%s", got.GetAPIVersion(), got.GetKind(), got.GetNamespace(), got.GetName())
			}
			if ref := p.Spec.Edge.Annotations["traefik.ingress.kubernetes.io/router.tls.options"]; ref != Namespace+"-"+TLSOptionName+"@kubernetescrd" {
				t.Errorf("the Ingress selects %q, not the rendered TLSOption", ref)
			}
			auth, _, _ := unstructured.NestedMap(got.Object, "spec", "clientAuth")
			if !reflect.DeepEqual(auth, map[string]any{"clientAuthType": "VerifyClientCertIfGiven", "secretNames": []any{tt.want}}) {
				t.Errorf("clientAuth = %v, want VerifyClientCertIfGiven against %s", auth, tt.want)
			}
		})
	}
}

func TestUnstructuredLeavesOutAnUnsetTrust(t *testing.T) {
	for name, tc := range map[string]struct {
		trust string
		want  bool
	}{"without trust": {"", false}, "with trust": {"appliance-trust", true}} {
		p, _, err := Render(defaultAppliance(), loadManifest(t), SecretNames{Trust: tc.trust}, testNodeIP)
		if err != nil {
			t.Fatal(err)
		}
		u, err := Unstructured(p)
		if err != nil {
			t.Fatal(err)
		}
		ref, found, err := unstructured.NestedString(u.Object, "spec", "common", "trustedCertificates", "secretRef")
		if err != nil || found != tc.want || ref != tc.trust {
			t.Errorf("%s: trustedCertificates.secretRef = %q (found %v, %v)", name, ref, found, err)
		}
		if _, found, _ := unstructured.NestedMap(u.Object, "spec", "common", "trustedCertificates"); found != tc.want {
			t.Errorf("%s: trustedCertificates present %v, want %v", name, found, tc.want)
		}
		if u.GetKind() != "Platform" || u.GetName() != Name {
			t.Errorf("%s: object %s %s", name, u.GetKind(), u.GetName())
		}
	}
}

func TestEmailConnectorGetsMailSettings(t *testing.T) {
	const email = "email-notification-provider"
	// Spring Boot binds it to the JavaMail property mail.smtp.ssl.enable: implicit TLS.
	const sslEnableEnv = "SPRING_MAIL_PROPERTIES_MAIL_SMTP_SSL_ENABLE"
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{email, "ejbca-ng-connector"}

	// An Appliance that chose the connector without a mail server still renders, without mail settings.
	_, connectors, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := envMap(connectors[0].Spec.Env)["SMTP_HOST"]; set {
		t.Error("mail settings rendered without a mail server")
	}
	// Spring Boot's startup connection check would keep the connector from starting while the server is unreachable.
	if got := envMap(connectors[0].Spec.Env)["SPRING_MAIL_TESTCONNECTION"]; got != "false" {
		t.Errorf("SPRING_MAIL_TESTCONNECTION = %q, want false", got)
	}

	for _, tc := range []struct {
		security string
		want     map[string]string
		unset    []string
	}{
		{v1alpha1.MailSecurityNone, map[string]string{"SMTP_TLS": "false"}, []string{sslEnableEnv}},
		{v1alpha1.MailSecuritySTARTTLS, map[string]string{"SMTP_TLS": "true"}, []string{sslEnableEnv}},
		{v1alpha1.MailSecurityTLS, map[string]string{"SMTP_TLS": "false", sslEnableEnv: "true"}, nil},
	} {
		t.Run(tc.security, func(t *testing.T) {
			a.Spec.Mail = &v1alpha1.MailSpec{Host: "smtp.example.com", Port: 465, Security: tc.security, From: "ilm@example.com", CredentialsRef: "appliance-mail"}
			_, connectors, err := Render(a, loadManifest(t), SecretNames{Mail: "ilm-mail"}, testNodeIP)
			if err != nil {
				t.Fatal(err)
			}
			mailer, ejbca := connectors[0], connectors[1]
			env := envMap(mailer.Spec.Env)
			for k, v := range map[string]string{"SMTP_HOST": "smtp.example.com", "SMTP_PORT": "465", "SMTP_AUTH": "true"} {
				tc.want[k] = v
			}
			for k, v := range tc.want {
				if env[k] != v {
					t.Errorf("%s = %q, want %q", k, env[k], v)
				}
			}
			for _, k := range append(tc.unset, "SMTP_USERNAME", "SMTP_PASSWORD") {
				if _, set := env[k]; set {
					t.Errorf("%s rendered as a plain value", k)
				}
			}
			if !hasSecretRef(mailer, "ilm-mail", "username", "SMTP_USERNAME") || !hasSecretRef(mailer, "ilm-mail", "password", "SMTP_PASSWORD") {
				t.Errorf("mail secretRefs %+v", mailer.Spec.SecretRefs)
			}
			if _, set := envMap(ejbca.Spec.Env)["SMTP_HOST"]; set || hasSecretRef(ejbca, "ilm-mail", "password", "SMTP_PASSWORD") {
				t.Error("mail settings rendered for a connector without the mail setting")
			}
		})
	}

	// Without credentials the connector does not authenticate.
	a.Spec.Mail = &v1alpha1.MailSpec{Host: "smtp.example.com", Port: 25, Security: v1alpha1.MailSecurityNone, From: "ilm@example.com"}
	_, connectors, err = Render(a, loadManifest(t), SecretNames{}, testNodeIP)
	if err != nil {
		t.Fatal(err)
	}
	if env := envMap(connectors[0].Spec.Env); env["SMTP_AUTH"] != "false" || env["SMTP_PORT"] != "25" {
		t.Errorf("env without credentials %v", env)
	}
	if hasSecretRef(connectors[0], "ilm-mail", "password", "SMTP_PASSWORD") {
		t.Error("credentials referenced without a mail Secret")
	}
}

// externalAppliance is defaultAppliance with every dependency external.
func externalAppliance() *v1alpha1.Appliance {
	a := defaultAppliance()
	a.Spec.Platform.Database = &v1alpha1.ExternalDatabase{Host: "db.example.com", Port: 5433, Name: "ilmdb", CredentialsRef: "appliance-database"}
	a.Spec.Platform.Messaging = &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5673, VirtualHost: "ilm", CredentialsRef: "appliance-messaging"}
	a.Spec.Platform.Identity = &v1alpha1.ExternalIdentity{Issuer: "https://idp.example.com/realms/ilm", ClientID: "ilm", CredentialsRef: "appliance-identity"}
	return a
}

func TestRenderExternalDatabase(t *testing.T) {
	a := externalAppliance()
	a.Spec.Platform.Components = []string{"ejbca-ng-connector", "pyadcs-connector"}
	m := loadManifest(t)

	got, connectors, err := Render(a, m, SecretNames{Database: "ilm-database", Messaging: "ilm-messaging"}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	want := otilm.DatabaseSpec{Mode: "external", Host: "db.example.com", Port: 5433, Name: "ilmdb",
		Credentials: &otilm.CredentialsRef{SecretRef: "ilm-database"}}
	if !reflect.DeepEqual(got.Spec.Database, want) {
		t.Errorf("database %+v, want %+v", got.Spec.Database, want)
	}
	ejbca, pyadcs := connectors[0], connectors[1]
	if env := envMap(ejbca.Spec.Env); env["JDBC_URL"] != "jdbc:postgresql://db.example.com:5433/ilmdb?characterEncoding=UTF-8" {
		t.Errorf("ejbca env %v", env)
	}
	if ref := ejbca.Spec.SecretRefs[0]; ref.Name != "ilm-database" {
		t.Errorf("ejbca database secret %+v", ref)
	}
	if env := envMap(pyadcs.Spec.Env); env["DATABASE_HOST"] != "db.example.com" || env["DATABASE_PORT"] != "5433" || env["DATABASE_NAME"] != "ilmdb" {
		t.Errorf("pyadcs env %v", env)
	}
	if ref := pyadcs.Spec.SecretRefs[0]; ref.Name != "ilm-database" {
		t.Errorf("pyadcs database secret %+v", ref)
	}

	if _, _, err := Render(a, m, SecretNames{Messaging: "ilm-messaging"}, testNodeIP); err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("render without the copied credentials: %v", err)
	}
}

func TestRenderExternalMessaging(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Messaging = externalAppliance().Spec.Platform.Messaging
	m := loadManifest(t)

	got, _, err := Render(a, m, SecretNames{Messaging: "ilm-messaging"}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	want := otilm.MessagingSpec{Mode: "external", BrokerType: "rabbitmq", Host: "mq.example.com", Port: 5673, VirtualHost: "ilm",
		Credentials: &otilm.CredentialsRef{SecretRef: "ilm-messaging"}}
	if !reflect.DeepEqual(got.Spec.Messaging, want) {
		t.Errorf("messaging %+v, want %+v", got.Spec.Messaging, want)
	}
	if got.Spec.Database.Mode != "managed" {
		t.Errorf("database %+v", got.Spec.Database)
	}
	if _, _, err := Render(a, m, SecretNames{}, testNodeIP); err == nil || !strings.Contains(err.Error(), "messaging") {
		t.Errorf("render without the copied credentials: %v", err)
	}
}

func TestRenderExternalIdentity(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Identity = externalAppliance().Spec.Platform.Identity

	got, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	if want := (&otilm.KeycloakSpec{Mode: "external"}); !reflect.DeepEqual(got.Spec.Keycloak, want) {
		t.Errorf("keycloak %+v, want %+v", got.Spec.Keycloak, want)
	}
}

func TestRenderUtils(t *testing.T) {
	a := defaultAppliance()
	got, _, err := Render(a, loadManifest(t), SecretNames{}, testNodeIP)
	if err != nil || got.Spec.Utils.Enabled {
		t.Fatalf("utils by default %+v, %v", got.Spec.Utils, err)
	}
	a.Spec.Platform.Utils = true
	got, _, err = Render(a, loadManifest(t), SecretNames{}, testNodeIP)
	if err != nil || !got.Spec.Utils.Enabled {
		t.Errorf("utils %+v, %v", got.Spec.Utils, err)
	}
}

// componentEnv is the value of the variable name in env, or "".
func componentEnv(env []otilm.EnvVar, name string) string {
	return envMap(env)[name]
}

func TestRenderDatabaseTLS(t *testing.T) {
	ca := &v1alpha1.SecretKeyRef{Name: "appliance-database", Key: v1alpha1.CAKey}
	caVolume := otilm.SecretRef{Name: "ilm-database-ca", Type: otilm.RefTypeVolume, MountPath: ptr("/etc/ilm/db-ca")}
	for _, tc := range []struct {
		mode     string
		ca       *v1alpha1.SecretKeyRef
		url      string
		volumes  bool
		pgEnvTLS string
	}{
		{mode: "", url: ""},
		{mode: "disable", url: "jdbc:postgresql://db.example.com:5433/ilmdb?characterEncoding=UTF-8&sslmode=disable", pgEnvTLS: "disable"},
		{mode: "require", url: "jdbc:postgresql://db.example.com:5433/ilmdb?characterEncoding=UTF-8&sslmode=require", pgEnvTLS: "require"},
		{mode: "verify-full", ca: ca, volumes: true, pgEnvTLS: "verify-full",
			url: "jdbc:postgresql://db.example.com:5433/ilmdb?characterEncoding=UTF-8&sslmode=verify-full&sslrootcert=/etc/ilm/db-ca/ca.crt"},
	} {
		t.Run("mode "+tc.mode, func(t *testing.T) {
			a := defaultAppliance()
			a.Spec.Platform.Components = []string{"ejbca-ng-connector", "pyadcs-connector"}
			a.Spec.Platform.Database = externalAppliance().Spec.Platform.Database
			a.Spec.Platform.Database.TLSMode, a.Spec.Platform.Database.CA = tc.mode, tc.ca
			secrets := SecretNames{Database: "ilm-database"}
			if tc.ca != nil {
				secrets.DatabaseCA = "ilm-database-ca"
			}

			got, connectors, err := Render(a, loadManifest(t), secrets, testNodeIP)

			if err != nil {
				t.Fatal(err)
			}
			for name, c := range map[string]otilm.ComponentSpec{"core": got.Spec.Core.ComponentSpec, "scheduler": got.Spec.Scheduler.ComponentSpec} {
				if v := componentEnv(c.Env, "SPRING_DATASOURCE_URL"); v != tc.url {
					t.Errorf("%s SPRING_DATASOURCE_URL %q, want %q", name, v, tc.url)
				}
				if componentEnv(c.Env, "JDBC_URL") != "" {
					t.Errorf("%s overrides JDBC_URL", name)
				}
				if hasVolume := slices.ContainsFunc(c.SecretRefs, func(r otilm.SecretRef) bool { return reflect.DeepEqual(r, caVolume) }); hasVolume != tc.volumes {
					t.Errorf("%s secretRefs %+v", name, c.SecretRefs)
				}
			}
			ejbca, pyadcs := connectors[0], connectors[1]
			wantJDBC := tc.url
			if wantJDBC == "" {
				wantJDBC = "jdbc:postgresql://db.example.com:5433/ilmdb?characterEncoding=UTF-8"
			}
			if v := componentEnv(ejbca.Spec.Env, "JDBC_URL"); v != wantJDBC {
				t.Errorf("connector JDBC_URL %q, want %q", v, wantJDBC)
			}
			if v := componentEnv(pyadcs.Spec.Env, "PGSSLMODE"); v != tc.pgEnvTLS {
				t.Errorf("connector PGSSLMODE %q, want %q", v, tc.pgEnvTLS)
			}
			for _, c := range connectors {
				if hasVolume := slices.ContainsFunc(c.Spec.SecretRefs, func(r otilm.SecretRef) bool { return reflect.DeepEqual(r, caVolume) }); hasVolume != tc.volumes {
					t.Errorf("%s secretRefs %+v", c.Name, c.Spec.SecretRefs)
				}
			}
			if tc.volumes && componentEnv(pyadcs.Spec.Env, "PGSSLROOTCERT") != "/etc/ilm/db-ca/ca.crt" {
				t.Errorf("pyadcs env %v", envMap(pyadcs.Spec.Env))
			}
		})
	}

	a := defaultAppliance()
	a.Spec.Platform.Database = &v1alpha1.ExternalDatabase{Host: "db", Port: 5432, Name: "ilm", CredentialsRef: "c", TLSMode: "verify-full", CA: ca}
	if _, _, err := Render(a, loadManifest(t), SecretNames{Database: "ilm-database"}, testNodeIP); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Errorf("render without the copied CA: %v", err)
	}
}

func TestRenderMessagingTLS(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Messaging = &v1alpha1.ExternalMessaging{Host: "mq.example.com", Port: 5671, VirtualHost: "ilm", CredentialsRef: "appliance-messaging",
		TLS: true, CA: &v1alpha1.SecretKeyRef{Name: "appliance-messaging", Key: v1alpha1.CAKey}}

	got, _, err := Render(a, loadManifest(t), SecretNames{Messaging: "ilm-messaging", MessagingCA: "ilm-messaging-ca"}, testNodeIP)

	if err != nil {
		t.Fatal(err)
	}
	wantURL := "amqps://mq.example.com:5671?transport.trustStoreLocation=/etc/ilm/broker-ca/truststore.p12&transport.trustStoreType=PKCS12&transport.trustStorePassword=" + TrustStorePassword
	volume := otilm.SecretRef{Name: "ilm-messaging-ca", Type: otilm.RefTypeVolume, MountPath: ptr("/etc/ilm/broker-ca")}
	for name, c := range map[string]otilm.ComponentSpec{"core": got.Spec.Core.ComponentSpec, "scheduler": got.Spec.Scheduler.ComponentSpec} {
		if v := componentEnv(c.Env, "BROKER_URL"); v != wantURL {
			t.Errorf("%s BROKER_URL %q", name, v)
		}
		if !slices.ContainsFunc(c.SecretRefs, func(r otilm.SecretRef) bool { return reflect.DeepEqual(r, volume) }) {
			t.Errorf("%s secretRefs %+v", name, c.SecretRefs)
		}
	}
	if got.Spec.Messaging.Port != 5671 || got.Spec.Messaging.VirtualHost != "ilm" {
		t.Errorf("messaging %+v", got.Spec.Messaging)
	}

	a.Spec.Platform.Messaging.TLS, a.Spec.Platform.Messaging.CA = false, nil
	got, _, err = Render(a, loadManifest(t), SecretNames{Messaging: "ilm-messaging"}, testNodeIP)
	if err != nil || componentEnv(got.Spec.Core.Env, "BROKER_URL") != "" || len(got.Spec.Core.SecretRefs) != 0 {
		t.Errorf("plain broker core %+v, %v", got.Spec.Core, err)
	}
	a.Spec.Platform.Messaging.TLS = true
	a.Spec.Platform.Messaging.CA = &v1alpha1.SecretKeyRef{Name: "appliance-messaging", Key: v1alpha1.CAKey}
	if _, _, err := Render(a, loadManifest(t), SecretNames{Messaging: "ilm-messaging"}, testNodeIP); err == nil {
		t.Error("rendered a TLS broker without the trust store copy")
	}
}
