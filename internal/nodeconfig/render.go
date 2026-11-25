package nodeconfig

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/cri"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/security"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// Kinds are the kinds of the documents the appliance manages on the running node.
var Kinds = []string{
	network.HostnameKind, network.LinkKind, network.ResolverKind, network.TimeSyncKind, runtime.EnvironmentConfigKind,
	security.TrustedRootsConfig, cri.RegistryAuthConfig, cri.RegistryMirrorConfig, cri.RegistryTLSConfig,
}

// Node is what the running node adds to the Appliance for its documents.
type Node struct {
	// IP is the node address, which the no-proxy list holds.
	IP string
	// Link is the running link configuration with the static address; nil with DHCP.
	Link *network.LinkConfigV1Alpha1
	// Registry is zot, which the node pulls every image through first; nil until zot is ready.
	Registry *Registry
}

// Registry is what the node needs to pull through zot.
type Registry struct {
	// CA is the PEM certificate of the CA zot's certificate is issued by.
	CA []byte
	// Upstreams are the registries zot holds images of.
	Upstreams []string
}

// Secrets is what the Secrets the Appliance references add to its documents.
type Secrets struct {
	// TrustedRoots is the PEM bundle of the additional trusted CA certificates; empty for none.
	TrustedRoots string
	// Registries are the registries with credentials.
	Registries []RegistryAuth
}

// RegistryAuth is the credentials the node pulls images from a registry with.
type RegistryAuth struct {
	// Host is the registry's host name or address, with an optional port.
	Host               string
	Username, Password string
}

// Render returns the documents of the node settings of a for node: the host name, the default route of the static
// link, the name servers, the time servers, with a proxy the proxy environment, and from secrets the trusted CA
// certificates and the registry credentials, those of the first entry of a host, and once zot is ready the mirrors
// that send every pull to it first. The address is never rendered: it is fixed at setup, so the static link keeps
// the address it runs with and only its route follows the gateway.
func Render(a *v1alpha1.Appliance, node Node, secrets Secrets) ([]config.Document, error) {
	spec := a.Spec
	if spec.Node.HostName == "" {
		return nil, errors.New("the Appliance has no host name")
	}
	docs := []config.Document{HostnameConfig(spec.Node.HostName)}

	link, err := gatewayLink(spec.Node.Gateway, node.Link)
	if err != nil {
		return nil, err
	}
	if link != nil {
		docs = append(docs, link)
	}

	dns := make([]netip.Addr, 0, len(spec.DNS.Servers))
	for _, s := range spec.DNS.Servers {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("DNS server %q is not an IP address", s)
		}
		dns = append(dns, addr)
	}
	docs = append(docs, ResolverConfig(dns), TimeSyncConfig(spec.NTP.Servers))

	if env := EnvironmentConfig(ProxyOf(spec.Proxy), node.IP); env != nil {
		docs = append(docs, env)
	}
	if secrets.TrustedRoots != "" {
		docs = append(docs, TrustedRootsConfig(secrets.TrustedRoots))
	}
	rendered := map[string]bool{}
	for _, r := range secrets.Registries {
		// A host listed twice would give two documents with the same name, which the node refuses.
		if !rendered[r.Host] {
			rendered[r.Host] = true
			docs = append(docs, RegistryAuthConfig(r))
		}
	}
	if node.Registry != nil {
		docs = append(docs, RegistryMirrors(node.Registry.CA, node.Registry.Upstreams)...)
	}
	return docs, nil
}

// gatewayLink returns the static link with the default route through gateway, or nil with DHCP, where a gateway
// cannot be set.
func gatewayLink(gateway string, link *network.LinkConfigV1Alpha1) (*network.LinkConfigV1Alpha1, error) {
	var gw netip.Addr
	if gateway != "" {
		var err error
		if gw, err = netip.ParseAddr(gateway); err != nil {
			return nil, fmt.Errorf("gateway %q is not an IP address", gateway)
		}
	}
	switch {
	case link != nil:
		return WithGateway(link, gw), nil
	case gw.IsValid():
		return nil, errors.New("a gateway needs a static address, and the node has none")
	}
	return nil, nil
}

// ProxyOf returns the proxy of p, or nil when p sets nothing.
func ProxyOf(p *v1alpha1.ProxySpec) *Proxy {
	if p == nil || (p.HTTP == "" && p.HTTPS == "" && p.NoProxy == "") {
		return nil
	}
	return &Proxy{HTTP: p.HTTP, HTTPS: p.HTTPS, NoProxy: p.NoProxy}
}

// StaticLink returns the first link configuration of docs that holds an address, or nil when there is none.
func StaticLink(docs []config.Document) *network.LinkConfigV1Alpha1 {
	for _, doc := range docs {
		if link, ok := doc.(*network.LinkConfigV1Alpha1); ok && len(link.LinkAddresses) > 0 {
			return link
		}
	}
	return nil
}
