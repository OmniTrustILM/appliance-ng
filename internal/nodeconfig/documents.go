// Package nodeconfig builds the Talos documents of the node settings the appliance manages, for the first boot and
// for the Appliance Manager, and validates those settings.
package nodeconfig

import (
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/siderolabs/go-pointer"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// TrustedRootsName is the name of the document with the additional trusted CA certificates.
const TrustedRootsName = "appliance"

// timeSyncBootTimeout bounds the wait for NTP at boot: without a reachable NTP server Talos never reports time as
// synced and refuses to bootstrap etcd, so it trusts the hardware clock after this long.
const timeSyncBootTimeout = 30 * time.Second

// Proxy is the outbound HTTP proxy of the node and its services.
type Proxy struct {
	HTTP, HTTPS, NoProxy string
}

// HostnameConfig returns the document that gives the node the host name name, without an automatic name.
func HostnameConfig(name string) *network.HostnameConfigV1Alpha1 {
	doc := network.NewHostnameConfigV1Alpha1()
	doc.ConfigAuto = pointer.To(nethelpers.AutoHostnameKindOff)
	doc.ConfigHostname = name
	return doc
}

// LinkConfig returns the document that puts the static address on link, with the default route through gateway.
func LinkConfig(link string, address netip.Prefix, gateway netip.Addr) *network.LinkConfigV1Alpha1 {
	doc := network.NewLinkConfigV1Alpha1(link)
	doc.LinkUp = pointer.To(true)
	doc.LinkAddresses = []network.AddressConfig{{AddressAddress: address}}
	return WithGateway(doc, gateway)
}

// WithGateway returns a copy of link whose only route is the default route through gateway, or without routes for
// the zero gateway. Everything else of link, its addresses included, is kept.
func WithGateway(link *network.LinkConfigV1Alpha1, gateway netip.Addr) *network.LinkConfigV1Alpha1 {
	doc := link.DeepCopy()
	doc.LinkRoutes = nil
	if gateway.IsValid() {
		doc.LinkRoutes = []network.RouteConfig{{RouteGateway: meta.Addr{Addr: gateway}}}
	}
	return doc
}

// ResolverConfig returns the document with the name servers dns, keeping the host DNS cache that the generated
// configuration enables; empty dns keeps the name servers Talos uses.
func ResolverConfig(dns []netip.Addr) *network.ResolverConfigV1Alpha1 {
	doc := network.NewResolverConfigV1Alpha1()
	doc.ResolverHostDNS = network.HostDNSConfig{
		HostDNSEnabled:              pointer.To(true),
		HostDNSForwardKubeDNSToHost: pointer.To(true),
	}
	for _, addr := range dns {
		doc.ResolverNameservers = append(doc.ResolverNameservers, network.NameserverConfig{Address: meta.Addr{Addr: addr}})
	}
	return doc
}

// TimeSyncConfig returns the document with the time servers ntp, host names or addresses; empty ntp keeps the Talos
// default servers.
func TimeSyncConfig(ntp []string) *network.TimeSyncConfigV1Alpha1 {
	doc := network.NewTimeSyncConfigV1Alpha1()
	doc.TimeBootTimeout = timeSyncBootTimeout
	if len(ntp) > 0 {
		doc.TimeNTP = &network.NTPConfig{Servers: ntp}
	}
	return doc
}

// EnvironmentConfig returns the document with the proxy environment of p for the node at nodeIP, or nil without a
// proxy.
func EnvironmentConfig(p *Proxy, nodeIP string) *runtime.EnvironmentV1Alpha1 {
	if p == nil {
		return nil
	}
	doc := runtime.NewEnvironmentV1Alpha1()
	doc.EnvironmentVariables = map[string]string{"NO_PROXY": NoProxy(nodeIP, p.NoProxy)}
	for name, value := range map[string]string{"HTTP_PROXY": p.HTTP, "HTTPS_PROXY": p.HTTPS} {
		if value != "" {
			doc.EnvironmentVariables[name] = value
		}
	}
	return doc
}

// NoProxy returns the no-proxy list for the node at nodeIP: what the cluster reaches on the node itself, through
// its pod and service networks and by its cluster names comes first, so that etcd, the kubelet and in-cluster
// traffic never go through the proxy, then the administrator's entries extra. An empty nodeIP is left out.
func NoProxy(nodeIP, extra string) string {
	entries := []string{"localhost", "127.0.0.1"}
	if nodeIP != "" {
		entries = append(entries, nodeIP)
	}
	entries = append(entries, constants.DefaultIPv4PodCIDR, constants.DefaultIPv4ServiceCIDR, ".svc", "."+constants.DefaultDNSDomain)
	if extra != "" {
		entries = append(entries, extra)
	}
	return strings.Join(entries, ",")
}

// TrustedRootsConfig returns the document that adds the CA certificates of the PEM bundle to those the node trusts.
func TrustedRootsConfig(bundle string) *security.TrustedRootsConfigV1Alpha1 {
	doc := security.NewTrustedRootsConfigV1Alpha1()
	doc.MetaName = TrustedRootsName
	doc.Certificates = bundle
	return doc
}

// RegistryAuthConfig returns the document with the credentials the node, and every workload, pulls images from the
// registry of r with.
func RegistryAuthConfig(r RegistryAuth) *cri.RegistryAuthConfigV1Alpha1 {
	doc := cri.NewRegistryAuthConfigV1Alpha1(r.Host)
	doc.RegistryUsername = r.Username
	doc.RegistryPassword = r.Password
	return doc
}

// MirrorHost is where zot serves the node: its loopback address and zot's port.
const MirrorHost = "127.0.0.1:5443"

// RegistryMirrors returns the documents that send the pulls from each registry of upstreams to zot first, trusting
// the CA caPEM. A pull zot cannot serve falls back to the upstream registry.
func RegistryMirrors(caPEM []byte, upstreams []string) []config.Document {
	docs := make([]config.Document, 0, len(upstreams)+1)
	for _, host := range upstreams {
		doc := cri.NewRegistryMirrorConfigV1Alpha1(host)
		doc.RegistryEndpoints = []cri.RegistryEndpoint{{
			// zot keeps each image under its upstream registry's name.
			EndpointURL:          meta.URL{URL: &url.URL{Scheme: "https", Host: MirrorHost, Path: "/v2/" + release.MirrorPath(host)}},
			EndpointOverridePath: pointer.To(true),
		}}
		docs = append(docs, doc)
	}
	tls := cri.NewRegistryTLSConfigV1Alpha1(MirrorHost)
	tls.TLSCA = string(caPEM)
	return append(docs, tls)
}
