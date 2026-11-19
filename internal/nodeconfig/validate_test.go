package nodeconfig_test

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
)

func TestParseGateway(t *testing.T) {
	prefix := netip.MustParsePrefix("192.0.2.10/24")
	for gateway, ok := range map[string]bool{
		" 192.0.2.1 ": true, "192.0.2.254": true,
		"192.0.2.10": false, "192.0.2.0": false, "192.0.2.255": false, "198.51.100.1": false, "gateway": false,
	} {
		gw, bad := nodeconfig.ParseGateway(prefix, gateway)
		if (bad == "") != ok {
			t.Errorf("ParseGateway(%q) = %s, %q; want accepted %v", gateway, gw, bad, ok)
		}
	}
}

func TestParseDNS(t *testing.T) {
	got, bad := nodeconfig.ParseDNS([]string{" 192.0.2.53", "2001:db8::53"})
	if bad != "" || len(got) != 2 || got[0].String() != "192.0.2.53" {
		t.Errorf("ParseDNS = %v, %q", got, bad)
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "192.0.2.53"
	}
	for _, servers := range [][]string{nil, nine, {"dns.example.com"}, {"127.0.0.1"}, {"2001:db8::53%eth0"}} {
		if _, bad := nodeconfig.ParseDNS(servers); bad == "" {
			t.Errorf("ParseDNS(%v) accepted", servers)
		}
	}
}

func TestParseNTP(t *testing.T) {
	got, bad := nodeconfig.ParseNTP([]string{" NTP.Example.com ", "192.0.2.123"})
	if bad != "" || !slices.Equal(got, []string{"ntp.example.com", "192.0.2.123"}) {
		t.Errorf("ParseNTP = %v, %q", got, bad)
	}
	if got, bad := nodeconfig.ParseNTP(nil); bad != "" || got != nil {
		t.Errorf("ParseNTP(nil) = %v, %q; want none", got, bad)
	}
	nine := make([]string, 9)
	for i := range nine {
		nine[i] = "ntp.example.com"
	}
	for _, servers := range [][]string{nine, {"ntp_example"}, {"-bad.example.com"}, {"169.254.1.1"}, {"2001:db8::123%eth0"}} {
		if _, bad := nodeconfig.ParseNTP(servers); bad == "" {
			t.Errorf("ParseNTP(%v) accepted", servers)
		}
	}
}

func TestParseProxy(t *testing.T) {
	got, bad := nodeconfig.ParseProxy(" http://proxy.example.com:3128 ", "", " localhost, ,.example.com,10.0.0.0/8 ")
	want := nodeconfig.Proxy{HTTP: "http://proxy.example.com:3128", NoProxy: "localhost,.example.com,10.0.0.0/8"}
	if bad != "" || got == nil || *got != want {
		t.Errorf("ParseProxy = %+v, %q; want %+v", got, bad, want)
	}
	if got, bad := nodeconfig.ParseProxy(" ", "", " , "); got != nil || bad != "" {
		t.Errorf("ParseProxy of blanks = %+v, %q; want none", got, bad)
	}
	for _, in := range [][3]string{
		{"ftp://proxy.example.com", "", ""},
		{"", "http://user:pw@proxy.example.com", ""},
		{"http://proxy.example.com/path", "", ""},
		{"http://proxy.example.com", "", "a b"},
	} {
		if got, bad := nodeconfig.ParseProxy(in[0], in[1], in[2]); bad == "" {
			t.Errorf("ParseProxy(%q) = %+v, accepted", in, got)
		}
	}
}
