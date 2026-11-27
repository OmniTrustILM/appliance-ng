package nodeconfig_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	coreconfig "github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
)

const (
	nodeIP   = "192.0.2.10"
	link     = "enp0s1"
	proxyURL = "http://proxy.example.com:3128"
)

// metalMode is the validation mode of talosctl validate --mode metal.
type metalMode struct{}

func (metalMode) String() string        { return constants.PlatformMetal }
func (metalMode) RequiresInstall() bool { return true }
func (metalMode) InContainer() bool     { return false }

func appliance() *v1alpha1.Appliance {
	return &v1alpha1.Appliance{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ApplianceName},
		Spec: v1alpha1.ApplianceSpec{
			Node:  v1alpha1.NodeSpec{HostName: "ilm.example.com", Address: "192.0.2.10/24", Gateway: "192.0.2.1"},
			DNS:   v1alpha1.DNSSpec{Servers: []string{"192.0.2.53", "2001:db8::53"}},
			NTP:   v1alpha1.NTPSpec{Servers: []string{"ntp.example.com", "192.0.2.123"}},
			Proxy: &v1alpha1.ProxySpec{HTTP: proxyURL, HTTPS: proxyURL, NoProxy: ".example.com"},
		},
	}
}

// runningLink is the link configuration first boot gives a static address.
func runningLink() *network.LinkConfigV1Alpha1 {
	return nodeconfig.LinkConfig(link, netip.MustParsePrefix("192.0.2.10/24"), netip.MustParseAddr("192.0.2.254"))
}

func render(t *testing.T, a *v1alpha1.Appliance, n nodeconfig.Node) []config.Document {
	t.Helper()
	docs, err := nodeconfig.Render(a, n, nodeconfig.Secrets{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return docs
}

func kinds(docs []config.Document) []string {
	var out []string
	for _, doc := range docs {
		out = append(out, doc.Kind())
	}
	return out
}

// generated returns a complete machine configuration from first boot, for validating documents in their context.
func generated(t *testing.T) coreconfig.Provider {
	t.Helper()
	out, err := machineconfig.Generate(machineconfig.Input{ClusterName: "appliance", NodeIP: nodeIP, InstallerImage: "factory.talos.dev/installer/abc:v1.14.5"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configloader.NewFromBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// withDocuments returns cfg with its documents of the kinds of docs replaced by docs.
func withDocuments(t *testing.T, cfg coreconfig.Provider, docs []config.Document) coreconfig.Provider {
	t.Helper()
	replaced := kinds(docs)
	var all []config.Document
	for _, doc := range cfg.Documents() {
		if !slices.Contains(replaced, doc.Kind()) {
			all = append(all, doc)
		}
	}
	out, err := container.New(append(all, docs...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRenderNodeDocuments(t *testing.T) {
	a := appliance()
	// The spec's address differs from the running one only to show that the address never comes from the spec.
	a.Spec.Node.Address = "192.0.2.99/24"
	docs := render(t, a, nodeconfig.Node{IP: nodeIP, Link: runningLink()})

	want := []string{network.HostnameKind, network.LinkKind, network.ResolverKind, network.TimeSyncKind, runtime.EnvironmentConfigKind}
	if got := kinds(docs); !slices.Equal(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for _, doc := range docs {
		if v, ok := doc.(config.Validator); ok {
			if _, err := v.Validate(metalMode{}, validation.WithStrict()); err != nil {
				t.Errorf("%s does not validate: %v", doc.Kind(), err)
			}
		}
	}
	cfg := withDocuments(t, generated(t), docs)
	if _, err := cfg.ValidateAsClient(metalMode{}, validation.WithStrict()); err != nil {
		t.Fatalf("the configuration with the rendered documents does not validate: %v", err)
	}

	linkDoc := docs[1].(*network.LinkConfigV1Alpha1)
	if linkDoc.MetaName != link || len(linkDoc.LinkAddresses) != 1 || linkDoc.LinkAddresses[0].AddressAddress.String() != "192.0.2.10/24" {
		t.Errorf("link %q addresses %v, want the running address kept", linkDoc.MetaName, linkDoc.LinkAddresses)
	}
	if len(linkDoc.LinkRoutes) != 1 || linkDoc.LinkRoutes[0].RouteGateway.String() != "192.0.2.1" {
		t.Errorf("routes %v, want one default route through the spec's gateway", linkDoc.LinkRoutes)
	}
	env := docs[4].(*runtime.EnvironmentV1Alpha1).EnvironmentVariables
	if !strings.HasPrefix(env["NO_PROXY"], "localhost,127.0.0.1,"+nodeIP+",") || !strings.HasSuffix(env["NO_PROXY"], ",.example.com") {
		t.Errorf("NO_PROXY %q", env["NO_PROXY"])
	}
	encoded := encodeAll(t, docs)
	if strings.Contains(encoded, "192.0.2.99") {
		t.Errorf("the spec's address was rendered:\n%s", encoded)
	}
}

func encodeAll(t *testing.T, docs []config.Document) string {
	t.Helper()
	var b strings.Builder
	for _, doc := range docs {
		data, err := encoder.NewEncoder(doc, encoder.WithComments(encoder.CommentsDisabled)).Encode()
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteString("---\n")
	}
	return b.String()
}

// TestRenderMatchesFirstBoot checks that the documents of an Appliance equal those first boot generated for the same
// answers, so the manager changes nothing on a node it just took over.
func TestRenderMatchesFirstBoot(t *testing.T) {
	a := appliance()
	out, err := machineconfig.Generate(machineconfig.Input{
		ClusterName: "appliance", NodeIP: nodeIP, InstallerImage: "factory.talos.dev/installer/abc:v1.14.5",
		HostName: a.Spec.Node.HostName, Link: link, Address: netip.MustParsePrefix(a.Spec.Node.Address),
		Gateway: netip.MustParseAddr(a.Spec.Node.Gateway),
		DNS:     []netip.Addr{netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8::53")},
		NTP:     a.Spec.NTP.Servers,
		Proxy:   &machineconfig.Proxy{HTTP: proxyURL, HTTPS: proxyURL, NoProxy: ".example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configloader.NewFromBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	var running []config.Document
	for _, doc := range cfg.Documents() {
		if slices.Contains(nodeconfig.Kinds, doc.Kind()) {
			running = append(running, doc)
		}
	}
	docs := render(t, a, nodeconfig.Node{IP: nodeIP, Link: nodeconfig.StaticLink(running)})
	slices.SortFunc(running, func(a, b config.Document) int { return strings.Compare(a.Kind(), b.Kind()) })
	slices.SortFunc(docs, func(a, b config.Document) int { return strings.Compare(a.Kind(), b.Kind()) })
	if got, want := encodeAll(t, docs), encodeAll(t, running); got != want {
		t.Fatalf("rendered documents differ from first boot's\nrendered:\n%s\nfirst boot:\n%s", got, want)
	}
}

func TestRenderWithDHCPAndNoProxy(t *testing.T) {
	a := appliance()
	a.Spec.Node.Address, a.Spec.Node.Gateway, a.Spec.Proxy, a.Spec.NTP.Servers = "", "", &v1alpha1.ProxySpec{}, nil
	docs := render(t, a, nodeconfig.Node{IP: nodeIP})
	if got, want := kinds(docs), []string{network.HostnameKind, network.ResolverKind, network.TimeSyncKind}; !slices.Equal(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	if ts := docs[2].(*network.TimeSyncConfigV1Alpha1); ts.TimeNTP != nil || ts.TimeBootTimeout == 0 {
		t.Errorf("time sync %+v, want the boot timeout and the default servers", ts)
	}
}

func TestRenderWithoutGatewayDropsTheRoute(t *testing.T) {
	a := appliance()
	a.Spec.Node.Gateway = ""
	docs := render(t, a, nodeconfig.Node{IP: nodeIP, Link: runningLink()})
	if l := docs[1].(*network.LinkConfigV1Alpha1); len(l.LinkRoutes) != 0 || len(l.LinkAddresses) != 1 {
		t.Errorf("link routes %v addresses %v, want no route and the address kept", l.LinkRoutes, l.LinkAddresses)
	}
}

func TestRenderRefuses(t *testing.T) {
	for name, edit := range map[string]func(*v1alpha1.Appliance){
		"no host name":         func(a *v1alpha1.Appliance) { a.Spec.Node.HostName = "" },
		"gateway without link": func(a *v1alpha1.Appliance) { a.Spec.Node.Address = "" },
		"bad gateway":          func(a *v1alpha1.Appliance) { a.Spec.Node.Gateway = "gateway" },
		"bad DNS server":       func(a *v1alpha1.Appliance) { a.Spec.DNS.Servers = []string{"dns.example.com"} },
	} {
		a := appliance()
		edit(a)
		if _, err := nodeconfig.Render(a, nodeconfig.Node{IP: nodeIP}, nodeconfig.Secrets{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRenderTrustedRootsAndRegistryAuth(t *testing.T) {
	const bundle = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	secrets := nodeconfig.Secrets{
		TrustedRoots: bundle,
		Registries: []nodeconfig.RegistryAuth{
			{Host: "registry.example.com:5000", Username: "robot", Password: "pa55"},
			{Host: "hub.example.com", Username: "puller", Password: "w0rd"},
		},
	}
	docs, err := nodeconfig.Render(appliance(), nodeconfig.Node{IP: nodeIP, Link: runningLink()}, secrets)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{network.HostnameKind, network.LinkKind, network.ResolverKind, network.TimeSyncKind, runtime.EnvironmentConfigKind,
		security.TrustedRootsConfig, cri.RegistryAuthConfig, cri.RegistryAuthConfig}
	if got := kinds(docs); !slices.Equal(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for _, kind := range []string{security.TrustedRootsConfig, cri.RegistryAuthConfig} {
		if !slices.Contains(nodeconfig.Kinds, kind) {
			t.Errorf("%s is not among the managed kinds", kind)
		}
	}
	cfg := withDocuments(t, generated(t), docs)
	if _, err := cfg.ValidateAsClient(metalMode{}, validation.WithStrict()); err != nil {
		t.Fatalf("the configuration with the rendered documents does not validate: %v", err)
	}
	if roots := cfg.TrustedRoots().ExtraTrustedRootCertificates(); len(roots) != 1 || !strings.HasSuffix(roots[0], bundle) {
		t.Errorf("trusted roots %q, want the bundle", roots)
	}
	auths := cfg.RegistryAuthConfigs()
	if a := auths["registry.example.com:5000"]; a == nil || a.Username() != "robot" || a.Password() != "pa55" {
		t.Errorf("auth of registry.example.com:5000 = %+v, want its credentials", a)
	}
	if a := auths["hub.example.com"]; a == nil || a.Username() != "puller" || a.Password() != "w0rd" {
		t.Errorf("auth of hub.example.com = %+v, want its credentials", a)
	}
}

func TestRenderRegistryAuthOncePerHost(t *testing.T) {
	secrets := nodeconfig.Secrets{Registries: []nodeconfig.RegistryAuth{
		{Host: "registry.example.com", Username: "robot", Password: "pa55"},
		{Host: "registry.example.com", Username: "other", Password: "w0rd"},
	}}
	docs, err := nodeconfig.Render(appliance(), nodeconfig.Node{IP: nodeIP, Link: runningLink()}, secrets)
	if err != nil {
		t.Fatal(err)
	}
	var auths []*cri.RegistryAuthConfigV1Alpha1
	for _, doc := range docs {
		if a, ok := doc.(*cri.RegistryAuthConfigV1Alpha1); ok {
			auths = append(auths, a)
		}
	}
	if len(auths) != 1 || auths[0].RegistryUsername != "robot" {
		t.Fatalf("registry auth documents %+v, want one with the first entry's credentials", auths)
	}
	if _, err := container.New(docs...); err != nil {
		t.Errorf("the documents do not form a configuration: %v", err)
	}
}

func TestStaticLink(t *testing.T) {
	dhcp := network.NewLinkConfigV1Alpha1("enp0s2")
	static := runningLink()
	if got := nodeconfig.StaticLink([]config.Document{nodeconfig.HostnameConfig("a.example.com"), dhcp, static}); got != static {
		t.Errorf("StaticLink = %v, want the link with an address", got)
	}
	if got := nodeconfig.StaticLink([]config.Document{dhcp}); got != nil {
		t.Errorf("StaticLink = %v, want nil without an address", got)
	}
}

func TestNoProxy(t *testing.T) {
	if got, want := nodeconfig.NoProxy("", ""), "localhost,127.0.0.1,10.244.0.0/16,10.96.0.0/12,.svc,.cluster.local"; got != want {
		t.Errorf("NoProxy = %q, want %q", got, want)
	}
}

// testCA returns the PEM certificate of a new self-signed CA.
func testCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestRenderMirrorsAfterZotReady(t *testing.T) {
	ca := testCA(t)
	upstreams := []string{"docker.io", "ghcr.io", "quay.io"}
	docs := render(t, appliance(), nodeconfig.Node{IP: nodeIP, Link: runningLink(),
		Registry: &nodeconfig.Registry{CA: []byte(ca), Upstreams: upstreams}})

	want := []string{network.HostnameKind, network.LinkKind, network.ResolverKind, network.TimeSyncKind, runtime.EnvironmentConfigKind,
		cri.RegistryMirrorConfig, cri.RegistryMirrorConfig, cri.RegistryMirrorConfig, cri.RegistryTLSConfig}
	if got := kinds(docs); !slices.Equal(got, want) {
		t.Fatalf("kinds %v, want %v", got, want)
	}
	for _, kind := range []string{cri.RegistryMirrorConfig, cri.RegistryTLSConfig} {
		if !slices.Contains(nodeconfig.Kinds, kind) {
			t.Errorf("%s is not among the managed kinds", kind)
		}
	}
	cfg := withDocuments(t, generated(t), docs)
	if _, err := cfg.ValidateAsClient(metalMode{}, validation.WithStrict()); err != nil {
		t.Fatalf("the configuration with the mirrors does not validate: %v", err)
	}
	mirrors := cfg.RegistryMirrorConfigs()
	for _, host := range upstreams {
		m := mirrors[host]
		if m == nil {
			t.Fatalf("no mirror for %s in %v", host, mirrors)
		}
		endpoints := m.Endpoints()
		if len(endpoints) != 1 || endpoints[0].Endpoint() != "https://127.0.0.1:5443/v2/"+host || !endpoints[0].OverridePath() {
			t.Errorf("mirror of %s = %+v, want zot at https://127.0.0.1:5443/v2/%s with the path as given", host, endpoints, host)
		}
		if m.SkipFallback() {
			t.Errorf("mirror of %s skips the fallback to the upstream", host)
		}
	}
	tls := cfg.RegistryTLSConfigs()[nodeconfig.MirrorHost]
	if tls == nil || string(tls.CA()) != ca {
		t.Errorf("TLS of %s = %+v, want the CA", nodeconfig.MirrorHost, tls)
	}
}

func TestNoMirrorsWithoutZot(t *testing.T) {
	docs := render(t, appliance(), nodeconfig.Node{IP: nodeIP, Link: runningLink()})
	for _, kind := range kinds(docs) {
		if kind == cri.RegistryMirrorConfig || kind == cri.RegistryTLSConfig {
			t.Errorf("a %s before zot is ready", kind)
		}
	}
}

func TestRegistryMirrorOfAHostWithAPort(t *testing.T) {
	docs := nodeconfig.RegistryMirrors(nil, []string{"ghcr.io", "localhost:5005"})
	mirrors := map[string]*cri.RegistryMirrorConfigV1Alpha1{}
	for _, doc := range docs {
		if m, ok := doc.(*cri.RegistryMirrorConfigV1Alpha1); ok {
			mirrors[m.Name()] = m
		}
	}
	for host, want := range map[string]string{
		"ghcr.io":        "https://127.0.0.1:5443/v2/ghcr.io",
		"localhost:5005": "https://127.0.0.1:5443/v2/localhost_5005",
	} {
		m := mirrors[host]
		if m == nil || len(m.RegistryEndpoints) != 1 {
			t.Fatalf("no mirror document for %s in %v", host, mirrors)
		}
		ep := m.RegistryEndpoints[0]
		if got := ep.EndpointURL.String(); got != want {
			t.Errorf("%s: endpoint %s, want %s", host, got, want)
		}
		if ep.EndpointOverridePath == nil || !*ep.EndpointOverridePath {
			t.Errorf("%s: overridePath is not set", host)
		}
	}
}
