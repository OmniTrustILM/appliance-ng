package backup

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"filippo.io/age"
)

// Writer writes a backup archive. It is not safe for concurrent use.
type Writer struct {
	enc     io.WriteCloser
	tw      *tar.Writer
	members map[string]string
	names   map[string]bool
	buf     []byte
	closed  bool
}

// NewWriter starts an archive on w, encrypted to recipient.
func NewWriter(w io.Writer, recipient age.Recipient) (*Writer, error) {
	enc, err := age.Encrypt(w, recipient)
	if err != nil {
		return nil, fmt.Errorf("start encryption: %w", err)
	}
	return &Writer{enc: enc, tw: tar.NewWriter(enc), members: map[string]string{}, names: map[string]bool{}}, nil
}

// Add writes a member of the given size. A size of -1 means a stream of unknown length: it is split into members
// name.000, name.001, ... of at most 16 MiB each, buffered in memory and never on disk.
// After a failed Add the archive is unusable and must be abandoned.
func (w *Writer) Add(name string, size int64, r io.Reader) error {
	if w.closed {
		return errors.New("archive is closed")
	}
	if err := validName(name); err != nil {
		return err
	}
	if w.names[name] {
		return fmt.Errorf("member %q added twice", name)
	}
	w.names[name] = true
	if size >= 0 {
		return w.addMember(name, size, r)
	}
	return w.addChunks(name, r)
}

func (w *Writer) addMember(name string, size int64, r io.Reader) error {
	if err := w.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	h := sha256.New()
	// io.CopyN would drop a source error returned with the last of the size bytes.
	n, err := io.Copy(io.MultiWriter(w.tw, h), io.LimitReader(r, size))
	if err != nil {
		return fmt.Errorf("write %s: got %d of %d bytes: %w", name, n, size, err)
	}
	if n < size {
		return fmt.Errorf("write %s: got %d of %d bytes: %w", name, n, size, io.ErrUnexpectedEOF)
	}
	var extra [1]byte
	k, err := io.ReadFull(r, extra[:])
	if k > 0 {
		return fmt.Errorf("write %s: more data than the declared %d bytes", name, size)
	}
	if err != io.EOF {
		return fmt.Errorf("read %s: %w", name, err)
	}
	w.members[name] = hex.EncodeToString(h.Sum(nil))
	return nil
}

func (w *Writer) addChunks(name string, r io.Reader) error {
	if w.buf == nil {
		w.buf = make([]byte, chunkSize)
	}
	for i := 0; ; i++ {
		n, err := io.ReadFull(r, w.buf)
		// Only the end conditions of io.ReadFull itself end the stream; a source error that wraps one is a failure.
		last := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !last {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if n == 0 && i > 0 {
			return nil
		}
		if err := w.addMember(chunkName(name, i), int64(n), bytes.NewReader(w.buf[:n])); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// Close writes the manifest as the last member and finishes the archive. The caller still closes the destination.
func (w *Writer) Close(m Manifest) error { return w.close(m, FormatVersion) }

func (w *Writer) close(m Manifest, format int) error {
	if w.closed {
		return errors.New("archive is closed")
	}
	w.closed = true
	m.Format = format
	m.Members = w.members
	if m.Time.IsZero() {
		m.Time = time.Now().UTC()
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := w.addMember(manifestName, int64(len(body)), bytes.NewReader(body)); err != nil {
		return err
	}
	delete(w.members, manifestName)
	if err := w.tw.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	if err := w.enc.Close(); err != nil {
		return fmt.Errorf("finish encryption: %w", err)
	}
	return nil
}
