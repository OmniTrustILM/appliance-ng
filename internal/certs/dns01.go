package certs

import (
	"fmt"
	"slices"
)

const (
	// ACMEEABSecretName is the Secret, in the system namespace, that holds the HMAC key of a custom directory's
	// external account binding under the key ACMEEABHMACKey.
	ACMEEABSecretName = "appliance-acme-eab"
	// ACMEILMEABSecretName is the Secret, in the system namespace, of the ILM profile's binding: its HMAC key under
	// ACMEEABHMACKey and the UUID of ILM's secret that holds the same key, which is the binding's key ID, under
	// ACMEEABKeyIDKey.
	ACMEILMEABSecretName = "appliance-acme-ilm-eab"
	// ACMEEABHMACKey holds the binding's HMAC key, base64url-encoded.
	ACMEEABHMACKey = "hmac"
	// ACMEEABKeyIDKey holds the ILM profile binding's key ID.
	ACMEEABKeyIDKey = "keyId"
	// ACMEDNS01SecretName is the Secret, in the system namespace, that holds the DNS provider's secret settings
	// under the keys of DNS01Provider.Secrets.
	ACMEDNS01SecretName = "appliance-acme-dns01"
)

// DNS01Key is a setting of a DNS-01 provider; Required says cert-manager refuses the provider without it.
type DNS01Key struct {
	Name     string
	Required bool
	// Field is cert-manager's field of the setting when it is not Name, or Name with the SecretRef suffix for a
	// secret setting.
	Field string
}

// DNS01Provider lists the settings of one of cert-manager's built-in DNS-01 providers, named as in its
// ACMEIssuerDNS01Provider structs without the SecretRef suffix: Config keys are plain strings, Secrets keys are
// kept in the credentials Secret.
type DNS01Provider struct {
	// Field is cert-manager's solver field of the provider when it is not the provider's name.
	Field   string
	Config  []DNS01Key
	Secrets []DNS01Key
}

func required(name string) DNS01Key { return DNS01Key{Name: name, Required: true} }
func optional(name string) DNS01Key { return DNS01Key{Name: name} }

// as names cert-manager's field of the setting.
func (k DNS01Key) as(field string) DNS01Key {
	k.Field = field
	return k
}

// DNS01Providers are the DNS-01 providers the appliance supports, by name.
var DNS01Providers = map[string]DNS01Provider{
	"rfc2136": {
		Config:  []DNS01Key{required("nameserver"), optional("tsigKeyName"), optional("tsigAlgorithm")},
		Secrets: []DNS01Key{optional("tsigSecret")},
	},
	"cloudflare": {Secrets: []DNS01Key{required("apiToken")}},
	"route53": {
		Config:  []DNS01Key{optional("region"), required("accessKeyID"), optional("hostedZoneID"), optional("role")},
		Secrets: []DNS01Key{required("secretAccessKey")},
	},
	"azuredns": {
		Field: "azureDNS",
		Config: []DNS01Key{required("subscriptionID"), required("resourceGroupName"), optional("tenantID"),
			optional("hostedZoneName"), optional("clientID")},
		Secrets: []DNS01Key{optional("clientSecret")},
	},
	"clouddns": {
		Field:   "cloudDNS",
		Config:  []DNS01Key{required("project")},
		Secrets: []DNS01Key{optional("serviceAccountKey").as("serviceAccountSecretRef")},
	},
	"digitalocean": {Secrets: []DNS01Key{required("token")}},
	"acmedns": {
		Field:   "acmeDNS",
		Config:  []DNS01Key{required("host")},
		Secrets: []DNS01Key{required("accountJSON").as("accountSecretRef")},
	},
	"akamai": {
		Config:  []DNS01Key{required("serviceConsumerDomain")},
		Secrets: []DNS01Key{required("clientToken"), required("clientSecret"), required("accessToken")},
	},
}

// ValidateDNS01 checks the settings of a DNS-01 provider: the provider is known, config and secrets use only its
// keys and every required key is given. The errors name keys, never values.
func ValidateDNS01(provider string, config, secrets map[string]string) error {
	p, ok := DNS01Providers[provider]
	if !ok {
		return fmt.Errorf("the DNS provider %q is not supported", provider)
	}
	if err := checkKeys(p.Config, config, "setting"); err != nil {
		return err
	}
	return checkKeys(p.Secrets, secrets, "credential")
}

// checkKeys refuses a key outside keys and a missing required one; kind names the group in the message.
func checkKeys(keys []DNS01Key, given map[string]string, kind string) error {
	for name := range given {
		if !slices.ContainsFunc(keys, func(k DNS01Key) bool { return k.Name == name }) {
			return fmt.Errorf("the DNS provider has no %s %q", kind, name)
		}
	}
	for _, k := range keys {
		if k.Required && given[k.Name] == "" {
			return fmt.Errorf("the DNS provider needs the %s %q", kind, k.Name)
		}
	}
	return nil
}
