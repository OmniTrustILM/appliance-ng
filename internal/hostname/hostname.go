// Package hostname validates the host names the appliance is given for the node and the platform.
package hostname

import (
	"net/netip"
	"regexp"
)

// maxLength is the longest host name DNS allows.
const maxLength = 253

var fqdn = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)+$`)

// IsFQDN reports whether s is a fully qualified host name in lower case: two or more labels of letters, digits and
// hyphens of at most 63 characters each, at most 253 characters in all, and not an IP address.
func IsFQDN(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return false
	}
	return len(s) <= maxLength && fqdn.MatchString(s)
}
