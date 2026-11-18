package machineconfig

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"
	"github.com/siderolabs/talos/pkg/machinery/config/validation"
)

func staticInput() Input {
	in := testInput()
	in.NodeIP = "192.0.2.10"
	in.HostName = "ilm.example.com"
	in.Link = "enp0s1"
	in.Address = netip.MustParsePrefix("192.0.2.10/24")
	in.Gateway = netip.MustParseAddr("192.0.2.1")
	in.DNS = []netip.Addr{netip.MustParseAddr("192.0.2.53"), netip.MustParseAddr("2001:db8::53")}
	in.NTP = []string{"ntp.example.com", "192.0.2.123"}
	in.Proxy = &Proxy{HTTP: "http://proxy.example.com:3128", HTTPS: "http://proxy.example.com:3128", NoProxy: "localhost,.example.com"}
	in.Handover = []byte(`{"version":1}`)
	return in
}

func TestGenerateStaticNetwork(t *testing.T) {
	out := mustGenerate(t, staticInput())
	cfg, err := configloader.NewFromBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ValidateAsClient(metalMode{}, validation.WithStrict()); err != nil {
		t.Fatalf("configuration does not validate: %v", err)
	}

	found := map[string]int{}
	for _, doc := range cfg.Documents() {
		found[doc.Kind()]++
		switch d := doc.(type) {
		case *network.HostnameConfigV1Alpha1:
			if d.ConfigHostname != "ilm.example.com" || d.ConfigAuto == nil || *d.ConfigAuto != 0 {
				t.Errorf("host name %q, auto %v; want ilm.example.com without an automatic name", d.ConfigHostname, d.ConfigAuto)
			}
		case *network.LinkConfigV1Alpha1:
			if d.MetaName != "enp0s1" || len(d.LinkAddresses) != 1 || d.LinkAddresses[0].AddressAddress.String() != "192.0.2.10/24" {
				t.Errorf("link %q addresses %v", d.MetaName, d.LinkAddresses)
			}
			if len(d.LinkRoutes) != 1 || d.LinkRoutes[0].RouteGateway.String() != "192.0.2.1" || d.LinkRoutes[0].RouteDestination.IsValid() {
				t.Errorf("routes %v, want one default route through 192.0.2.1", d.LinkRoutes)
			}
		case *network.ResolverConfigV1Alpha1:
			var got []string
			for _, ns := range d.ResolverNameservers {
				got = append(got, ns.Address.String())
			}
			if !slices.Equal(got, []string{"192.0.2.53", "2001:db8::53"}) {
				t.Errorf("name servers %v", got)
			}
			if d.ResolverHostDNS.HostDNSEnabled == nil || !*d.ResolverHostDNS.HostDNSEnabled {
				t.Error("host DNS was dropped")
			}
		case *network.TimeSyncConfigV1Alpha1:
			if d.TimeBootTimeout != 30*time.Second || d.TimeNTP == nil || !slices.Equal(d.TimeNTP.Servers, []string{"ntp.example.com", "192.0.2.123"}) {
				t.Errorf("time sync boot timeout %s, NTP %v", d.TimeBootTimeout, d.TimeNTP)
			}
		case *runtime.EnvironmentV1Alpha1:
			want := map[string]string{
				"HTTP_PROXY": "http://proxy.example.com:3128", "HTTPS_PROXY": "http://proxy.example.com:3128",
				// The node, the pod and service networks and the cluster names never go through the proxy.
				"NO_PROXY": "localhost,127.0.0.1,192.0.2.10,10.244.0.0/16,10.96.0.0/12,.svc,.cluster.local,localhost,.example.com",
			}
			for k, v := range want {
				if d.EnvironmentVariables[k] != v {
					t.Errorf("%s = %q, want %q", k, d.EnvironmentVariables[k], v)
				}
			}
		case *extensions.ServiceConfigV1Alpha1:
			if d.ServiceName != "firstboot" || len(d.ServiceConfigFiles) != 1 ||
				d.ServiceConfigFiles[0].ConfigFileMountPath != HandoverPath || d.ServiceConfigFiles[0].ConfigFileContent != `{"version":1}` {
				t.Errorf("extension service config %q files %+v", d.ServiceName, d.ServiceConfigFiles)
			}
		}
	}
	for _, kind := range []string{"HostnameConfig", "LinkConfig", "ResolverConfig", "TimeSyncConfig", "EnvironmentConfig", "ExtensionServiceConfig"} {
		if found[kind] != 1 {
			t.Errorf("%d %s documents, want 1", found[kind], kind)
		}
	}
	if !strings.Contains(string(out), "https://192.0.2.10:6443") {
		t.Error("the cluster endpoint is not the static address")
	}

}

func TestGenerateKeepsDHCP(t *testing.T) {
	in := staticInput()
	in.Address, in.Gateway, in.Link, in.Proxy, in.NTP = netip.Prefix{}, netip.Addr{}, "", nil, nil
	out := mustGenerate(t, in)
	cfg, err := configloader.NewFromBytes(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range cfg.Documents() {
		switch d := doc.(type) {
		case *network.LinkConfigV1Alpha1, *runtime.EnvironmentV1Alpha1:
			t.Errorf("unexpected %s document with DHCP and no proxy", doc.Kind())
		case *network.TimeSyncConfigV1Alpha1:
			if d.TimeNTP != nil && len(d.TimeNTP.Servers) != 0 {
				t.Errorf("NTP servers %v without any chosen", d.TimeNTP.Servers)
			}
		}
	}
	if !strings.Contains(string(out), "hostname: ilm.example.com") {
		t.Error("host name missing")
	}
}

func TestGenerateRejectsInconsistentNetwork(t *testing.T) {
	for name, edit := range map[string]func(*Input){
		"static without link":         func(in *Input) { in.Link = "" },
		"static without gateway":      func(in *Input) { in.Gateway = netip.Addr{} },
		"node IP is not the address":  func(in *Input) { in.NodeIP = "192.0.2.11" },
		"gateway outside the network": func(in *Input) { in.Gateway = netip.MustParseAddr("198.51.100.1") },
	} {
		in := staticInput()
		edit(&in)
		if _, err := Generate(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
