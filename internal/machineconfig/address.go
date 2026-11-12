package machineconfig

import "net/netip"

// usableNodeAddress reports whether addr may be the node address: a global unicast IPv4 address
// (not loopback, link-local, multicast or unspecified).
func usableNodeAddress(addr netip.Addr) bool {
	return addr.Is4() && addr.IsGlobalUnicast()
}

// PickAddress returns the first global unicast IPv4 address in addrs.
func PickAddress(addrs []netip.Prefix) (netip.Addr, bool) {
	for _, prefix := range addrs {
		if addr := prefix.Addr(); usableNodeAddress(addr) {
			return addr, true
		}
	}

	return netip.Addr{}, false
}
