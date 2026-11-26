package addons

import (
	"bytes"
	"io/fs"
	"path"
	"strings"
)

// Pinned returns fsys with each digest in pins replaced, in the add-on manifests, by the digest it maps to. A pod
// then references the image manifest of the node's arch rather than the index the release pins, which a registry
// filled from a bundle of one arch does not hold.
func Pinned(fsys fs.FS, pins map[string]string) fs.FS {
	if len(pins) == 0 {
		return fsys
	}
	pairs := make([]string, 0, 2*len(pins))
	for from, to := range pins {
		pairs = append(pairs, "@"+from, "@"+to)
	}
	return pinnedFS{FS: fsys, pins: strings.NewReplacer(pairs...)}
}

type pinnedFS struct {
	fs.FS
	pins *strings.Replacer
}

func (p pinnedFS) ReadFile(name string) ([]byte, error) {
	data, err := fs.ReadFile(p.FS, name)
	if err != nil || path.Ext(name) != ".yaml" {
		return data, err
	}
	return []byte(p.pins.Replace(string(data))), nil
}

func (p pinnedFS) Open(name string) (fs.File, error) {
	if path.Ext(name) != ".yaml" {
		return p.FS.Open(name)
	}
	data, err := p.ReadFile(name)
	if err != nil {
		return nil, err
	}
	info, err := fs.Stat(p.FS, name)
	if err != nil {
		return nil, err
	}
	return &pinnedFile{Reader: bytes.NewReader(data), info: pinnedInfo{FileInfo: info, size: int64(len(data))}}, nil
}

// pinnedFile is a read-only add-on manifest with its digests replaced.
type pinnedFile struct {
	*bytes.Reader
	info pinnedInfo
}

func (f *pinnedFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *pinnedFile) Close() error               { return nil }

// pinnedInfo is the file information of the original manifest with the size of the pinned one.
type pinnedInfo struct {
	fs.FileInfo
	size int64
}

func (i pinnedInfo) Size() int64 { return i.size }

// Replaced returns fsys with the file name holding data instead.
func Replaced(fsys fs.FS, name string, data []byte) fs.FS {
	return replacedFS{FS: fsys, name: name, data: data}
}

type replacedFS struct {
	fs.FS
	name string
	data []byte
}

func (r replacedFS) ReadFile(name string) ([]byte, error) {
	if name != r.name {
		return fs.ReadFile(r.FS, name)
	}
	return r.data, nil
}

func (r replacedFS) Open(name string) (fs.File, error) {
	if name != r.name {
		return r.FS.Open(name)
	}
	info, err := fs.Stat(r.FS, name)
	if err != nil {
		return nil, err
	}
	return &pinnedFile{Reader: bytes.NewReader(r.data), info: pinnedInfo{FileInfo: info, size: int64(len(r.data))}}, nil
}
