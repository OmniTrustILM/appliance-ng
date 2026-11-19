package nodeconfig

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// The validators return the message to show for the field when a value is refused, or "" when it is accepted.

// maxServers is the most name servers and time servers the node takes.
const maxServers = 8

const label = `[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?`

var (
	serverName = regexp.MustCompile(`^` + label + `(\.` + label + `)*$`)
	noProxyRE  = regexp.MustCompile(`^[A-Za-z0-9.*:/_\[\]-]+$`)
)

// ParseGateway validates the gateway of the static address prefix: an address in its subnet that is neither the
// node address nor the network or broadcast address.
func ParseGateway(prefix netip.Prefix, gateway string) (netip.Addr, string) {
	network, broadcast := prefix.Masked().Addr(), LastAddr(prefix)
	gw, err := netip.ParseAddr(strings.TrimSpace(gateway))
	if err != nil || !prefix.Masked().Contains(gw) || gw == prefix.Addr() || gw == network || gw == broadcast {
		return gw, "Enter a gateway address in the subnet of the node address."
	}
	return gw, ""
}

// LastAddr returns the last address of the IPv4 prefix p, its broadcast address.
func LastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// ParseDNS validates the name servers, at least one and at most maxServers, given as global IP addresses.
func ParseDNS(servers []string) ([]netip.Addr, string) {
	if len(servers) == 0 || len(servers) > maxServers {
		return nil, "Enter one to eight DNS servers."
	}
	out := make([]netip.Addr, 0, len(servers))
	for _, v := range servers {
		addr, err := netip.ParseAddr(strings.TrimSpace(v))
		if err != nil || !globalAddress(addr) {
			return nil, "Enter the DNS servers as IP addresses."
		}
		out = append(out, addr)
	}
	return out, ""
}

// ParseNTP validates up to maxServers time servers given as global IP addresses or host names.
func ParseNTP(servers []string) ([]string, string) {
	if len(servers) > maxServers {
		return nil, "Enter up to eight NTP servers."
	}
	var out []string
	for _, v := range servers {
		server, ok := parseServer(v)
		if !ok {
			return nil, "Enter the NTP servers as host names or IP addresses."
		}
		out = append(out, server)
	}
	return out, ""
}

// parseServer validates a server given as a global IP address or a host name.
func parseServer(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if addr, err := netip.ParseAddr(v); err == nil {
		return addr.String(), globalAddress(addr)
	}
	v = ASCIILower(v)
	return v, len(v) <= 253 && serverName.MatchString(v)
}

// globalAddress reports whether addr is a global unicast address without a zone, which only names a local link.
func globalAddress(addr netip.Addr) bool {
	return addr.IsGlobalUnicast() && addr.Zone() == ""
}

// ParseProxy validates the proxy URLs and the no-proxy list; a proxy without any value is none, nil.
func ParseProxy(httpURL, httpsURL, noProxy string) (*Proxy, string) {
	out := &Proxy{}
	for _, f := range []struct {
		in  string
		out *string
	}{{httpURL, &out.HTTP}, {httpsURL, &out.HTTPS}} {
		if strings.TrimSpace(f.in) == "" {
			continue
		}
		proxyURL, bad := parseProxyURL(f.in)
		if bad != "" {
			return nil, bad
		}
		*f.out = proxyURL
	}
	list, bad := parseNoProxy(noProxy)
	if bad != "" {
		return nil, bad
	}
	out.NoProxy = list
	if *out == (Proxy{}) {
		return nil, ""
	}
	return out, ""
}

// parseProxyURL validates a proxy URL: http or https, a host, no credentials, no path, query or fragment.
func parseProxyURL(v string) (string, string) {
	u, err := url.Parse(strings.TrimSpace(v))
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
		return "", "Enter the proxy as a URL, such as http://proxy.example.com:3128."
	case u.User != nil:
		return "", "Proxy credentials are not supported."
	}
	return u.String(), ""
}

// parseNoProxy validates a comma-separated no-proxy list and returns it without blank entries.
func parseNoProxy(v string) (string, string) {
	var entries []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if !noProxyRE.MatchString(e) {
			return "", "Enter the no-proxy list as host names, domains and networks separated by commas."
		}
		entries = append(entries, e)
	}
	return strings.Join(entries, ","), ""
}

// ASCIILower lower-cases the ASCII letters of s only.
func ASCIILower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return r
	}, s)
}
