package registry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// annotationRefName names, in a bundle index, the reference an image was built from.
const annotationRefName = "org.opencontainers.image.ref.name"

// Source is a verified bundle.
type Source interface {
	Images() ([]bundle.Image, error)
	Index() (v1.ImageIndex, error)
}

// PushBundle pushes the images of the verified bundle b into the registry, each under its upstream name by its digest
// alone, and then the bundle index as appliance/<kind>:<version>-<arch>. A load creates no upstream tag, so a workload
// pulling one is not moved to the bundle's image. Blobs the registry already holds are skipped, so a push that failed
// can be repeated. progress, if set, is told the bytes of images pushed and the total.
func (c *Client) PushBundle(ctx context.Context, b Source, md bundle.Metadata, progress func(done, total int64)) error {
	images, err := b.Images()
	if err != nil {
		return err
	}
	targets, err := c.targets(images)
	if err != nil {
		return err
	}
	repo, err := c.repository(bundleRepository(md.Kind))
	if err != nil {
		return err
	}
	var total int64
	for _, im := range images {
		total += im.Ref.Size
	}
	var done int64
	for i, im := range images {
		report := func(n int64) {
			if progress != nil {
				progress(done+min(n, im.Ref.Size), total)
			}
		}
		if err := c.pushImage(ctx, targets[i], im.Image, report); err != nil {
			return fmt.Errorf("push image %s: %w", im.Ref.Ref, err)
		}
		done += im.Ref.Size
		report(0)
		slog.InfoContext(ctx, "pushed bundle image", "ref", im.Ref.Ref, "digest", im.Ref.Digest)
	}
	index, err := b.Index()
	if err != nil {
		return err
	}
	tag := md.Version + "-" + md.Arch
	if err := remote.WriteIndex(repo.Tag(tag), index, c.options(ctx)...); err != nil {
		return fmt.Errorf("push bundle %s %s: %w", md.Kind, tag, err)
	}
	slog.InfoContext(ctx, "pushed bundle", "kind", md.Kind, "bundle", tag)
	return nil
}

// targets returns the reference by digest under its upstream name of each image, so that a bad reference is refused
// before anything is pushed.
func (c *Client) targets(images []bundle.Image) ([]name.Digest, error) {
	refs := make([]name.Digest, 0, len(images))
	for _, im := range images {
		path, _, err := splitRef(im.Ref.Ref)
		if err != nil {
			return nil, err
		}
		repo, err := c.repository(path)
		if err != nil {
			return nil, err
		}
		digest, err := v1.NewHash(im.Ref.Digest)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", im.Ref.Ref, err)
		}
		refs = append(refs, repo.Digest(digest.String()))
	}
	return refs, nil
}

// pushImage pushes img as ref; report is told the bytes written of the image.
func (c *Client) pushImage(ctx context.Context, ref name.Digest, img v1.Image, report func(written int64)) error {
	updates := make(chan v1.Update, 16)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for u := range updates {
			if u.Error == nil {
				report(u.Complete)
			}
		}
	}()
	err := remote.Write(ref, img, append(c.options(ctx), remote.WithProgress(updates))...)
	<-finished
	return err
}

// splitRef splits an image reference as release.yaml writes it, [<host>/]<path>[:<tag>][@<digest>], into the repository
// and the tag. The repository is the name in the registry, as containerd resolves it (release.Repository).
func splitRef(ref string) (repo, tag string, err error) {
	repo, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo, tag = repo[:i], repo[i+1:]
	}
	if repo != "" {
		repo = release.Repository(repo)
	}
	host, path, _ := strings.Cut(repo, "/")
	if host == "" || path == "" || strings.HasSuffix(path, "/") {
		return "", "", fmt.Errorf("the image reference %q has no repository", ref)
	}
	return repo, tag, nil
}
