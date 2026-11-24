package bundle

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

// Size limits of blobs read into memory.
const (
	maxManifestSize = 4 << 20
	maxFilesSize    = 64 << 20
)

// Bundle is a bundle in an OCI layout directory. Its files, notes and images are available once Verify succeeds.
type Bundle struct {
	dir string
	// extracted is set when dir was extracted from a tar; Close removes it.
	extracted bool
	verified  *verified
}

type verified struct {
	index    v1.Hash
	metadata Metadata
	notes    v1.Descriptor
	files    v1.Descriptor
}

// Open opens the bundle at path, an OCI layout directory or a tar of one. A tar is extracted into a directory next to
// it, which Close removes.
func Open(path string) (*Bundle, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	if info.IsDir() {
		return &Bundle{dir: path}, nil
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".bundle-")
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	if err := extract(path, dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Bundle{dir: dir, extracted: true}, nil
}

// Close removes the directory a tar was extracted into.
func (b *Bundle) Close() error {
	if !b.extracted {
		return nil
	}
	return os.RemoveAll(b.dir)
}

// extract writes the layout entries of the tar at archive into dir: oci-layout, index.json and the blobs. It skips
// directories and anything else, and refuses sparse entries and content larger than the archive itself.
func extract(archive, dir string) error {
	f, err := os.Open(archive) //nolint:gosec // the path is the caller's bundle file
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	cr := &countingReader{r: f}
	x := &extractor{tr: tar.NewReader(cr), cr: cr, dir: dir, budget: info.Size()}
	for {
		h, err := x.tr.Next()
		if errors.Is(err, io.EOF) {
			// A tar that ends at an entry boundary lacks its end-of-archive marker, two zero blocks.
			if cr.n < x.end+2*tarBlock {
				return ErrTruncated
			}
			return nil
		}
		if err != nil {
			return tarError(err)
		}
		if err := x.entry(h); err != nil {
			return err
		}
	}
}

// extractor writes the layout entries of a tar into dir, at most budget bytes in all.
type extractor struct {
	tr     *tar.Reader
	cr     *countingReader
	dir    string
	budget int64
	// end is the offset where the data of the last entry read ends.
	end int64
}

func (x *extractor) entry(h *tar.Header) error {
	if sparse(h) {
		return fmt.Errorf("read bundle file: %s is a sparse entry", h.Name)
	}
	x.end = x.cr.n + (h.Size+tarBlock-1)/tarBlock*tarBlock
	name, ok := layoutEntry(h)
	if !ok {
		return nil
	}
	n, err := extractFile(io.LimitReader(x.tr, x.budget+1), filepath.Join(x.dir, filepath.FromSlash(name)))
	if err != nil {
		return tarError(err)
	}
	if x.budget -= n; x.budget < 0 {
		return errors.New("read bundle file: its entries hold more data than the file")
	}
	return nil
}

func sparse(h *tar.Header) bool {
	if h.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

const tarBlock = 512

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func tarError(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	}
	return fmt.Errorf("read bundle file: %w", err)
}

// layoutEntry returns the path of a regular file of an OCI layout, accepting a leading "./".
func layoutEntry(h *tar.Header) (string, bool) {
	if h.Typeflag != tar.TypeReg {
		return "", false
	}
	name := path.Clean(strings.TrimPrefix(h.Name, "./"))
	if strings.Contains(name, "..") {
		return "", false
	}
	if name == "oci-layout" || name == "index.json" {
		return name, true
	}
	hexDigest, ok := strings.CutPrefix(name, "blobs/sha256/")
	if !ok || !validHex(hexDigest) {
		return "", false
	}
	return path.Join("blobs", "sha256", filepath.Base(hexDigest)), true
}

// extractFile writes r to the new file dst and returns the bytes written.
func extractFile(r io.Reader, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // dst is a checked layout path
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		return n, err
	}
	return n, f.Close()
}

func validHex(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// File returns the manifest name of a verified bundle.
func (b *Bundle) File(name string) ([]byte, error) {
	if b.verified == nil {
		return nil, errors.New("the bundle is not verified")
	}
	if !slices.Contains(b.verified.metadata.Files, name) {
		return nil, fmt.Errorf("the bundle has no file %s", name)
	}
	archive, err := b.readBlob(b.verified.files, maxFilesSize)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("the bundle has no file %s: %w", name, err)
		}
		if h.Name == name {
			return io.ReadAll(tr)
		}
	}
}

// Notes returns the release notes of a verified bundle.
func (b *Bundle) Notes() ([]byte, error) {
	if b.verified == nil {
		return nil, errors.New("the bundle is not verified")
	}
	return b.readBlob(b.verified.notes, maxFilesSize)
}

// Images yields each image of a verified bundle in metadata order; the image is nil if it cannot be read.
func (b *Bundle) Images() iter.Seq2[ImageRef, v1.Image] {
	return func(yield func(ImageRef, v1.Image) bool) {
		if b.verified == nil {
			return
		}
		index, err := layout.Path(b.dir).ImageIndex()
		if err == nil {
			index, err = index.ImageIndex(b.verified.index)
		}
		for _, ref := range b.verified.metadata.Images {
			var img v1.Image
			if err == nil {
				img = b.image(index, ref)
			}
			if !yield(ref, img) {
				return
			}
		}
	}
}

func (b *Bundle) image(index v1.ImageIndex, ref ImageRef) v1.Image {
	h, err := v1.NewHash(ref.Digest)
	if err != nil {
		return nil
	}
	img, err := index.Image(h)
	if err != nil {
		return nil
	}
	return img
}

// openBlob opens the blob d names.
func (b *Bundle) openBlob(d v1.Descriptor) (*os.File, error) {
	if d.Digest.Algorithm != "sha256" || !validHex(d.Digest.Hex) {
		return nil, fmt.Errorf("unsupported digest %q", d.Digest)
	}
	f, err := os.Open(filepath.Join(b.dir, "blobs", "sha256", d.Digest.Hex))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	return f, nil
}

// readBlob reads a blob of at most limit bytes and checks it against d.
func (b *Bundle) readBlob(d v1.Descriptor, limit int64) ([]byte, error) {
	if d.Size > limit {
		return nil, fmt.Errorf("blob %s: %d bytes, more than %d", d.Digest, d.Size, limit)
	}
	f, err := b.openBlob(d)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, d.Size+1))
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	return data, matches(d, bytes.NewReader(data))
}

// checkBlob streams a blob and checks it against d.
func (b *Bundle) checkBlob(d v1.Descriptor) error {
	f, err := b.openBlob(d)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return matches(d, f)
}

func matches(d v1.Descriptor, r io.Reader) error {
	digest, size, err := v1.SHA256(io.LimitReader(r, d.Size+1))
	if err != nil {
		return fmt.Errorf("blob %s: %w", d.Digest, err)
	}
	if digest != d.Digest || size != d.Size {
		return fmt.Errorf("blob %s does not match its descriptor", d.Digest)
	}
	return nil
}
