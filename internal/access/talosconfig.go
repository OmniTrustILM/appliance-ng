package access

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"

	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

// The Talos roles a talosconfig can carry.
const (
	RoleOSReader = "os:reader"
	RoleOSAdmin  = "os:admin"
)

// MaxTalosconfigTTL is the longest a talosconfig is valid.
const MaxTalosconfigTTL = 8 * time.Hour

// ValidateTalosconfig returns an *InvalidError unless role is a talosconfig role and ttl whole hours up to MaxTalosconfigTTL.
func ValidateTalosconfig(role string, ttl time.Duration) error {
	if role != RoleOSReader && role != RoleOSAdmin {
		return &InvalidError{FieldRole, "Choose os:reader or os:admin."}
	}
	if ttl < time.Hour || ttl > MaxTalosconfigTTL || ttl%time.Hour != 0 {
		return &InvalidError{FieldHours, "Choose 1 to 8 hours."}
	}
	return nil
}

// BreakGlassTalosconfigTTL is how long the recovery kit's talosconfig is valid.
const BreakGlassTalosconfigTTL = 365 * 24 * time.Hour

var errInvalidTalosconfig = errors.New("the node returned an invalid talosconfig")

// Talosconfig has the node generate a talosconfig with role, valid for ttl, and points it at address. The node's
// CA signs the client certificate; the manager never holds the CA key.
func Talosconfig(ctx context.Context, node talos.Node, address, role string, ttl time.Duration) ([]byte, error) {
	if err := ValidateTalosconfig(role, ttl); err != nil {
		return nil, err
	}
	cfg, err := generate(ctx, node, address, role, ttl)
	if err != nil {
		return nil, err
	}
	return encode(cfg)
}

// BreakGlassTalosconfig has the node generate the recovery kit's os:admin talosconfig, valid for
// BreakGlassTalosconfigTTL, points it at address and returns it with the expiry of its client certificate.
func BreakGlassTalosconfig(ctx context.Context, node talos.Node, address string) ([]byte, time.Time, error) {
	cfg, err := generate(ctx, node, address, RoleOSAdmin, BreakGlassTalosconfigTTL)
	if err != nil {
		return nil, time.Time{}, err
	}
	notAfter, err := certificateNotAfter(cfg)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := encode(cfg)
	return data, notAfter, err
}

// generate has the node generate a talosconfig with role, valid for ttl, and points it at address.
func generate(ctx context.Context, node talos.Node, address, role string, ttl time.Duration) (*clientconfig.Config, error) {
	data, err := node.ClientConfig(ctx, []string{role}, ttl)
	if err != nil {
		return nil, err
	}
	cfg, err := clientconfig.FromBytes(data)
	if err != nil {
		// The parser quotes the offending value, which may be the private key.
		return nil, errInvalidTalosconfig
	}
	for _, c := range cfg.Contexts {
		c.Endpoints, c.Nodes = []string{address}, []string{address}
	}
	return cfg, nil
}

// certificateNotAfter is the expiry of the client certificate of the talosconfig's current context.
func certificateNotAfter(cfg *clientconfig.Config) (time.Time, error) {
	current := cfg.Contexts[cfg.Context]
	if current == nil {
		return time.Time{}, errInvalidTalosconfig
	}
	crt, err := base64.StdEncoding.DecodeString(current.Crt)
	if err != nil {
		return time.Time{}, errInvalidTalosconfig
	}
	block, _ := pem.Decode(crt)
	if block == nil {
		return time.Time{}, errInvalidTalosconfig
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, errInvalidTalosconfig
	}
	return leaf.NotAfter.UTC(), nil
}

func encode(cfg *clientconfig.Config) ([]byte, error) {
	out, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode the talosconfig: %w", err)
	}
	return out, nil
}
