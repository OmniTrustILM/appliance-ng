package update

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// DefaultRepository holds the published bundles in <DefaultRepository>/system and <DefaultRepository>/platform.
const DefaultRepository = "ghcr.io/omnitrustilm/appliance-ng"

// Limits of what is read of a bundle in its repository before its signature is checked.
const (
	maxManifest   = 4 << 20
	maxMetadata   = 128 << 20
	maxSignature  = 4 << 20
	maxReferrers  = 32
	sigstoreMedia = "application/vnd.dev.sigstore.bundle.v0.3+json"
)

// Online is a repository of bundles.
type Online struct {
	// Repository holds the bundle repositories <Repository>/system and <Repository>/platform.
	Repository string
	// Options reach the repository, read from the Appliance at each use.
	Options func(ctx context.Context) ([]remote.Option, error)
}

func (o Online) repository(kind string) (name.Repository, error) {
	return name.NewRepository(o.Repository + "/" + kind)
}

// errSignature marks a bundle in the repository whose signature or metadata does not hold.
var errSignature = errors.New("the bundle in the repository is not verified")

// signed is a bundle in the repository whose signature and metadata are verified: its index, read by digest, and a
// layout directory with its metadata, notes and files.
type signed struct {
	b     *bundle.Bundle
	index v1.ImageIndex
	md    bundle.Metadata
}

// signed reads the bundle kind:tag, its metadata and its signatures, into a layout in dir and verifies them with key
// for arch. The images stay in the repository. An unverified bundle wraps errSignature and the verification error.
func (o Online) signed(ctx context.Context, kind, tag, dir string, key crypto.PublicKey, arch string) (*signed, error) {
	opts, err := o.Options(ctx)
	if err != nil {
		return nil, fail("reach the repository", err)
	}
	repo, err := o.repository(kind)
	if err != nil {
		return nil, fail("name the repository", err)
	}
	desc, err := remote.Get(repo.Tag(tag), opts...)
	if err != nil {
		return nil, fail("read the bundle", err)
	}
	digest, size, err := v1.SHA256(bytes.NewReader(desc.Manifest))
	if err != nil || !desc.MediaType.IsIndex() || size > maxManifest {
		return nil, signatureError(fmt.Errorf("%s is no bundle index", tag))
	}
	index, err := remote.Index(repo.Digest(digest.String()), opts...)
	if err != nil {
		return nil, fail("read the bundle", err)
	}
	if err := writeSigned(dir, repo, desc, digest, index, opts); err != nil {
		return nil, err
	}
	b, err := bundle.Open(dir)
	if err != nil {
		return nil, fail("open the bundle", err)
	}
	md, err := b.VerifySigned(key, arch)
	if err != nil {
		_ = b.Close()
		return nil, &Error{Reason: Reason(verifyError(err)), step: "verify", err: fmt.Errorf("%w: %w", errSignature, err)}
	}
	if md.Kind != kind || tag != md.Version+"-"+md.Arch {
		_ = b.Close()
		return nil, signatureError(fmt.Errorf("%s:%s holds the %s bundle %s-%s", kind, tag, md.Kind, md.Version, md.Arch))
	}
	return &signed{b: b, index: index, md: md}, nil
}

func signatureError(err error) error {
	return &Error{Reason: BadSignature, step: "verify", err: fmt.Errorf("%w: %w", errSignature, err)}
}

// writeSigned writes a layout into dir with the bundle index, its metadata artifact and its referrers.
func writeSigned(dir string, repo name.Repository, desc *remote.Descriptor, digest v1.Hash, index v1.ImageIndex, opts []remote.Option) error {
	p, err := layout.Write(dir, empty.Index)
	if err != nil {
		return fail("write the bundle", err)
	}
	if err := p.WriteBlob(digest, io.NopCloser(bytes.NewReader(desc.Manifest))); err != nil {
		return fail("write the bundle", err)
	}
	if err := p.AppendDescriptor(v1.Descriptor{MediaType: desc.MediaType, Digest: digest, Size: int64(len(desc.Manifest))}); err != nil {
		return fail("write the bundle", err)
	}
	im, err := index.IndexManifest()
	if err != nil || len(im.Manifests) == 0 {
		return signatureError(errors.New("the bundle index is empty"))
	}
	metadata, err := index.Image(im.Manifests[0].Digest)
	if err != nil {
		return fail("read the bundle metadata", err)
	}
	if err := writeImage(p, metadata, maxMetadata, false); err != nil {
		return err
	}
	return writeReferrers(p, repo.Digest(digest.String()), opts)
}

// writeReferrers writes the signatures among the referrers of the bundle index ref into the layout, listed in
// index.json.
func writeReferrers(p layout.Path, ref name.Digest, opts []remote.Option) error {
	referrers, err := remote.Referrers(ref, opts...)
	if err != nil {
		return fail("read the signatures", err)
	}
	rim, err := referrers.IndexManifest()
	if err != nil {
		return fail("read the signatures", err)
	}
	for i, d := range rim.Manifests {
		if i == maxReferrers {
			break
		}
		if d.ArtifactType != sigstoreMedia {
			continue
		}
		img, err := remote.Image(ref.Context().Digest(d.Digest.String()), opts...)
		if err != nil {
			return fail("read a signature", err)
		}
		if err := writeImage(p, img, maxSignature, true); err != nil {
			return err
		}
	}
	return nil
}

// writeImage writes img, at most limit bytes of config and layers, into the layout, and lists it in index.json when
// listed.
func writeImage(p layout.Path, img v1.Image, limit int64, listed bool) error {
	m, err := img.Manifest()
	if err != nil {
		return fail("read the bundle", err)
	}
	if err := withinLimit(append([]v1.Descriptor{m.Config}, m.Layers...), limit); err != nil {
		return signatureError(err)
	}
	if err := p.WriteImage(img); err != nil {
		return fail("write the bundle", err)
	}
	if !listed {
		return nil
	}
	raw, err := img.RawManifest()
	if err != nil {
		return fail("read the bundle", err)
	}
	digest, size, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return fail("read the bundle", err)
	}
	if err := p.AppendDescriptor(v1.Descriptor{MediaType: m.MediaType, Digest: digest, Size: size}); err != nil {
		return fail("write the bundle", err)
	}
	return nil
}

// withinLimit refuses blobs whose sizes add up to more than limit. A negative size is refused too: the registry
// client reads a blob of unknown size without bound, while it stops a blob of known size at that size.
func withinLimit(blobs []v1.Descriptor, limit int64) error {
	var total int64
	for _, d := range blobs {
		if d.Size < 0 || d.Size > limit-total {
			return fmt.Errorf("an artifact with a blob of %d bytes after %d, more than %d", d.Size, total, limit)
		}
		total += d.Size
	}
	return nil
}

// remoteSource is a bundle whose signature was verified, with its images read from the repository by the digests
// its signed metadata lists.
type remoteSource struct {
	index v1.ImageIndex
	md    bundle.Metadata
}

func (s remoteSource) Index() (v1.ImageIndex, error) { return s.index, nil }

func (s remoteSource) Images() ([]bundle.Image, error) {
	images := make([]bundle.Image, 0, len(s.md.Images))
	for _, ref := range s.md.Images {
		h, err := v1.NewHash(ref.Digest)
		if err != nil {
			return nil, fmt.Errorf("bundle image %s: %w", ref.Ref, err)
		}
		img, err := s.index.Image(h)
		if err != nil {
			return nil, fmt.Errorf("bundle image %s: %w", ref.Ref, err)
		}
		images = append(images, bundle.Image{Ref: ref, Image: img})
	}
	return images, nil
}

// Fetch loads the bundle version of kind for the manager's arch from the online repository, checking it as Load does
// before its images are copied into the registry. The caller holds a reservation.
func (l *Loader) Fetch(ctx context.Context, kind, version string) error {
	ctx, cancel := context.WithTimeout(ctx, l.deps.FetchTimeout)
	defer cancel()
	l.started(kind, version)
	md, err := l.fetch(ctx, kind, version)
	if md.Version == "" {
		md.Version = version
	}
	l.finish(ctx, kind, sourceOnline, md, err)
	return err
}

func (l *Loader) fetch(ctx context.Context, kind, version string) (bundle.Metadata, error) {
	if err := os.MkdirAll(l.deps.Uploads, dirMode); err != nil {
		return bundle.Metadata{}, fail("create the uploads directory", err)
	}
	dir, err := os.MkdirTemp(l.deps.Uploads, ".fetch-")
	if err != nil {
		return bundle.Metadata{}, fail("create the uploads directory", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	s, err := l.deps.Online.signed(ctx, kind, tag(version, l.deps.Arch), dir, l.deps.Key, l.deps.Arch)
	if err != nil {
		return bundle.Metadata{}, err
	}
	defer func() { _ = s.b.Close() }()
	cur, err := l.admissible(ctx, kind, version, s.md)
	if err != nil {
		return s.md, err
	}
	if err := l.deps.Space(l.deps.Uploads, uint64(bundleInfo(s.md).SizeBytes)); err != nil { //nolint:gosec // sizes are never negative
		return s.md, spaceError(err)
	}
	return s.md, l.install(ctx, remoteSource{index: s.index, md: s.md}, s.b, s.md, cur)
}

// ApplianceOptions returns the options that reach the registry host through the Appliance's proxy, trusting the
// system roots and its additional trusted CAs, with the credentials its registries name for host. c reads the
// Appliance and the Secrets.
func ApplianceOptions(c client.Reader, host string) func(ctx context.Context) ([]remote.Option, error) {
	return func(ctx context.Context) ([]remote.Option, error) {
		a := &v1alpha1.Appliance{}
		if err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
			return nil, fmt.Errorf("read the appliance: %w", err)
		}
		roots, err := trustedRoots(ctx, c, a)
		if err != nil {
			return nil, err
		}
		t, ok := ilmclient.ProviderClient(roots, a.Spec.Proxy, 0).Transport.(*http.Transport)
		if !ok {
			return nil, errors.New("the outbound client has no HTTP transport to guard")
		}
		opts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(guard(t, idleTimeout))}
		auth, err := credentials(ctx, c, a, host)
		if err != nil || auth == nil {
			return opts, err
		}
		return append(opts, remote.WithAuth(auth)), nil
	}
}

// trustedRoots are the system roots and the Appliance's additional trusted CAs.
func trustedRoots(ctx context.Context, c client.Reader, a *v1alpha1.Appliance) (*x509.CertPool, error) {
	var bundle []byte
	if ref := a.Spec.Trust; ref != nil {
		s, err := systemSecret(ctx, c, ref.Name)
		if err != nil {
			return nil, err
		}
		bundle = s.Data[ref.Key]
	}
	roots, _ := certs.TrustedRoots(bundle) // without the system roots, the additional CAs are still trusted
	return roots, nil
}

// credentials are those the Appliance's registries name for host; nil without any.
func credentials(ctx context.Context, c client.Reader, a *v1alpha1.Appliance, host string) (authn.Authenticator, error) {
	for _, r := range a.Spec.Registries {
		if r.Host != host || r.CredentialsRef == "" {
			continue
		}
		s, err := systemSecret(ctx, c, r.CredentialsRef)
		if err != nil {
			return nil, err
		}
		return &authn.Basic{
			Username: string(s.Data[v1alpha1.CredentialsUsernameKey]),
			Password: string(s.Data[v1alpha1.CredentialsPasswordKey]),
		}, nil
	}
	return nil, nil
}

// idleTimeout bounds each read and write of a connection to the repository and the wait for an answer's headers, so
// that a stalled repository or proxy fails a check or a fetch instead of holding it.
const idleTimeout = 2 * time.Minute

// guard makes t give up a connection that stalls for idle and refuses requests without https, which also
// refuses a redirect away from https.
func guard(t *http.Transport, idle time.Duration) http.RoundTripper {
	dial := t.DialContext
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleConn{Conn: c, idle: idle}, nil
	}
	t.ResponseHeaderTimeout = idle
	return httpsOnly{t}
}

type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, errors.New("refused a request without https")
	}
	return h.next.RoundTrip(req)
}

// idleConn fails a read or a write that makes no progress for idle.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

func systemSecret(ctx context.Context, c client.Reader, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: name}, s); err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", kube.SystemNamespace, name, err)
	}
	return s, nil
}
