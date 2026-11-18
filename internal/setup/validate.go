package setup

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/OmniTrustILM/appliance-ng/internal/api/setupapi"
	"github.com/OmniTrustILM/appliance-ng/internal/hostname"
	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
)

const (
	maxServers = 8
	label      = `[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?`
)

var (
	hostName  = regexp.MustCompile(`^` + label + `(\.` + label + `)*$`)
	noProxyRE = regexp.MustCompile(`^[A-Za-z0-9.*:/_\[\]-]+$`)
)

// fieldError names the request field that failed validation.
type fieldError struct{ field, message string }

// parseNode validates the node settings and returns them as hand-over answers and as machine configuration input.
func parseNode(s setupapi.NodeSettings) (NodeAnswers, machineconfig.Input, *fieldError) {
	var (
		answers NodeAnswers
		in      machineconfig.Input
	)

	answers.HostName = asciiLower(strings.TrimSpace(s.HostName))
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

	if len(s.Dns) == 0 || len(s.Dns) > maxServers {
		return answers, in, &fieldError{"dns", "Enter one to eight DNS servers."}
	}
	for _, v := range s.Dns {
		addr, err := netip.ParseAddr(strings.TrimSpace(v))
		if err != nil || !addr.IsGlobalUnicast() {
			return answers, in, &fieldError{"dns", "Enter the DNS servers as IP addresses."}
		}
		in.DNS, answers.DNS = append(in.DNS, addr), append(answers.DNS, addr.String())
	}

	if len(s.Ntp) > maxServers {
		return answers, in, &fieldError{"ntp", "Enter up to eight NTP servers."}
	}
	for _, v := range s.Ntp {
		server, ok := parseServer(v)
		if !ok {
			return answers, in, &fieldError{"ntp", "Enter the NTP servers as host names or IP addresses."}
		}
		in.NTP = append(in.NTP, server)
	}
	answers.NTP = in.NTP

	if answers.Proxy, ferr = parseProxy(s.Proxy); ferr != nil {
		return answers, in, ferr
	}
	if answers.Proxy != nil {
		in.Proxy = &machineconfig.Proxy{HTTP: answers.Proxy.HTTP, HTTPS: answers.Proxy.HTTPS, NoProxy: answers.Proxy.NoProxy}
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
	network, broadcast := prefix.Masked().Addr(), lastAddr(prefix)
	if prefix.Addr() == network || prefix.Addr() == broadcast {
		return prefix, netip.Addr{}, &fieldError{"address", "The address is the network or broadcast address of its subnet."}
	}
	gw, err := netip.ParseAddr(gateway)
	if err != nil || !prefix.Masked().Contains(gw) || gw == prefix.Addr() || gw == network || gw == broadcast {
		return prefix, gw, &fieldError{"gateway", "Enter a gateway address in the subnet of the node address."}
	}
	return prefix, gw, nil
}

// lastAddr returns the last address of the IPv4 prefix p.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// parseServer validates a server given as a global IP address or a host name.
func parseServer(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if addr, err := netip.ParseAddr(v); err == nil {
		return addr.String(), addr.IsGlobalUnicast()
	}
	v = asciiLower(v)
	return v, len(v) <= 253 && hostName.MatchString(v)
}

// parseProxy validates the proxy URLs and the no-proxy list; a proxy without any value is none.
func parseProxy(p *setupapi.Proxy) (*Proxy, *fieldError) {
	if p == nil {
		return nil, nil
	}
	out := &Proxy{}
	for _, f := range []struct {
		in  *string
		out *string
	}{{p.Http, &out.HTTP}, {p.Https, &out.HTTPS}} {
		if f.in == nil || strings.TrimSpace(*f.in) == "" {
			continue
		}
		proxyURL, bad := parseProxyURL(*f.in)
		if bad != nil {
			return nil, bad
		}
		*f.out = proxyURL
	}
	if p.NoProxy != nil {
		list, bad := parseNoProxy(*p.NoProxy)
		if bad != nil {
			return nil, bad
		}
		out.NoProxy = list
	}
	if *out == (Proxy{}) {
		return nil, nil
	}
	return out, nil
}

// parseProxyURL validates a proxy URL: http or https, a host, no credentials, no path, query or fragment.
func parseProxyURL(v string) (string, *fieldError) {
	u, err := url.Parse(strings.TrimSpace(v))
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return "", &fieldError{"proxy", "Enter the proxy as a URL, such as http://proxy.example.com:3128."}
	case u.User != nil:
		return "", &fieldError{"proxy", "Proxy credentials are not supported."}
	}
	return u.String(), nil
}

// parseNoProxy validates a comma-separated no-proxy list and returns it without blank entries.
func parseNoProxy(v string) (string, *fieldError) {
	var entries []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if !noProxyRE.MatchString(e) {
			return "", &fieldError{"proxy", "Enter the no-proxy list as host names, domains and networks separated by commas."}
		}
		entries = append(entries, e)
	}
	return strings.Join(entries, ","), nil
}

// asciiLower lower-cases the ASCII letters of s only.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return r
	}, s)
}
