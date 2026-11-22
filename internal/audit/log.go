// Package audit keeps the console's audit log: one JSON line per mutating request, in a file rotated by size.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
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
		return fmt.Errorf("open the audit file: %w", err)
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
		return 0, fmt.Errorf("read the audit file: %w", err)
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

// Tail returns up to n of the most recent entries, newest first. Lines that do not decode, such as one torn by a
// crash, are skipped.
func (l *Log) Tail(n int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var entries []Entry
	for i := 0; i < MaxFiles && len(entries) < n; i++ {
		lines, err := readEntries(l.path(i))
		if err != nil {
			return nil, err
		}
		slices.Reverse(lines)
		entries = append(entries, lines[:min(len(lines), n-len(entries))]...)
	}
	return entries, nil
}

// readEntries returns the entries of a file, oldest first; a missing file has none.
func readEntries(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open the audit file: %w", err)
	}
	defer func() { _ = f.Close() }()
	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), MaxBytes)
	for scanner.Scan() {
		var e Entry
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			entries = append(entries, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the audit file: %w", err)
	}
	return entries, nil
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
