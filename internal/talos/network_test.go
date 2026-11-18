package talos

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
)

func TestNetwork(t *testing.T) {
	st := newState(t)
	addr := func(id, link, prefix string) resource.Resource {
		a := network.NewAddressStatus(network.NamespaceName, id)
		a.TypedSpec().LinkName, a.TypedSpec().Address = link, netip.MustParsePrefix(prefix)
		return a
	}
	route := func(id, link, dst, gw string, family nethelpers.Family, priority uint32) resource.Resource {
		r := network.NewRouteStatus(network.NamespaceName, id)
		spec := r.TypedSpec()
		spec.OutLinkName, spec.Family, spec.Priority, spec.Table = link, family, priority, nethelpers.TableMain
		if dst != "" {
			spec.Destination = netip.MustParsePrefix(dst)
		}
		spec.Gateway = netip.MustParseAddr(gw)
		return r
	}
	resolvers := network.NewResolverStatus(network.NamespaceName, network.ResolverID)
	resolvers.TypedSpec().NameServers = []network.NameServerSpec{{Addr: netip.MustParseAddr("10.0.2.3")}, {Addr: netip.MustParseAddr("fe80::1")}}
	times := network.NewTimeServerStatus(network.NamespaceName, network.TimeServerID)
	times.TypedSpec().NTPServers = []string{"ntp.example.com"}
	host := network.NewHostnameStatus(network.NamespaceName, network.HostnameID)
	host.TypedSpec().Hostname, host.TypedSpec().Domainname = "ilm", "example.com"
	// A default route of another routing table is not the node's gateway, whatever its priority.
	otherTable := route("eth0/default-table100", "eth0", "", "10.0.2.99", nethelpers.FamilyInet4, 1)
	otherTable.(*network.RouteStatus).TypedSpec().Table = nethelpers.RoutingTable(100)

	for _, r := range []resource.Resource{
		addr("eth0/10.0.2.15/24", "eth0", "10.0.2.15/24"),
		addr("eth1/192.0.2.5/24", "eth1", "192.0.2.5/24"),
		route("eth1/default", "eth1", "", "192.0.2.1", nethelpers.FamilyInet4, 100),
		route("eth0/net", "eth0", "10.0.2.0/24", "10.0.2.9", nethelpers.FamilyInet4, 1),
		route("eth0/default-high", "eth0", "", "10.0.2.254", nethelpers.FamilyInet4, 2048),
		route("eth0/default", "eth0", "", "10.0.2.2", nethelpers.FamilyInet4, 1024),
		route("eth0/default6", "eth0", "", "2001:db8::1", nethelpers.FamilyInet6, 1),
		otherTable,
		resolvers, times, host,
	} {
		if err := st.Create(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := (&node{st: st}).Network(context.Background(), netip.MustParseAddr("10.0.2.15"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Link != "eth0" || got.Address != netip.MustParsePrefix("10.0.2.15/24") || got.Gateway != netip.MustParseAddr("10.0.2.2") {
		t.Errorf("link %q address %s gateway %s, want eth0 10.0.2.15/24 10.0.2.2", got.Link, got.Address, got.Gateway)
	}
	if !slices.Equal(got.DNS, []netip.Addr{netip.MustParseAddr("10.0.2.3")}) {
		t.Errorf("DNS %v, want 10.0.2.3 without the link-local server", got.DNS)
	}
	if !slices.Equal(got.NTP, []string{"ntp.example.com"}) || got.HostName != "ilm.example.com" {
		t.Errorf("NTP %v host name %q", got.NTP, got.HostName)
	}
}

func TestNetworkWithoutStatus(t *testing.T) {
	got, err := (&node{st: newState(t)}).Network(context.Background(), netip.MustParseAddr("10.0.2.15"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Link != "" || got.Gateway.IsValid() || got.DNS != nil || got.NTP != nil || got.HostName != "" {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestNetworkHostNameWithoutDomain(t *testing.T) {
	st := newState(t)
	host := network.NewHostnameStatus(network.NamespaceName, network.HostnameID)
	host.TypedSpec().Hostname = "talos-abc"
	if err := st.Create(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	got, err := (&node{st: st}).Network(context.Background(), netip.MustParseAddr("10.0.2.15"))
	if err != nil || got.HostName != "" {
		t.Errorf("host name %q, %v; want none for a name without a domain", got.HostName, err)
	}
}

func TestNetworkError(t *testing.T) {
	if _, err := (&node{st: failingState{newState(t)}}).Network(context.Background(), netip.MustParseAddr("10.0.2.15")); err == nil {
		t.Fatal("expected error")
	}
}
