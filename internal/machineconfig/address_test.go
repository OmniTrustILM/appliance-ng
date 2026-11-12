package machineconfig

import (
	"net/netip"
	"testing"
)

func TestPickAddress(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		want  string
	}{
		{"link-local first", []string{"169.254.1.1/16", "10.0.2.15/24"}, "10.0.2.15"},
		{"ipv6 only", []string{"fe80::1/64", "2001:db8::5/64"}, ""},
		{"empty", nil, ""},
		{"first global ipv4", []string{"127.0.0.1/8", "192.168.1.5/24", "10.0.0.9/8"}, "192.168.1.5"},
		{"unspecified", []string{"0.0.0.0/0"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var prefixes []netip.Prefix
			for _, a := range tt.addrs {
				prefixes = append(prefixes, netip.MustParsePrefix(a))
			}

			got, ok := PickAddress(prefixes)
			if tt.want == "" {
				if ok {
					t.Fatalf("PickAddress() = %s, want none", got)
				}

				return
			}

			if !ok || got != netip.MustParseAddr(tt.want) {
				t.Fatalf("PickAddress() = %s, %v; want %s", got, ok, tt.want)
			}
		})
	}
}
