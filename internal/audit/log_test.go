package audit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func entry(i int) Entry {
	return Entry{
		Time:   time.Date(2026, 11, 18, 10, 0, i, 0, time.UTC),
		User:   "admin",
		Action: fmt.Sprintf("PUT /api/v1/appliance #%d", i),
		Result: "success",
		Paths:  []string{"spec.node.hostName"},
	}
}

func open(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir
}

func TestWriteAndTail(t *testing.T) {
	l, dir := open(t)
	for i := range 3 {
		if err := l.Write(entry(i)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := l.Tail(2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(2), entry(1)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail(2) = %+v, want newest first %+v", got, want)
	}
	all, err := l.Tail(10)
	if err != nil || len(all) != 3 {
		t.Errorf("Tail(10) = %d entries, %v; want 3", len(all), err)
	}
	if none, err := l.Tail(0); err != nil || len(none) != 0 {
		t.Errorf("Tail(0) = %v, %v; want none", none, err)
	}

	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o, want 600", mode)
	}
	data, _ := os.ReadFile(filepath.Join(dir, FileName))
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Errorf("%d lines, want one JSON line per entry:\n%s", lines, data)
	}
	if !strings.Contains(string(data), `"paths":["spec.node.hostName"]`) {
		t.Errorf("unexpected encoding:\n%s", data)
	}
}

func TestReopenAppends(t *testing.T) {
	l, dir := open(t)
	if err := l.Write(entry(0)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Write(entry(1)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(1), entry(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %+v, want %+v", got, want)
	}
}

func TestAuditRotation(t *testing.T) {
	l, dir := open(t)
	const n = 30
	line, err := encode(entry(n - 1))
	if err != nil {
		t.Fatal(err)
	}
	// Two entries of the same length fit in a file, so the last 20 fill the 10 kept files.
	l.maxBytes = int64(2 * len(line))
	for i := range n {
		if err := l.Write(entry(i)); err != nil {
			t.Fatal(err)
		}
	}

	names, err := filepath.Glob(filepath.Join(dir, FileName+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != MaxFiles {
		t.Errorf("files %v, want %d", names, MaxFiles)
	}
	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > l.maxBytes {
			t.Errorf("%s has %d bytes, more than %d", name, info.Size(), l.maxBytes)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode %o, want 600", name, mode)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%s.%d", FileName, MaxFiles))); !os.IsNotExist(err) {
		t.Errorf("a file beyond the kept ones exists: %v", err)
	}

	got, err := l.Tail(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2*MaxFiles {
		t.Fatalf("Tail kept %d entries, want %d", len(got), 2*MaxFiles)
	}
	if !reflect.DeepEqual(got[0], entry(n-1)) {
		t.Errorf("newest %+v, want %+v", got[0], entry(n-1))
	}
	if oldest := got[len(got)-1]; !reflect.DeepEqual(oldest, entry(n-2*MaxFiles)) {
		t.Errorf("oldest %+v, want %+v", oldest, entry(n-2*MaxFiles))
	}
}

func TestDefaultLimit(t *testing.T) {
	l, _ := open(t)
	if l.maxBytes != MaxBytes || MaxBytes != 10<<20 || MaxFiles != 10 {
		t.Errorf("limits %d bytes in %d files, want 10 MiB in 10 files", l.maxBytes, MaxFiles)
	}
}

func TestTailSkipsTornLine(t *testing.T) {
	l, dir := open(t)
	if err := l.Write(entry(0)); err != nil {
		t.Fatal(err)
	}
	// A crash in the middle of a write leaves a partial line.
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"2026-`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := l.Tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %+v, want %+v", got, want)
	}
}

func TestConcurrentWrites(t *testing.T) {
	l, _ := open(t)
	l.maxBytes = 4096
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			if err := l.Write(entry(i)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := l.Tail(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Errorf("Tail = %d entries, want 50", len(got))
	}
}

func TestOpenFailsOnAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Error("Open on a regular file succeeded")
	}
}

func TestWriteAfterClose(t *testing.T) {
	l, _ := open(t)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(entry(0)); err == nil {
		t.Error("Write after Close succeeded")
	}
}

func TestOpenEndsATornLine(t *testing.T) {
	l, dir := open(t)
	if err := l.Write(entry(0)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"2026-`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	l, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.Write(entry(1)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Tail(5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(1), entry(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %+v, want %+v", got, want)
	}
}

func TestWriteContinuesAfterAFailedRotation(t *testing.T) {
	l, dir := open(t)
	line, err := encode(entry(0))
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = int64(len(line))
	fail := true
	l.rename = func(oldpath, newpath string) error {
		if fail {
			return errors.New("disk trouble")
		}
		return os.Rename(oldpath, newpath)
	}
	for i := range 3 {
		if err := l.Write(entry(i)); err != nil {
			t.Fatalf("write %d with a failing rotation: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".1")); !os.IsNotExist(err) {
		t.Fatalf("a rotated file exists although rotation failed: %v", err)
	}

	fail = false
	if err := l.Write(entry(3)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName+".1")); err != nil {
		t.Errorf("the retried rotation left no rotated file: %v", err)
	}
	got, err := l.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(3), entry(2), entry(1), entry(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %+v, want every entry", got)
	}
}

func TestWriteAfterAPartialWriteStartsANewLine(t *testing.T) {
	l, _ := open(t)
	if err := l.Write(entry(0)); err != nil {
		t.Fatal(err)
	}
	l.write = func(f *os.File, p []byte) (int, error) {
		n, _ := f.Write(p[:len(p)/2])
		return n, errors.New("disk full")
	}
	if err := l.Write(entry(1)); err == nil {
		t.Fatal("a partial write reported no error")
	}
	l.write = (*os.File).Write
	if err := l.Write(entry(2)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Entry{entry(2), entry(0)}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tail = %+v, want the entries around the broken one", got)
	}
}

// pageAll follows the cursors from the newest entry to the oldest, n entries a page.
func pageAll(t *testing.T, l *Log, n int) []Entry {
	t.Helper()
	var all []Entry
	before := ""
	for range 100 {
		page, next, err := l.Page(before, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > n {
			t.Fatalf("page of %d entries, want at most %d", len(page), n)
		}
		all = append(all, page...)
		if next == "" {
			return all
		}
		before = next
	}
	t.Fatal("the cursors never end")
	return nil
}

func TestAuditPageBefore(t *testing.T) {
	l, _ := open(t)
	line, err := encode(entry(99))
	if err != nil {
		t.Fatal(err)
	}
	l.maxBytes = int64(3 * len(line))
	write := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			if err := l.Write(entry(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(0, 20)

	t.Run("pages cover the log newest first without gaps or repeats", func(t *testing.T) {
		for _, n := range []int{1, 2, 3, 4, 7, 20, 50} {
			var want []Entry
			for i := 19; i >= 0; i-- {
				want = append(want, entry(i))
			}
			if got := pageAll(t, l, n); !reflect.DeepEqual(got, want) {
				t.Errorf("pages of %d = %d entries, want all 20 newest first", n, len(got))
			}
		}
	})

	t.Run("a cursor keeps its place while entries are added and files rotate", func(t *testing.T) {
		first, next, err := l.Page("", 5)
		if err != nil || len(first) != 5 || next == "" {
			t.Fatalf("Page = %d entries, next %q, %v", len(first), next, err)
		}
		write(20, 27)
		second, _, err := l.Page(next, 5)
		if err != nil {
			t.Fatal(err)
		}
		if want := []Entry{entry(14), entry(13), entry(12), entry(11), entry(10)}; !reflect.DeepEqual(second, want) {
			t.Errorf("second page after new entries = %+v, want %+v", second, want)
		}
	})

	t.Run("a cursor into a dropped file ends the pages", func(t *testing.T) {
		_, next, err := l.Page("", 1)
		if err != nil {
			t.Fatal(err)
		}
		write(27, 27+3*MaxFiles+3)
		page, after, err := l.Page(next, 5)
		if err != nil || len(page) != 0 || after != "" {
			t.Errorf("Page of a dropped file = %d entries, next %q, %v; want none", len(page), after, err)
		}
	})

	t.Run("a cursor past the end or inside a line reads only the whole lines before it", func(t *testing.T) {
		_, next, err := l.Page("", 1)
		if err != nil {
			t.Fatal(err)
		}
		c, err := parseCursor(next)
		if err != nil {
			t.Fatal(err)
		}
		newest, _, err := l.Page("", 3)
		if err != nil {
			t.Fatal(err)
		}
		for name, tc := range map[string]struct {
			offset int64
			want   []Entry
		}{
			"past the end":  {c.offset + 1<<20, newest[:2]},
			"inside a line": {c.offset + int64(len(line))/2, newest[1:3]},
		} {
			page, _, err := l.Page(cursor{file: c.file, offset: tc.offset}.String(), 2)
			if err != nil || !reflect.DeepEqual(page, tc.want) {
				t.Errorf("%s: Page = %+v, %v; want %+v", name, page, err, tc.want)
			}
		}
	})

	t.Run("an invalid cursor is refused", func(t *testing.T) {
		for _, cursor := range []string{"!", "eA", "MTIz"} {
			if _, _, err := l.Page(cursor, 5); !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("Page(%q) = %v, want ErrInvalidCursor", cursor, err)
			}
		}
	})
}

func TestFileIDStableOnceATornFirstLineEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(`{"user":"tor`), 0o600); err != nil {
		t.Fatal(err)
	}
	torn, err := fileID(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("\n{}\n")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if ended, err := fileID(path); err != nil || ended != torn {
		t.Errorf("fileID changed from %x to %x (%v) when the torn first line ended", torn, ended, err)
	}
}
