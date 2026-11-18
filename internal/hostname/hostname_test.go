package hostname

import (
	"strings"
	"testing"
)

func TestIsFQDN(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want bool
	}{
		"two labels":         {"ilm.example", true},
		"three labels":       {"ilm.example.com", true},
		"hyphens and digits": {"ilm-1.example2.com", true},
		"63-character label": {strings.Repeat("a", 63) + ".example.com", true},
		"253 characters":     {strings.Repeat(strings.Repeat("a", 49)+".", 5) + "com", true},
		"single label":       {"ilm", false},
		"empty":              {"", false},
		"IPv4 address":       {"10.0.2.15", false},
		"IPv6 address":       {"2001:db8::1", false},
		"64-character label": {strings.Repeat("a", 64) + ".example.com", false},
		"254 characters":     {strings.Repeat(strings.Repeat("a", 49)+".", 5) + "coms", false},
		"leading hyphen":     {"-ilm.example.com", false},
		"trailing hyphen":    {"ilm-.example.com", false},
		"underscore":         {"ilm_1.example.com", false},
		"empty label":        {"ilm..example.com", false},
		"trailing dot":       {"ilm.example.com.", false},
		"upper case":         {"ILM.example.com", false},
	} {
		if got := IsFQDN(tc.in); got != tc.want {
			t.Errorf("%s: IsFQDN(%q) = %v, want %v", name, tc.in, got, tc.want)
		}
	}
}
