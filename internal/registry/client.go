package registry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Client talks to the in-cluster zot registry.
type Client struct {
	host      string
	transport http.RoundTripper
	auth      authn.Authenticator
}

// New returns a client of the registry at endpoint, a host:port or an https URL, that trusts the TLS certificates
// roots verify. It pushes as user with password; an empty password pulls anonymously.
// A connection that stalls for IdleTimeout fails, as does a TLS handshake that takes longer than the default one.
func New(endpoint string, roots *x509.CertPool, user, password string) *Client {
	return newWithIdle(endpoint, roots, user, password, IdleTimeout)
}

func newWithIdle(endpoint string, roots *x509.CertPool, user, password string, idle time.Duration) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	c := &Client{
		host:      strings.TrimPrefix(endpoint, "https://"),
		transport: Guard(t, idle),
		auth:      authn.Anonymous,
	}
	if password != "" {
		c.auth = &authn.Basic{Username: user, Password: password}
	}
	return c
}

func (c *Client) options(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithTransport(c.transport), remote.WithAuth(c.auth)}
}

// repository returns the repository path in this registry.
func (c *Client) repository(path string) (name.Repository, error) {
	repo, err := name.NewRepository(c.host + "/" + path)
	if err != nil {
		return name.Repository{}, fmt.Errorf("repository %s: %w", path, err)
	}
	return repo, nil
}

// bundleRepository is the repository that holds the bundle indexes of kind.
func bundleRepository(kind string) string { return "appliance/" + kind }

// Bundles lists the bundles of kind in the registry as their tags, <version>-<arch>.
func (c *Client) Bundles(ctx context.Context, kind string) ([]string, error) {
	repo, err := c.repository(bundleRepository(kind))
	if err != nil {
		return nil, err
	}
	tags, err := remote.List(repo, c.options(ctx)...)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s bundles: %w", kind, err)
	}
	return tags, nil
}

func isNotFound(err error) bool { return isStatus(err, http.StatusNotFound) }

func isStatus(err error, code int) bool {
	var terr *transport.Error
	return errors.As(err, &terr) && terr.StatusCode == code
}

// Copy copies the image or image index src, a reference in any registry, to dst, a reference in this registry
// without its host. opts apply to reading src, such as a proxy or credentials.
func (c *Client) Copy(ctx context.Context, src, dst string, opts ...remote.Option) error {
	from, err := name.ParseReference(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	to, err := name.ParseReference(c.host + "/" + dst)
	if err != nil {
		return fmt.Errorf("copy to %s: %w", dst, err)
	}
	desc, err := remote.Get(from, append([]remote.Option{remote.WithContext(ctx)}, opts...)...)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err == nil {
			err = remote.WriteIndex(to, idx, c.options(ctx)...)
		}
		return copyResult(src, dst, err)
	}
	img, err := desc.Image()
	if err == nil {
		err = remote.Write(to, img, c.options(ctx)...)
	}
	return copyResult(src, dst, err)
}

func copyResult(src, dst string, err error) error {
	if err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}
