// Package audit keeps the console's audit log: one JSON line per mutating request, in a file rotated by size.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Formats of the errors of the audit files.
const (
	errOpen = "open the audit file: %w"
	errRead = "read the audit file: %w"
)

const (
	// FileName is the current audit file; rotated files carry the suffixes .1 (newest) to .9 (oldest).
	FileName = "audit.log"
	// MaxBytes is the size at which the current file is rotated.
	MaxBytes = 10 << 20
	// MaxFiles is the number of files kept, the current one included.
	MaxFiles = 10
	fileMode = 0o600
	dirMode  = 0o700
)

// SystemUser is the user of the entries the manager writes on its own.
const SystemUser = "system:manager"

// Entry is one audited request. Paths are the JSON paths of the Appliance fields the request changed, never their
// values; Details describe an issued or revoked credential, never its secret.
type Entry struct {
	Time    time.Time         `json:"time"`
	User    string            `json:"user"`
	Action  string            `json:"action"`
	Result  string            `json:"result"`
	Paths   []string          `json:"paths,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// Log appends entries to the audit file of a directory. It is safe for concurrent use.
type Log struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	size     int64
	maxBytes int64
	rename   func(oldpath, newpath string) error
	write    func(f *os.File, p []byte) (int, error)
	// torn is set when a write broke off within its line; the next entry then starts a line of its own.
	torn bool
}

// Open opens, or creates, the audit file in dir for appending.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create the audit directory: %w", err)
	}
	l := &Log{dir: dir, maxBytes: MaxBytes, rename: os.Rename, write: (*os.File).Write}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) path(index int) string {
	if index == 0 {
		return filepath.Join(l.dir, FileName)
	}
	return filepath.Join(l.dir, fmt.Sprintf("%s.%d", FileName, index))
}

// openFile opens the current file for appending. A last line without its newline, left by a crash in the middle of
// a write, is ended first, so the next entry starts a line of its own.
func (l *Log) openFile() error {
	f, err := os.OpenFile(l.path(0), os.O_APPEND|os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return fmt.Errorf(errOpen, err)
	}
	size, err := endLine(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	l.file, l.size = f, size
	return nil
}

// endLine appends a newline to f unless it is empty or ends with one, and returns its size.
func endLine(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("read the size of the audit file: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return 0, fmt.Errorf(errRead, err)
	}
	if last[0] == '\n' {
		return size, nil
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return 0, fmt.Errorf("end the torn line of the audit file: %w", err)
	}
	return size + 1, nil
}

func encode(e Entry) ([]byte, error) {
	line, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode the audit entry: %w", err)
	}
	return append(line, '\n'), nil
}

// Write appends e and syncs it to disk, rotating first when e would take the current file past MaxBytes.
func (l *Log) Write(e Entry) error {
	line, err := encode(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return errors.New("the audit log is closed")
	}
	// A failed rotation keeps the current file open, so the entry is still written and the next write retries.
	if l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		if err := l.rotate(); err != nil && l.file == nil {
			return err
		}
	}
	if l.torn {
		line = append([]byte{'\n'}, line...)
	}
	n, err := l.write(l.file, line)
	l.size += int64(n)
	if err != nil {
		l.torn = l.torn || n > 0
		return fmt.Errorf("write the audit file: %w", err)
	}
	l.torn = false
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync the audit file: %w", err)
	}
	return nil
}

// rotate shifts every file one suffix up, dropping the oldest, and starts an empty current file. Whatever fails, it
// reopens the current file, so writes go on.
func (l *Log) rotate() error {
	err := l.file.Close()
	l.file = nil
	if err == nil {
		err = l.shift()
	}
	if oerr := l.openFile(); oerr != nil {
		return errors.Join(err, oerr)
	}
	if err != nil {
		return fmt.Errorf("rotate the audit file: %w", err)
	}
	return nil
}

func (l *Log) shift() error {
	for i := MaxFiles - 1; i > 0; i-- {
		if err := l.rename(l.path(i-1), l.path(i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDir(l.dir)
}

// syncDir makes the renames in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// ErrInvalidCursor is returned by Page for a cursor it did not issue.
var ErrInvalidCursor = errors.New("the audit cursor is not valid")

// Tail returns up to n of the most recent entries, newest first.
func (l *Log) Tail(n int) ([]Entry, error) {
	entries, _, err := l.Page("", n)
	return entries, err
}

// Page returns up to n entries, newest first, that were written before the position of the cursor before, or the
// newest ones when before is empty. next is the cursor of the following page, empty when no older entry is left. A
// cursor names its file by the file's first line, so it keeps its place while files rotate; once its file is
// dropped, the page is empty. Lines that do not decode, such as one torn by a crash, are skipped.
func (l *Log) Page(before string, n int) (entries []Entry, next string, err error) {
	from, err := optionalCursor(before)
	if err != nil {
		return nil, "", err
	}
	files, err := l.snapshot()
	if err != nil {
		return nil, "", err
	}
	defer closeAll(files)
	var positions []cursor
	for i := 0; i < len(files) && len(entries) <= n && n > 0; i++ {
		id, err := fileID(files[i])
		if err != nil {
			return nil, "", err
		}
		limit := files[i].size
		if from != nil {
			if id != from.file {
				continue
			}
			limit, from = min(from.offset, limit), nil
		}
		lines, offsets, err := readEntries(files[i], limit)
		if err != nil {
			return nil, "", err
		}
		entries, positions = appendNewest(entries, positions, lines, offsets, id, n)
	}
	if len(entries) > n {
		entries, next = entries[:n], positions[n-1].String()
	}
	return entries, next, nil
}

// snapshot is a file of the log, open, with its size when it was opened; a missing file has no file.
type snapshot struct {
	file *os.File
	size int64
}

// snapshot opens every file of the log, newest first, as they are between two writes. Files are only appended to
// and renamed, so they can be read up to their sizes without holding up the writes.
func (l *Log) snapshot() ([]snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	files := make([]snapshot, 0, MaxFiles)
	for i := range MaxFiles {
		f, err := os.Open(l.path(i))
		if errors.Is(err, os.ErrNotExist) {
			files = append(files, snapshot{})
			continue
		}
		if err != nil {
			closeAll(files)
			return nil, fmt.Errorf(errOpen, err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			closeAll(files)
			return nil, fmt.Errorf(errRead, err)
		}
		files = append(files, snapshot{file: f, size: info.Size()})
	}
	return files, nil
}

func closeAll(files []snapshot) {
	for _, s := range files {
		if s.file != nil {
			_ = s.file.Close()
		}
	}
}

// appendNewest appends the lines of the file id, newest first, with their positions at offsets, until entries holds
// one more than n.
func appendNewest(entries []Entry, positions []cursor, lines []Entry, offsets []int64, id [8]byte, n int) ([]Entry, []cursor) {
	for j := len(lines) - 1; j >= 0 && len(entries) <= n; j-- {
		entries = append(entries, lines[j])
		positions = append(positions, cursor{file: id, offset: offsets[j]})
	}
	return entries, positions
}

// cursor is the position of an entry: the identity of its file and the offset of its line.
type cursor struct {
	file   [8]byte
	offset int64
}

func (c cursor) String() string {
	b := make([]byte, 0, 16)
	b = append(b, c.file[:]...)
	return base64.RawURLEncoding.EncodeToString(binary.BigEndian.AppendUint64(b, uint64(c.offset)))
}

// optionalCursor parses the cursor s, nil when s is empty.
func optionalCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	c, err := parseCursor(s)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func parseCursor(s string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 16 {
		return cursor{}, ErrInvalidCursor
	}
	c := cursor{offset: int64(binary.BigEndian.Uint64(b[8:]))}
	copy(c.file[:], b[:8])
	if c.offset < 0 {
		return cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// fileID identifies a file by the start of the SHA-256 of its first line without the newline, which neither rotation
// nor the ending of a torn first line changes; a missing or empty file has the zero identity.
func fileID(s snapshot) ([8]byte, error) {
	var id [8]byte
	if s.file == nil {
		return id, nil
	}
	first, err := bufio.NewReader(io.NewSectionReader(s.file, 0, s.size)).ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return id, fmt.Errorf(errRead, err)
	}
	if len(first) > 0 {
		sum := sha256.Sum256(bytes.TrimSuffix(first, []byte{'\n'}))
		copy(id[:], sum[:])
	}
	return id, nil
}

// readEntries returns the entries of a file, oldest first, with the offsets of their lines, reading only the lines
// that end before limit. A missing file has none.
func readEntries(s snapshot, limit int64) ([]Entry, []int64, error) {
	if s.file == nil {
		return nil, nil, nil
	}
	var (
		entries []Entry
		offsets []int64
		offset  int64
	)
	scanner := bufio.NewScanner(io.NewSectionReader(s.file, 0, limit))
	scanner.Buffer(make([]byte, 0, 64<<10), MaxBytes)
	for scanner.Scan() {
		var e Entry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			entries, offsets = append(entries, e), append(offsets, offset)
		}
		offset += int64(len(scanner.Bytes())) + 1
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf(errRead, err)
	}
	return entries, offsets, nil
}

// Close closes the current file; later writes fail.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
