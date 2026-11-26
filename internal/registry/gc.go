package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// manifest is a manifest of a bundle in the registry: a repository and a digest, and the tag a bundle gave it.
type manifest struct {
	repo, digest, tag string
}

func (m manifest) digestKey() string { return m.repo + "@" + m.digest }
func (m manifest) tagKey() string    { return m.repo + ":" + m.tag }

// parts are the manifests of bundles: the images under their upstream names, the bundle indexes and the copies of
// the images in the bundles' repository.
type parts struct {
	images, indexes, copies []manifest
}

func (p *parts) add(o parts) {
	p.images = append(p.images, o.images...)
	p.indexes = append(p.indexes, o.indexes...)
	p.copies = append(p.copies, o.copies...)
}

func (p parts) all() []manifest { return slices.Concat(p.images, p.indexes, p.copies) }

// Prune deletes the bundles of kind that are not in keep, as the tags Bundles lists, with their images, except the
// manifests and tags a kept bundle or a bundle of another kind lists as well. The registry's garbage collection frees
// the blobs.
//
// The images go first, then the bundle index, then the copies, which zot keeps while an index of their repository
// lists them. A bundle is listed until its index is gone, so the next run repeats what failed before that.
func (c *Client) Prune(ctx context.Context, kind string, keep []string) error {
	tags, err := c.Bundles(ctx, kind)
	if err != nil {
		return err
	}
	kept, err := c.otherKinds(ctx, kind)
	if err != nil {
		return err
	}
	var drop parts
	for _, tag := range tags {
		keeping := slices.Contains(keep, tag)
		p, err := c.prunable(ctx, kind, tag, keeping)
		if err != nil {
			return err
		}
		if keeping {
			protect(kept, p)
		} else {
			drop.add(p)
		}
	}
	for _, step := range []struct {
		manifests  []manifest
		referenced bool
	}{{drop.images, false}, {drop.indexes, false}, {drop.copies, true}} {
		if err := c.removeAll(ctx, step.manifests, kept, step.referenced); err != nil {
			return err
		}
	}
	return nil
}

// otherKinds returns the manifests and tags the bundles of the kinds other than kind list, which share the upstream
// repositories with kind's bundles, as protect records them. A bundle that cannot be read for a reason a retry does
// not fix lists none.
func (c *Client) otherKinds(ctx context.Context, kind string) (map[string]bool, error) {
	kept := map[string]bool{}
	for _, other := range []string{release.BundleSystem, release.BundlePlatform} {
		if other == kind {
			continue
		}
		tags, err := c.Bundles(ctx, other)
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			p, err := c.bundleManifests(ctx, other, tag)
			if err != nil && transient(err) {
				return nil, err
			}
			protect(kept, p)
		}
	}
	return kept, nil
}

// protect records the manifests of p in kept by digest, and by tag those that have one.
func protect(kept map[string]bool, p parts) {
	for _, m := range p.all() {
		kept[m.digestKey()], kept[m.tagKey()] = true, m.tag != ""
	}
}

// prunable returns the manifests of the bundle tag of kind. A bundle that is not kept and cannot be read for a reason
// a retry does not fix is its tag alone.
func (c *Client) prunable(ctx context.Context, kind, tag string, keeping bool) (parts, error) {
	p, err := c.bundleManifests(ctx, kind, tag)
	if err == nil || keeping || transient(err) {
		return p, err
	}
	slog.WarnContext(ctx, "bundle index unreadable, deleting its tag only", "kind", kind, "bundle", tag, "error", err)
	return parts{indexes: []manifest{{repo: bundleRepository(kind), tag: tag}}}, nil
}

// removeAll removes the manifests not in kept, by digest and by tag; referenced tells that a manifest another index
// of its repository still lists is left to that index.
func (c *Client) removeAll(ctx context.Context, manifests []manifest, kept map[string]bool, referenced bool) error {
	for _, m := range manifests {
		if m.digest != "" && kept[m.digestKey()] {
			continue
		}
		if err := c.remove(ctx, m, kept[m.tagKey()], referenced); err != nil {
			return err
		}
	}
	return nil
}

// bundleManifests lists the manifests of the bundle tag of kind.
func (c *Client) bundleManifests(ctx context.Context, kind, tag string) (parts, error) {
	path := bundleRepository(kind)
	repo, err := c.repository(path)
	if err != nil {
		return parts{}, err
	}
	index, err := remote.Index(repo.Tag(tag), c.options(ctx)...)
	if err != nil {
		return parts{}, fmt.Errorf("read bundle %s %s: %w", kind, tag, err)
	}
	im, err := index.IndexManifest()
	if err != nil {
		return parts{}, fmt.Errorf("read bundle %s %s: %w", kind, tag, err)
	}
	digest, err := index.Digest()
	if err != nil {
		return parts{}, fmt.Errorf("read bundle %s %s: %w", kind, tag, err)
	}
	p := parts{indexes: []manifest{{repo: path, digest: digest.String(), tag: tag}}}
	for _, d := range im.Manifests {
		p.copies = append(p.copies, manifest{repo: path, digest: d.Digest.String()})
		ref, ok := d.Annotations[annotationRefName]
		if !ok {
			continue
		}
		upstream, upstreamTag, err := splitRef(ref)
		if err != nil {
			return parts{}, fmt.Errorf("read bundle %s %s: %w", kind, tag, err)
		}
		p.images = append(p.images, manifest{repo: upstream, digest: d.Digest.String(), tag: upstreamTag})
	}
	return p, nil
}

// transient reports whether err may go away when retried: the registry could not be reached or failed.
func transient(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode >= http.StatusInternalServerError || terr.StatusCode == http.StatusTooManyRequests
	}
	var uerr *url.Error
	return errors.As(err, &uerr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// remove deletes m by its digest and, unless keepTag, by its tag. What is gone already is not an error, nor, when
// referenced, a manifest the registry keeps because an index lists it.
func (c *Client) remove(ctx context.Context, m manifest, keepTag, referenced bool) error {
	repo, err := c.repository(m.repo)
	if err != nil {
		return err
	}
	var refs []name.Reference
	if m.digest != "" {
		refs = append(refs, repo.Digest(m.digest))
	}
	if m.tag != "" && !keepTag {
		refs = append(refs, repo.Tag(m.tag))
	}
	for _, ref := range refs {
		err := remote.Delete(ref, c.options(ctx)...)
		if isNotFound(err) {
			continue
		}
		if referenced && isStatus(err, http.StatusMethodNotAllowed) {
			slog.DebugContext(ctx, "registry manifest kept by an index", "reference", ref.String())
			continue
		}
		if err != nil {
			return fmt.Errorf("delete %s: %w", ref, err)
		}
		slog.InfoContext(ctx, "deleted registry manifest", "reference", ref.String())
	}
	return nil
}
