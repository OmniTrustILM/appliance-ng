package addons

import (
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

const (
	testIndex = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testArm   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestPinnedReferencesTheManifestOfTheArch(t *testing.T) {
	agent := "image: \"quay.io/cilium/cilium:v1.20.4@" + testIndex + "\"\n"
	chart := "binary @" + testIndex
	fsys := Pinned(fstest.MapFS{
		"cilium.yaml":      {Data: []byte(agent + agent)},
		"charts/chart.tgz": {Data: []byte(chart)},
	}, map[string]string{testIndex: testArm})

	want := "image: \"quay.io/cilium/cilium:v1.20.4@" + testArm + "\"\n"
	if got, err := fs.ReadFile(fsys, "cilium.yaml"); err != nil || string(got) != want+want {
		t.Errorf("ReadFile = %q, %v; want %q", got, err, want+want)
	}
	f, err := fsys.Open("cilium.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got, err := io.ReadAll(f); err != nil || string(got) != want+want {
		t.Errorf("Open and read = %q, %v; want %q", got, err, want+want)
	}
	if info, err := f.Stat(); err != nil || info.Name() != "cilium.yaml" || info.Size() != int64(len(want+want)) || info.IsDir() {
		t.Errorf("Stat = %v, %v; want the manifest with its pinned size", info, err)
	}
	if got, err := fs.ReadFile(fsys, "charts/chart.tgz"); err != nil || string(got) != chart {
		t.Errorf("a file other than a manifest = %q, %v; want it unchanged", got, err)
	}
	if _, err := fs.ReadFile(fsys, "missing.yaml"); err == nil {
		t.Error("ReadFile of a missing file: want an error")
	}
	if _, err := fsys.Open("missing.yaml"); err == nil {
		t.Error("Open of a missing file: want an error")
	}
}

func TestPinnedWithoutPinsIsTheFiles(t *testing.T) {
	files := fstest.MapFS{"a.yaml": {Data: []byte("a")}}
	if got := Pinned(files, nil); got == nil {
		t.Fatal("Pinned returned nil")
	} else if _, ok := got.(fstest.MapFS); !ok {
		t.Errorf("Pinned without pins = %T, want the files themselves", got)
	}
}
