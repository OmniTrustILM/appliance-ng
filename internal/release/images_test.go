package release

import (
	"reflect"
	"testing"
)

func TestCacheListFiltersArchAndAddsInstaller(t *testing.T) {
	m := &Manifest{Images: []Image{
		{Ref: "z/last:1", Digest: "sha256:1"},
		{Ref: "a/amd-only:1", Digest: "sha256:2", Arches: []string{"amd64"}},
		{Ref: "a/first:1@sha256:3", Digest: "sha256:3"},
		{Ref: "z/last:1", Digest: "sha256:1"},
	}}

	got := CacheList(m, "arm64", "factory/installer:v1")
	want := []string{"a/first:1@sha256:3", "factory/installer:v1", "z/last:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arm64: got %v want %v", got, want)
	}

	got = CacheList(m, "amd64", "")
	want = []string{"a/amd-only:1", "a/first:1@sha256:3", "z/last:1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("amd64: got %v want %v", got, want)
	}
}
