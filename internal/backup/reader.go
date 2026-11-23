package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"

	"filippo.io/age"
)

// decoder walks the tar members of a decrypted archive, hashing each one as it is read.
type decoder struct {
	plain    io.Reader
	tr       *tar.Reader
	seen     map[string]string
	pending  *tar.Header
	manifest *Manifest

	// started holds every logical member name seen; group is the chunked name being read and nextChunk the chunk
	// number it must continue with.
	started   map[string]bool
	group     string
	nextChunk int
}

func newDecoder(r io.Reader, id age.Identity) (*decoder, error) {
	dec, err := age.Decrypt(r, id)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			err = ErrTruncated
		}
		return nil, fmt.Errorf("decrypt backup: %w", err)
	}
	return &decoder{plain: dec, tr: tar.NewReader(dec), seen: map[string]string{}, started: map[string]bool{}}, nil
}

// readError classifies an error from the decrypted stream: a stream that ends early is truncated, anything else
// (a failed authentication, a malformed tar header) means the archive was modified. The cause stays reachable.
func readError(err error) error {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return ErrTruncated
	case errors.Is(err, ErrTampered), errors.Is(err, ErrTruncated):
		return err
	}
	return fmt.Errorf("%w: %w", ErrTampered, err)
}

// next returns the next member header, or io.EOF after the manifest. The archive ending before the manifest is
// ErrTruncated; a member that is not a regular file, or a manifest larger than maxManifestSize, is ErrTampered.
func (d *decoder) next() (*tar.Header, error) {
	if d.pending != nil {
		h := d.pending
		d.pending = nil
		return h, nil
	}
	h, err := d.tr.Next()
	if errors.Is(err, io.EOF) && d.manifest == nil {
		return nil, ErrTruncated
	}
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, readError(err)
	}
	if d.manifest != nil {
		return nil, fmt.Errorf("%w: data after the manifest", ErrTampered)
	}
	if h.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("%w: a member is not a regular file", ErrTampered)
	}
	if h.Name == manifestName && h.Size > maxManifestSize {
		return nil, fmt.Errorf("%w: the manifest is too large", ErrTampered)
	}
	return h, nil
}

// hashedMember reads one tar member and records its SHA-256 when it reaches the end.
type hashedMember struct {
	d    *decoder
	name string
	h    hash.Hash
}

func (d *decoder) open(h *tar.Header) (*hashedMember, error) {
	if err := d.place(h.Name); err != nil {
		return nil, err
	}
	if _, dup := d.seen[h.Name]; dup {
		return nil, fmt.Errorf("%w: member %s appears twice", ErrTampered, h.Name)
	}
	return &hashedMember{d: d, name: h.Name, h: sha256.New()}, nil
}

// place accepts only the names and chunk layouts the Writer produces: valid logical names, and chunks of one name
// numbered from 000 without gaps, contiguous in the archive and never sharing a name with a plain member.
func (d *decoder) place(name string) error {
	base, chunked := splitChunk(name)
	if err := validName(base); err != nil {
		return fmt.Errorf("%w: %w", ErrTampered, err)
	}
	if !chunked {
		if d.started[name] {
			return fmt.Errorf("%w: member %s appears twice", ErrTampered, name)
		}
		d.started[name], d.group = true, ""
		return nil
	}
	if base != d.group {
		if d.started[base] {
			return fmt.Errorf("%w: chunks of %s are not contiguous", ErrTampered, base)
		}
		d.started[base], d.group, d.nextChunk = true, base, 0
	}
	if name != chunkName(base, d.nextChunk) {
		return fmt.Errorf("%w: unexpected chunk %s", ErrTampered, name)
	}
	d.nextChunk++
	return nil
}

func (m *hashedMember) Read(p []byte) (int, error) {
	n, err := m.d.tr.Read(p)
	m.h.Write(p[:n])
	if errors.Is(err, io.EOF) {
		m.d.seen[m.name] = hex.EncodeToString(m.h.Sum(nil))
		return n, io.EOF
	}
	if err != nil {
		err = readError(err)
	}
	return n, err
}

func (d *decoder) readManifest() error {
	var m Manifest
	dec := json.NewDecoder(io.LimitReader(d.tr, maxManifestSize))
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrTruncated
		}
		return fmt.Errorf("%w: unreadable manifest", ErrTampered)
	}
	if err := dec.Decode(&struct{}{}); errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrTruncated
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the manifest", ErrTampered)
	}
	if m.Format > FormatVersion {
		return ErrFormatTooNew
	}
	if m.Format < 1 {
		return fmt.Errorf("%w: invalid format %d", ErrTampered, m.Format)
	}
	d.manifest = &m
	return nil
}

// finish checks every member against the manifest once the archive has been read to its end.
func (d *decoder) finish() (Manifest, error) {
	if _, err := d.next(); !errors.Is(err, io.EOF) {
		return Manifest{}, err
	}
	if err := d.drain(); err != nil {
		return Manifest{}, err
	}
	m := *d.manifest
	if len(m.Members) != len(d.seen) {
		return Manifest{}, fmt.Errorf("%w: member list differs from the manifest", ErrTampered)
	}
	for name, sum := range d.seen {
		if m.Members[name] != sum {
			return Manifest{}, fmt.Errorf("%w: %s does not match the manifest", ErrTampered, name)
		}
	}
	return m, nil
}

// drain reads the decrypted stream past the tar end to its end, so that age authenticates its last chunk. Only zero
// padding may follow the tar end.
func (d *decoder) drain() error {
	buf := make([]byte, 32<<10)
	for {
		n, err := d.plain.Read(buf)
		if slices.ContainsFunc(buf[:n], func(b byte) bool { return b != 0 }) {
			return fmt.Errorf("%w: data after the end of the archive", ErrTampered)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return readError(err)
		}
	}
}

// walk calls fn for every member except the manifest, then checks the archive against the manifest.
func (d *decoder) walk(fn func(h *tar.Header) error) (Manifest, error) {
	for {
		h, err := d.next()
		if errors.Is(err, io.EOF) {
			return d.finish()
		}
		if err != nil {
			return Manifest{}, err
		}
		if h.Name == manifestName {
			err = d.readManifest()
		} else {
			err = fn(h)
		}
		if err != nil {
			return Manifest{}, err
		}
	}
}

// Verify decrypts the whole archive and checks every member against the manifest.
func Verify(r io.Reader, id age.Identity) (Manifest, error) {
	d, err := newDecoder(r, id)
	if err != nil {
		return Manifest{}, err
	}
	return d.walk(func(h *tar.Header) error {
		m, err := d.open(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, m)
		return err
	})
}

// Extract decrypts the archive and calls fn once per logical member, in archive order, with one reader over the
// member's content (chunks joined). It streams, so fn may already have run for earlier members when a damaged archive
// is detected: run Verify first. Extract still fails with ErrTampered or ErrTruncated when the archive does not match
// its manifest once it has been read completely.
func Extract(r io.Reader, id age.Identity, fn func(name string, r io.Reader) error) error {
	d, err := newDecoder(r, id)
	if err != nil {
		return err
	}
	_, err = d.walk(func(h *tar.Header) error { return d.extractMember(h, fn) })
	return err
}

func (d *decoder) extractMember(h *tar.Header, fn func(string, io.Reader) error) error {
	m, err := d.open(h)
	if err != nil {
		return err
	}
	base, chunked := splitChunk(h.Name)
	var src io.Reader = m
	if chunked {
		src = &joined{d: d, base: base, cur: m}
	}
	if err := fn(base, src); err != nil {
		return err
	}
	// fn may stop early; read on so the walk stays in step with the archive.
	_, err = io.Copy(io.Discard, src)
	return err
}

// joined reads consecutive chunk members of one logical name as a single stream.
type joined struct {
	d    *decoder
	base string
	cur  *hashedMember
}

func (j *joined) Read(p []byte) (int, error) {
	for {
		if j.cur == nil {
			return 0, io.EOF
		}
		n, err := j.cur.Read(p)
		if !errors.Is(err, io.EOF) {
			return n, err
		}
		if err := j.advance(); err != nil {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

// advance moves to the next chunk of the same name, or ends the stream and leaves the next header pending.
func (j *joined) advance() error {
	h, err := j.d.next()
	if errors.Is(err, io.EOF) {
		j.cur = nil
		return nil
	}
	if err != nil {
		return err
	}
	if base, chunked := splitChunk(h.Name); !chunked || base != j.base {
		j.d.pending = h
		j.cur = nil
		return nil
	}
	j.cur, err = j.d.open(h)
	return err
}
