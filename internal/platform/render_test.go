package platform

import (
	"os"
	"reflect"
	"strings"
	"testing"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
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

	got, connectors, err := Render(defaultAppliance(), loadManifest(t), SecretNames{})

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

	got, _, err := Render(a, loadManifest(t), SecretNames{})

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

	got, _, err := Render(a, loadManifest(t), secrets)

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
	want := otilm.OutboundProxySpec{Enabled: true, HTTP: "http://proxy:3128", HTTPS: "http://proxy:3128", NoProxy: ".svc"}
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

	got, _, err := Render(a, loadManifest(t), SecretNames{})

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

	if _, _, err := Render(a, loadManifest(t), SecretNames{}); err == nil {
		t.Fatal("want an error when the TLS Secret is missing")
	}
}

func TestRenderComponents(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"x509-compliance-provider", "ejbca-ng-connector"}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{})

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
}

func TestRenderConnectorSettings(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"ejbca-ng-connector", "hashicorp-vault-connector", "software-cryptography-provider"}
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTPS: "http://proxy:3128"}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{Trust: "ilm-trust"})

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

	_, _, err := Render(a, loadManifest(t), SecretNames{})

	if err == nil || !strings.Contains(err.Error(), "timestamp-formatting-connector") {
		t.Fatalf("want an error naming the private component, got %v", err)
	}

	_, connectors, err := Render(a, loadManifest(t), SecretNames{PullSecret: "ilm-registry"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(connectors[1].Spec.Image.PullSecrets, []string{"ilm-registry"}) {
		t.Errorf("pull secrets %v", connectors[1].Spec.Image.PullSecrets)
	}
}

func TestRenderUnknownComponent(t *testing.T) {
	a := defaultAppliance()
	a.Spec.Platform.Components = []string{"no-such-connector"}

	if _, _, err := Render(a, loadManifest(t), SecretNames{}); err == nil || !strings.Contains(err.Error(), "no-such-connector") {
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
