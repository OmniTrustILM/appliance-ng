package talos

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Network is the network setup the node runs with for one of its addresses, as DHCP or the Talos defaults set it up.
// A value the node does not report is zero.
type Network struct {
	// Link is the interface that holds the address, and Address the address with its prefix length.
	Link    string
	Address netip.Prefix
	// Gateway is the IPv4 default gateway through Link.
	Gateway netip.Addr
	// DNS lists the name servers in use, without link-local addresses.
	DNS []netip.Addr
	// NTP lists the time servers in use.
	NTP []string
	// HostName is the fully qualified host name; a name without a domain is left out.
	HostName string
}

// Network reports the network setup of the node for addr.
func (n *node) Network(ctx context.Context, addr netip.Addr) (Network, error) {
	var nw Network

	if err := n.readNameServices(ctx, &nw); err != nil {
		return nw, err
	}

	addrs, err := safe.StateListAll[*network.AddressStatus](ctx, n.st)
	if err != nil {
		return nw, fmt.Errorf("read link addresses: %w", err)
	}
	for a := range addrs.All() {
		if a.TypedSpec().Address.Addr() == addr {
			nw.Link, nw.Address = a.TypedSpec().LinkName, a.TypedSpec().Address
			break
		}
	}
	if nw.Link == "" {
		return nw, nil
	}

	gateway, err := n.defaultGateway(ctx, nw.Link)
	if err != nil {
		return nw, err
	}
	nw.Gateway = gateway

	return nw, nil
}

// readNameServices fills the name servers, time servers and host name of nw.
func (n *node) readNameServices(ctx context.Context, nw *Network) error {
	resolvers, ok, err := getOptional[*network.ResolverStatus](ctx, n.st, network.ResolverID)
	if err != nil {
		return err
	}
	if ok {
		for _, ns := range resolvers.TypedSpec().NameServers {
			if !ns.Addr.IsLinkLocalUnicast() {
				nw.DNS = append(nw.DNS, ns.Addr)
			}
		}
	}

	times, ok, err := getOptional[*network.TimeServerStatus](ctx, n.st, network.TimeServerID)
	if err != nil {
		return err
	}
	if ok {
		nw.NTP = times.TypedSpec().NTPServers
	}

	host, ok, err := getOptional[*network.HostnameStatus](ctx, n.st, network.HostnameID)
	if err != nil {
		return err
	}
	if ok && host.TypedSpec().Domainname != "" {
		nw.HostName = host.TypedSpec().FQDN()
	}
	return nil
}

// defaultGateway returns the IPv4 default gateway through link with the lowest priority, or the zero address.
func (n *node) defaultGateway(ctx context.Context, link string) (netip.Addr, error) {
	routes, err := safe.StateListAll[*network.RouteStatus](ctx, n.st)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read routes: %w", err)
	}
	var gateway netip.Addr
	var best uint32
	for r := range routes.All() {
		spec := r.TypedSpec()
		isDefault := spec.Table == nethelpers.TableMain && spec.Family == nethelpers.FamilyInet4 &&
			spec.Destination.Bits() <= 0 && spec.Gateway.Is4()
		if isDefault && spec.OutLinkName == link && (!gateway.IsValid() || spec.Priority < best) {
			gateway, best = spec.Gateway, spec.Priority
		}
	}
	return gateway, nil
}

// getOptional reads the network resource id of type T; a missing resource is the zero T.
func getOptional[T meta.ResourceWithRD](ctx context.Context, st state.CoreState, id resource.ID) (T, bool, error) {
	res, err := safe.StateGetByID[T](ctx, st, id)
	if state.IsNotFoundError(err) || status.Code(err) == codes.NotFound {
		var zero T
		return zero, false, nil
	}
	if err != nil {
		return res, false, fmt.Errorf("read %s: %w", id, err)
	}

	return res, true, nil
}
