package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// bundleFiles are the manifests each kind of bundle carries, as paths in the repository. The bundle stores each under
// its base name.
var bundleFiles = map[string][]string{
	release.BundleSystem:   {"release.yaml", "addons/manager.yaml"},
	release.BundlePlatform: {"addons/ilm-operator.yaml"},
}

const (
	addonsDir    = "addons"
	operatorFile = "ilm-operator.yaml"
)

var arches = []string{"amd64", "arm64"}

// Build writes the unsigned bundle of kind for arch as an OCI layout directory at out. It copies every image of the
// bundle from its registry by the digest release.yaml pins and reads the manifests from files, the repository root.
// built are the images built with the appliance, such as the installer and the manager, each as <ref>@sha256:<hex>;
// only a system bundle carries them, after the images of release.yaml.
func Build(ctx context.Context, m *release.Manifest, kind, arch string, built []string, files fs.FS, notes []byte, out string) (Metadata, error) {
	paths, ok := bundleFiles[kind]
	if !ok {
		return Metadata{}, fmt.Errorf("unknown bundle kind %q", kind)
	}
	if !slices.Contains(arches, arch) {
		return Metadata{}, fmt.Errorf("unknown arch %q", arch)
	}
	builtImages, err := parseBuilt(kind, built)
	if err != nil {
		return Metadata{}, err
	}
	md := newMetadata(m, kind, arch)
	contents, err := readFiles(m, files, paths, &md)
	if err != nil {
		return Metadata{}, err
	}
	var images []v1.Image
	if md.Images, images, err = fetchImages(ctx, append(m.BundleImages(kind, arch), builtImages...), arch); err != nil {
		return Metadata{}, err
	}
	if err := writeLayout(out, md, contents, notes, images); err != nil {
		return Metadata{}, fmt.Errorf("write bundle %s: %w", out, err)
	}
	return md, nil
}

func newMetadata(m *release.Manifest, kind, arch string) Metadata {
	md := Metadata{
		Format:  FormatVersion,
		Kind:    kind,
		Version: m.Version,
		Arch:    arch,
		Created: time.Now().UTC().Truncate(time.Second),
	}
	if kind == release.BundleSystem {
		md.Talos, md.Kubernetes, md.OperatorRange = m.Talos.Version, m.Talos.Kubernetes, m.Compatibility.Operator
	} else {
		md.ILM, md.MinAppliance = m.Platform.ILMVersion, m.Compatibility.MinAppliance
	}
	return md
}

// readFiles reads the manifests at paths, keyed by base name, and records them, and the operator version, in md.
func readFiles(m *release.Manifest, files fs.FS, paths []string, md *Metadata) (map[string][]byte, error) {
	contents := map[string][]byte{}
	for _, p := range paths {
		data, addon, err := readFile(m, files, p)
		if err != nil {
			return nil, err
		}
		base := path.Base(p)
		if base == operatorFile {
			md.Operator = addon.Version
		}
		md.Files = append(md.Files, base)
		contents[base] = data
	}
	return contents, nil
}

// fetchImages fetches the linux/arch image of each of imgs.
func fetchImages(ctx context.Context, imgs []release.Image, arch string) ([]ImageRef, []v1.Image, error) {
	var refs []ImageRef
	var images []v1.Image
	for _, img := range imgs {
		image, err := fetch(ctx, img, arch)
		if err != nil {
			return nil, nil, err
		}
		ref, err := imageRef(img.Ref, image)
		if err != nil {
			return nil, nil, fmt.Errorf("image %s: %w", img.Ref, err)
		}
		slog.InfoContext(ctx, "bundle image", "ref", ref.Ref, "digest", ref.Digest, "size", ref.Size)
		refs = append(refs, ref)
		images = append(images, image)
	}
	return refs, images, nil
}

// readFile reads the file at p from files. An add-on file must match the sha256 release.yaml pins for it; its entry
// is returned.
func readFile(m *release.Manifest, files fs.FS, p string) ([]byte, release.AddonFile, error) {
	data, err := fs.ReadFile(files, p)
	if err != nil {
		return nil, release.AddonFile{}, fmt.Errorf("read %s: %w", p, err)
	}
	if path.Dir(p) != addonsDir {
		return data, release.AddonFile{}, nil
	}
	i := slices.IndexFunc(m.Addons, func(a release.AddonFile) bool { return a.File == path.Base(p) })
	if i < 0 {
		return nil, release.AddonFile{}, fmt.Errorf("%s is not an add-on of the release manifest", p)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != m.Addons[i].SHA256 {
		return nil, release.AddonFile{}, fmt.Errorf("%s has sha256 %s, the release manifest pins %s", p, got, m.Addons[i].SHA256)
	}
	return data, m.Addons[i], nil
}

// parseBuilt parses the references of the images built with the appliance, which only a system bundle carries.
func parseBuilt(kind string, built []string) ([]release.Image, error) {
	if len(built) > 0 && kind != release.BundleSystem {
		return nil, fmt.Errorf("a %s bundle carries no built images", kind)
	}
	images := make([]release.Image, 0, len(built))
	for _, b := range built {
		ref, digest, ok := strings.Cut(b, "@")
		if !ok {
			return nil, fmt.Errorf("built image %s: want <ref>@sha256:<hex>", b)
		}
		h, err := v1.NewHash(digest)
		if err != nil || h.Algorithm != "sha256" {
			return nil, fmt.Errorf("built image %s: want <ref>@sha256:<hex>", b)
		}
		images = append(images, release.Image{Ref: ref, Digest: digest, Bundle: release.BundleSystem})
	}
	return images, nil
}

// fetch returns the image manifest of img for linux/arch, fetched by the digest release.yaml pins.
func fetch(ctx context.Context, img release.Image, arch string) (v1.Image, error) {
	desc, err := fetchPinned(ctx, img)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", img.Ref, err)
	}
	if desc.MediaType.IsIndex() {
		return indexImage(desc, img.Ref, arch)
	}
	return singleImage(desc, img.Ref, arch)
}

// fetchPinned fetches the descriptor of img by its pinned digest.
func fetchPinned(ctx context.Context, img release.Image) (*remote.Descriptor, error) {
	ref, err := name.ParseReference(img.Ref)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(ref.Context().Digest(img.Digest), remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, err
	}
	if desc.Digest.String() != img.Digest {
		return nil, fmt.Errorf("registry returned %s, the release manifest pins %s", desc.Digest, img.Digest)
	}
	return desc, nil
}

// singleImage returns the image desc points to when it is for linux/arch.
func singleImage(desc *remote.Descriptor, ref, arch string) (v1.Image, error) {
	image, err := desc.Image()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	cf, err := image.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if cf.OS != "linux" || cf.Architecture != arch {
		return nil, fmt.Errorf("image %s is for %s/%s, not linux/%s", ref, cf.OS, cf.Architecture, arch)
	}
	return image, nil
}

// indexImage returns the linux/arch image of the index desc points to.
func indexImage(desc *remote.Descriptor, ref, arch string) (v1.Image, error) {
	index, err := desc.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	im, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	for _, d := range im.Manifests {
		if d.MediaType.IsImage() && d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == arch {
			return index.Image(d.Digest)
		}
	}
	return nil, fmt.Errorf("image %s has no linux/%s manifest", ref, arch)
}

func imageRef(ref string, img v1.Image) (ImageRef, error) {
	digest, err := img.Digest()
	if err != nil {
		return ImageRef{}, err
	}
	raw, err := img.RawManifest()
	if err != nil {
		return ImageRef{}, err
	}
	m, err := img.Manifest()
	if err != nil {
		return ImageRef{}, err
	}
	size := int64(len(raw)) + m.Config.Size
	for _, l := range m.Layers {
		size += l.Size
	}
	return ImageRef{Ref: ref, Digest: digest.String(), Size: size}, nil
}

// writeLayout writes the layout of a bundle: the metadata artifact, images[i] for md.Images[i], and the bundle index
// listing both, tagged <version>-<arch>.
func writeLayout(out string, md Metadata, files map[string][]byte, notes []byte, images []v1.Image) error {
	p, err := layout.Write(out, empty.Index)
	if err != nil {
		return err
	}
	meta, err := writeMetadata(p, md, files, notes)
	if err != nil {
		return err
	}
	children := []v1.Descriptor{meta}
	for i, img := range images {
		if err := p.WriteImage(img); err != nil {
			return fmt.Errorf("image %s: %w", md.Images[i].Ref, err)
		}
		mt, err := img.MediaType()
		if err != nil {
			return err
		}
		raw, err := img.RawManifest()
		if err != nil {
			return err
		}
		digest, err := img.Digest()
		if err != nil {
			return err
		}
		children = append(children, v1.Descriptor{
			MediaType:   mt,
			Digest:      digest,
			Size:        int64(len(raw)),
			Platform:    &v1.Platform{OS: "linux", Architecture: md.Arch},
			Annotations: map[string]string{annotationRefName: md.Images[i].Ref},
		})
	}
	index, err := writeJSON(p, types.OCIImageIndex, v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		ArtifactType:  artifactTypeBundle,
		Manifests:     children,
		Annotations:   map[string]string{annotationVersion: md.Version},
	})
	if err != nil {
		return err
	}
	index.Annotations = map[string]string{annotationRefName: md.Version + "-" + md.Arch}
	return p.AppendDescriptor(index)
}

// writeMetadata writes the metadata artifact: bundle.json as its config, the notes and a tar of the files as layers.
func writeMetadata(p layout.Path, md Metadata, files map[string][]byte, notes []byte) (v1.Descriptor, error) {
	config, err := writeJSON(p, mediaTypeBundleConfig, md)
	if err != nil {
		return v1.Descriptor{}, err
	}
	notesLayer, err := writeBytes(p, mediaTypeNotes, notes)
	if err != nil {
		return v1.Descriptor{}, err
	}
	notesLayer.Annotations = map[string]string{annotationTitle: notesTitle}
	archive, err := tarFiles(md.Files, files)
	if err != nil {
		return v1.Descriptor{}, err
	}
	manifestsLayer, err := writeBytes(p, mediaTypeManifests, archive)
	if err != nil {
		return v1.Descriptor{}, err
	}
	manifestsLayer.Annotations = map[string]string{annotationTitle: manifestsTitle}
	desc, err := writeJSON(p, types.OCIManifestSchema1, v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  artifactTypeBundle,
		Config:        config,
		Layers:        []v1.Descriptor{notesLayer, manifestsLayer},
	})
	if err != nil {
		return v1.Descriptor{}, err
	}
	desc.ArtifactType = artifactTypeBundle
	return desc, nil
}

func tarFiles(names []string, files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSON(p layout.Path, mt types.MediaType, v any) (v1.Descriptor, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return writeBytes(p, mt, data)
}

func writeBytes(p layout.Path, mt types.MediaType, data []byte) (v1.Descriptor, error) {
	digest, size, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		return v1.Descriptor{}, err
	}
	if err := p.WriteBlob(digest, io.NopCloser(bytes.NewReader(data))); err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mt, Digest: digest, Size: size}, nil
}
