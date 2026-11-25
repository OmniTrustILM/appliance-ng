package certs

import (
	"slices"
	"testing"
)

func TestDNS01Providers(t *testing.T) {
	var names []string
	for name := range DNS01Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	want := []string{"acmedns", "akamai", "azuredns", "clouddns", "cloudflare", "digitalocean", "rfc2136", "route53"}
	if !slices.Equal(names, want) {
		t.Errorf("providers %v, want %v", names, want)
	}
}

func TestValidateDNS01(t *testing.T) {
	if err := ValidateDNS01("route53", map[string]string{"region": "eu-west-1", "accessKeyID": "id"}, map[string]string{"secretAccessKey": "k"}); err != nil {
		t.Errorf("valid settings refused: %v", err)
	}
	if err := ValidateDNS01("route53", map[string]string{"accessKeyID": "id"}, nil); err == nil {
		t.Error("a missing secretAccessKey was accepted")
	}
	if err := ValidateDNS01("route53", map[string]string{"region": "eu-west-1"}, map[string]string{"secretAccessKey": "k"}); err == nil {
		t.Error("a missing accessKeyID was accepted")
	}
}
