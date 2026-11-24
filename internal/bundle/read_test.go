package bundle

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// tarItem is an entry of a hand-made tar: a header with its content, or PAX records written raw before the next one.
type tarItem struct {
	h       tar.Header
	content string
	pax     map[string]string
}

func TestExtractHostileEntries(t *testing.T) {
	reg := func(name, content string) tarItem {
		return tarItem{h: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}, content: content}
	}
	special := func(flag byte, link string) tarItem {
		return tarItem{h: tar.Header{Name: "index.json", Typeflag: flag, Mode: 0o644, Linkname: link}}
	}
	cases := []struct {
		name    string
		items   []tarItem
		refused bool
	}{
		{name: "symlink", items: []tarItem{special(tar.TypeSymlink, "/etc/passwd")}},
		{name: "hardlink", items: []tarItem{special(tar.TypeLink, "oci-layout")}},
		{name: "char device", items: []tarItem{special(tar.TypeChar, "")}},
		{name: "block device", items: []tarItem{special(tar.TypeBlock, "")}},
		{name: "fifo", items: []tarItem{special(tar.TypeFifo, "")}},
		{name: "parent path", items: []tarItem{reg("../index.json", "{}")}},
		{name: "parent path inside blobs", items: []tarItem{reg("blobs/sha256/../../../escape", "x")}},
		{name: "absolute path", items: []tarItem{reg("/index.json", "{}")}},
		{name: "blob name not hex", items: []tarItem{reg("blobs/sha256/"+strings.Repeat("z", 64), "x")}},
		{name: "blob name too short", items: []tarItem{reg("blobs/sha256/"+strings.Repeat("a", 63), "x")}},
		{name: "blob name in uppercase", items: []tarItem{reg("blobs/sha256/"+strings.Repeat("A", 64), "x")}},
		{name: "duplicate entry", items: []tarItem{reg("index.json", "{}"), reg("index.json", "{}")}, refused: true},
		{name: "PAX sparse entry", refused: true, items: []tarItem{{
			pax: map[string]string{"GNU.sparse.size": "104857600", "GNU.sparse.numblocks": "1", "GNU.sparse.map": "0,5"},
			h:   tar.Header{Name: "index.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}, content: "hello",
		}}},
		{name: "GNU sparse entry", refused: true, items: []tarItem{{
			h: tar.Header{Name: "index.json", Typeflag: tar.TypeGNUSparse, Mode: 0o644, Format: tar.FormatGNU},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "bundle.tar")
			writeTar(t, archive, append([]tarItem{reg("oci-layout", "{}")}, tc.items...))
			dir := filepath.Join(root, "layout")
			if err := os.Mkdir(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			err := extract(archive, dir)
			if tc.refused != (err != nil) {
				t.Fatalf("extract = %v, want refused %v", err, tc.refused)
			}
			want := []string{"bundle.tar", "layout", "layout/oci-layout"}
			if tc.name == "duplicate entry" {
				want = append(want, "layout/index.json")
			}
			if got := filesUnder(t, root); !tc.refused && !slices.Equal(got, want) {
				t.Errorf("files = %v, want %v", got, want)
			}
			for _, f := range filesUnder(t, root) {
				if f != "bundle.tar" && f != "layout" && !strings.HasPrefix(f, "layout/") {
					t.Errorf("extract wrote %s outside the layout", f)
				}
			}
		})
	}
}

func TestExtractRefusesSparseExpansion(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "bundle.tar")
	writeTar(t, archive, []tarItem{{
		pax: map[string]string{"GNU.sparse.size": "104857600", "GNU.sparse.numblocks": "1", "GNU.sparse.map": "0,5"},
		h:   tar.Header{Name: "index.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}, content: "hello",
	}})
	dir := filepath.Join(root, "layout")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := extract(archive, dir); err == nil {
		t.Fatal("extract: want an error")
	}
	if info, err := os.Stat(filepath.Join(dir, "index.json")); err == nil && info.Size() > 5 {
		t.Errorf("extract wrote %d bytes of a 5-byte entry", info.Size())
	}
}

// filesUnder returns the sorted slash paths of everything under root.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, err := filepath.Rel(root, p)
		paths = append(paths, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

// writeTar writes items to path as a tar. PAX records are written as a raw extended header: tar.Writer drops the
// GNU.sparse keys.
func writeTar(t *testing.T, path string, items []tarItem) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, it := range items {
		if it.pax != nil {
			writePAXHeader(t, tw, &buf, it.pax)
			it.h.Format = tar.FormatUSTAR
		}
		if err := tw.WriteHeader(&it.h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(it.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePAXHeader(t *testing.T, tw *tar.Writer, buf *bytes.Buffer, records map[string]string) {
	t.Helper()
	var data strings.Builder
	for _, k := range slices.Sorted(maps.Keys(records)) {
		data.WriteString(paxRecord(k, records[k]))
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	start := buf.Len()
	if err := tw.WriteHeader(&tar.Header{Name: "PaxHeader", Typeflag: tar.TypeReg, Size: int64(data.Len()), Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	block := buf.Bytes()[start : start+tarBlock]
	block[156] = tar.TypeXHeader
	copy(block[148:156], "        ")
	sum := 0
	for _, c := range block {
		sum += int(c)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
	if _, err := tw.Write([]byte(data.String())); err != nil {
		t.Fatal(err)
	}
}

// paxRecord encodes one PAX record, "<length> <key>=<value>\n", where the length counts itself.
func paxRecord(k, v string) string {
	s := " " + k + "=" + v + "\n"
	n := len(s) + 1
	for len(strconv.Itoa(n))+len(s) != n {
		n++
	}
	return strconv.Itoa(n) + s
}
