package setup

import (
	"net/netip"
	"strings"

	"github.com/OmniTrustILM/appliance-ng/internal/api/setupapi"
	"github.com/OmniTrustILM/appliance-ng/internal/hostname"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
)

// fieldError names the request field that failed validation.
type fieldError struct{ field, message string }

// parseNode validates the node settings and returns them as hand-over answers and as machine configuration input.
func parseNode(s setupapi.NodeSettings) (NodeAnswers, machineconfig.Input, *fieldError) {
	var (
		answers NodeAnswers
		in      machineconfig.Input
	)

	answers.HostName = nodeconfig.ASCIILower(strings.TrimSpace(s.HostName))
	if !hostname.IsFQDN(answers.HostName) {
		return answers, in, &fieldError{"hostName", "Enter a fully qualified host name, such as ilm.example.com, not an IP address."}
	}
	in.HostName = answers.HostName

	var ferr *fieldError
	if in.Address, in.Gateway, ferr = parseStatic(strings.TrimSpace(s.Address), strings.TrimSpace(s.Gateway)); ferr != nil {
		return answers, in, ferr
	}
	if in.Address.IsValid() {
		answers.Address, answers.Gateway = in.Address.String(), in.Gateway.String()
	}

	var bad string
	if in.DNS, bad = nodeconfig.ParseDNS(s.Dns); bad != "" {
		return answers, in, &fieldError{"dns", bad}
	}
	for _, addr := range in.DNS {
		answers.DNS = append(answers.DNS, addr.String())
	}

	if in.NTP, bad = nodeconfig.ParseNTP(s.Ntp); bad != "" {
		return answers, in, &fieldError{"ntp", bad}
	}
	answers.NTP = in.NTP

	if in.Proxy, ferr = parseProxy(s.Proxy); ferr != nil {
		return answers, in, ferr
	}
	if p := in.Proxy; p != nil {
		answers.Proxy = &Proxy{HTTP: p.HTTP, HTTPS: p.HTTPS, NoProxy: p.NoProxy}
	}
	return answers, in, nil
}

// parseStatic validates a static IPv4 address in CIDR notation and its gateway; both empty keep DHCP.
func parseStatic(address, gateway string) (netip.Prefix, netip.Addr, *fieldError) {
	if address == "" {
		if gateway != "" {
			return netip.Prefix{}, netip.Addr{}, &fieldError{"gateway", "A gateway needs a static address."}
		}
		return netip.Prefix{}, netip.Addr{}, nil
	}
	prefix, err := netip.ParsePrefix(address)
	if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsGlobalUnicast() || prefix.Bits() < 8 || prefix.Bits() > 30 {
		return prefix, netip.Addr{}, &fieldError{"address", "Enter an IPv4 address in CIDR notation, such as 192.0.2.10/24."}
	}
	if network, broadcast := prefix.Masked().Addr(), nodeconfig.LastAddr(prefix); prefix.Addr() == network || prefix.Addr() == broadcast {
		return prefix, netip.Addr{}, &fieldError{"address", "The address is the network or broadcast address of its subnet."}
	}
	gw, bad := nodeconfig.ParseGateway(prefix, gateway)
	if bad != "" {
		return prefix, gw, &fieldError{"gateway", bad}
	}
	return prefix, gw, nil
}

// parseProxy validates the proxy of the request; a proxy without any value is none.
func parseProxy(p *setupapi.Proxy) (*machineconfig.Proxy, *fieldError) {
	if p == nil {
		return nil, nil
	}
	out, bad := nodeconfig.ParseProxy(deref(p.Http), deref(p.Https), deref(p.NoProxy))
	if bad != "" {
		return nil, &fieldError{"proxy", bad}
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
