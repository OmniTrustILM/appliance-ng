package certs

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Hosts returns the host names the appliance's certificate covers: the node host name and the ILM host name,
// which defaults to it.
func Hosts(a *v1alpha1.Appliance) []string {
	hosts := []string{a.Spec.Node.HostName}
	if p := a.Spec.Platform; p != nil && p.HostName != "" && p.HostName != a.Spec.Node.HostName {
		hosts = append(hosts, p.HostName)
	}
	return hosts
}

// UncoveredHost returns the first of hosts that the first certificate of certPEM does not cover, or "" when it
// covers them all or certPEM holds no readable certificate.
func UncoveredHost(certPEM []byte, hosts []string) string {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return ""
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return h
		}
	}
	return ""
}

// NodeAddress returns the node address from the status, the spec, or with DHCP the internal address of the
// manager's node; the certificate covers it as an IP address.
func NodeAddress(ctx context.Context, c client.Reader, a *v1alpha1.Appliance) (netip.Addr, bool, error) {
	for _, s := range []string{a.Status.NodeAddress, a.Spec.Node.Address} {
		if addr, ok := ParseAddress(s); ok {
			return addr, true, nil
		}
	}
	node, err := kube.OwnNode(ctx, c)
	if err != nil || node == nil {
		return netip.Addr{}, false, err
	}
	addr, ok := InternalAddress(node)
	return addr, ok, nil
}

// InternalAddress returns the first internal address of n that ParseAddress accepts.
func InternalAddress(n *corev1.Node) (netip.Addr, bool) {
	for _, address := range n.Status.Addresses {
		if address.Type != corev1.NodeInternalIP {
			continue
		}
		if addr, ok := ParseAddress(address.Address); ok {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// ParseAddress accepts an address with or without a prefix length, and only a global unicast IPv4 address.
func ParseAddress(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		prefix, perr := netip.ParsePrefix(s)
		if perr != nil {
			return netip.Addr{}, false
		}
		addr = prefix.Addr()
	}
	return addr, addr.Is4() && addr.IsGlobalUnicast()
}
