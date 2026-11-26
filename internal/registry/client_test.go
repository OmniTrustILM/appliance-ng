package registry

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	ggcr "github.com/google/go-containerregistry/pkg/registry"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const testPassword = "correct-horse-battery"

// newZot starts an in-process registry behind TLS, optionally wrapped by wrap, and returns its address and CA pool.
func newZot(t *testing.T, wrap func(http.Handler) http.Handler) (string, *x509.CertPool) {
	t.Helper()
	h := http.Handler(ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0))))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return strings.TrimPrefix(srv.URL, "https://"), pool
}

func newClient(t *testing.T) (*Client, string) {
	t.Helper()
	addr, pool := newZot(t, nil)
	return New(addr, pool, PushUser, ""), addr
}

func newImage(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func digestOf(t *testing.T, d interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	h, err := d.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

// fakeBundle is a verified bundle: images in metadata order and the bundle index naming them.
type fakeBundle struct {
	images []bundle.Image
	index  v1.ImageIndex
	err    error
}

func (f *fakeBundle) Images() ([]bundle.Image, error) { return f.images, f.err }
func (f *fakeBundle) Index() (v1.ImageIndex, error)   { return f.index, f.err }
func (f *fakeBundle) metadata(version string) bundle.Metadata {
	return bundle.Metadata{Kind: "system", Version: version, Arch: "amd64"}
}

// newBundle builds a bundle of the images by upstream reference, plus a metadata artifact the way a built bundle has.
func newBundle(t *testing.T, images map[string]v1.Image) *fakeBundle {
	t.Helper()
	f := &fakeBundle{}
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: newImage(t)})
	for _, ref := range slices.Sorted(maps.Keys(images)) {
		img := images[ref]
		f.images = append(f.images, bundle.Image{Ref: bundle.ImageRef{Ref: ref, Digest: digestOf(t, img), Size: 700}, Image: img})
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Annotations: map[string]string{annotationRefName: ref}},
		})
	}
	f.index = idx
	return f
}

func exists(t *testing.T, c *Client, ref string) bool {
	t.Helper()
	r, err := name.ParseReference(c.host + "/" + ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Head(r, c.options(t.Context())...)
	return err == nil
}

func mustTag(t *testing.T, ref string) name.Tag {
	t.Helper()
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func TestPushBundleUnderUpstreamNames(t *testing.T) {
	c, _ := newClient(t)
	images := map[string]v1.Image{
		"ghcr.io/project-zot/zot-minimal:v2.1.0":                          newImage(t),
		"quay.io/cilium/cilium:v1.18.0@sha256:" + strings.Repeat("a", 64): newImage(t),
		"registry.k8s.io/pause@sha256:" + strings.Repeat("b", 64):         newImage(t),
	}
	b := newBundle(t, images)

	var updates [][2]int64
	err := c.PushBundle(t.Context(), b, b.metadata("1.2.3"), func(done, total int64) { updates = append(updates, [2]int64{done, total}) })
	if err != nil {
		t.Fatal(err)
	}

	for _, im := range b.images {
		repo, tag, err := splitRef(im.Ref.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if !exists(t, c, repo+"@"+im.Ref.Digest) {
			t.Errorf("%s: not in zot by digest %s", repo, im.Ref.Digest)
		}
		if tag != "" && !exists(t, c, repo+":"+tag) {
			t.Errorf("%s: not in zot by tag %s", repo, tag)
		}
	}
	if !exists(t, c, "appliance/system:1.2.3-amd64") {
		t.Error("the bundle index is not in zot under appliance/system:1.2.3-amd64")
	}
	got, err := c.Bundles(t.Context(), "system")
	if err != nil || !slices.Equal(got, []string{"1.2.3-amd64"}) {
		t.Errorf("Bundles = %v, %v; want [1.2.3-amd64]", got, err)
	}
	if len(updates) == 0 {
		t.Fatal("no progress reported")
	}
	last := updates[len(updates)-1]
	if last[0] != last[1] || last[1] != 3*700 {
		t.Errorf("last progress = %v, want done = total = 2100", last)
	}
	for i := 1; i < len(updates); i++ {
		if updates[i][0] < updates[i-1][0] {
			t.Fatalf("progress went back: %v", updates)
		}
	}
}

func TestPushBundleReportsSourceErrors(t *testing.T) {
	c, _ := newClient(t)
	b := &fakeBundle{err: errors.New("unreadable")}
	if err := c.PushBundle(t.Context(), b, b.metadata("1"), nil); err == nil {
		t.Error("want the source error")
	}
	b = newBundle(t, map[string]v1.Image{"ghcr.io/": newImage(t)})
	if err := c.PushBundle(t.Context(), b, b.metadata("1"), nil); err == nil {
		t.Error("want an error for a reference without a repository")
	}
}

// recorder wraps a registry: it records the blobs uploaded per repository and refuses manifests of refuse's repository.
type recorder struct {
	mu      sync.Mutex
	uploads map[string][]string
	refuse  string
}

func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		refuse := r.refuse
		if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/blobs/uploads/") {
			repo := strings.TrimPrefix(req.URL.Path[:strings.Index(req.URL.Path, "/blobs/")], "/v2/")
			r.uploads[repo] = append(r.uploads[repo], req.URL.Query().Get("digest"))
		}
		r.mu.Unlock()
		if req.Method == http.MethodPut && refuse != "" && strings.Contains(req.URL.Path, "/"+refuse+"/manifests/") {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploads = map[string][]string{}
}

func TestPushResumesAfterFailure(t *testing.T) {
	rec := &recorder{uploads: map[string][]string{}, refuse: "quay.io/second"}
	addr, pool := newZot(t, rec.wrap)
	c := New(addr, pool, PushUser, "")
	b := newBundle(t, map[string]v1.Image{
		"ghcr.io/first:v1":  newImage(t),
		"quay.io/second:v1": newImage(t),
	})

	if err := c.PushBundle(t.Context(), b, b.metadata("1.2.3"), nil); err == nil {
		t.Fatal("the first push: want the refused manifest to fail it")
	}
	if len(rec.uploads["ghcr.io/first"]) == 0 {
		t.Fatal("the first push did not upload the first image")
	}
	if got, _ := c.Bundles(t.Context(), "system"); len(got) != 0 {
		t.Fatalf("Bundles after a failed push = %v, want none", got)
	}

	rec.reset()
	rec.mu.Lock()
	rec.refuse = ""
	rec.mu.Unlock()
	if err := c.PushBundle(t.Context(), b, b.metadata("1.2.3"), nil); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"ghcr.io/first", "quay.io/second"} {
		if n := len(rec.uploads[repo]); n != 0 {
			t.Errorf("the second push uploaded %d blobs to %s again", n, repo)
		}
	}
	if got, _ := c.Bundles(t.Context(), "system"); !slices.Equal(got, []string{"1.2.3-amd64"}) {
		t.Errorf("Bundles after the second push = %v", got)
	}
}

func TestPushNeedsPassword(t *testing.T) {
	addr, pool := newZot(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !ok || u != PushUser || p != testPassword {
				w.Header().Set("WWW-Authenticate", `Basic realm="zot"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	b := newBundle(t, map[string]v1.Image{"ghcr.io/app:v1": newImage(t)})

	for _, password := range []string{"", "wrong-password"} {
		err := New(addr, pool, PushUser, password).PushBundle(t.Context(), b, b.metadata("1"), nil)
		if err == nil {
			t.Fatalf("push with password %q: want an error", password)
		}
		if password != "" && strings.Contains(err.Error(), password) {
			t.Errorf("the error shows the password: %v", err)
		}
	}
	c := New(addr, pool, PushUser, testPassword)
	if err := c.PushBundle(t.Context(), b, b.metadata("1"), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Bundles(t.Context(), "system"); err != nil || !slices.Equal(got, []string{"1-amd64"}) {
		t.Errorf("Bundles = %v, %v", got, err)
	}
}

func TestPruneKeepsSharedDigests(t *testing.T) {
	c, _ := newClient(t)
	shared, onlyOld, onlyNew := newImage(t), newImage(t), newImage(t)
	oldMoving, newMoving := newImage(t), newImage(t)
	old := newBundle(t, map[string]v1.Image{
		"ghcr.io/shared:v1":  shared,
		"ghcr.io/old:v1":     onlyOld,
		"ghcr.io/moving:tag": oldMoving,
	})
	cur := newBundle(t, map[string]v1.Image{
		"ghcr.io/shared:v1":  shared,
		"ghcr.io/new:v2":     onlyNew,
		"ghcr.io/moving:tag": newMoving,
	})
	for version, b := range map[string]*fakeBundle{"1.0.0": old, "2.0.0": cur} {
		if err := c.PushBundle(t.Context(), b, b.metadata(version), nil); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Prune(t.Context(), "system", []string{"2.0.0-amd64"}); err != nil {
		t.Fatal(err)
	}

	for ref, want := range map[string]bool{
		"ghcr.io/shared@" + digestOf(t, shared):    true,
		"ghcr.io/shared:v1":                        true,
		"ghcr.io/new@" + digestOf(t, onlyNew):      true,
		"ghcr.io/moving@" + digestOf(t, newMoving): true,
		"ghcr.io/moving:tag":                       true,
		"ghcr.io/old@" + digestOf(t, onlyOld):      false,
		"ghcr.io/old:v1":                           false,
		"ghcr.io/moving@" + digestOf(t, oldMoving): false,
		"appliance/system:1.0.0-amd64":             false,
		"appliance/system:2.0.0-amd64":             true,
	} {
		if got := exists(t, c, ref); got != want {
			t.Errorf("%s exists = %v, want %v", ref, got, want)
		}
	}
	if got, err := c.Bundles(t.Context(), "system"); err != nil || !slices.Equal(got, []string{"2.0.0-amd64"}) {
		t.Errorf("Bundles = %v, %v; want [2.0.0-amd64]", got, err)
	}
}

// TestPruneKeepsImagesOfTheOtherKind keeps an image that moved from a system bundle to a platform bundle when the
// system bundle is pruned.
func TestPruneKeepsImagesOfTheOtherKind(t *testing.T) {
	c, _ := newClient(t)
	moved, onlySystem := newImage(t), newImage(t)
	sys := newBundle(t, map[string]v1.Image{"ghcr.io/moved:v1": moved, "ghcr.io/system:v1": onlySystem})
	if err := c.PushBundle(t.Context(), sys, sys.metadata("1.0.0"), nil); err != nil {
		t.Fatal(err)
	}
	plat := newBundle(t, map[string]v1.Image{"ghcr.io/moved:v1": moved})
	platform := bundle.Metadata{Kind: release.BundlePlatform, Version: "2.0.0", Arch: "amd64"}
	if err := c.PushBundle(t.Context(), plat, platform, nil); err != nil {
		t.Fatal(err)
	}

	if err := c.Prune(t.Context(), release.BundleSystem, nil); err != nil {
		t.Fatal(err)
	}

	for ref, want := range map[string]bool{
		"ghcr.io/moved@" + digestOf(t, moved):       true,
		"ghcr.io/moved:v1":                          true,
		"ghcr.io/system@" + digestOf(t, onlySystem): false,
		"appliance/system:1.0.0-amd64":              false,
		"appliance/platform:2.0.0-amd64":            true,
	} {
		if got := exists(t, c, ref); got != want {
			t.Errorf("%s exists = %v, want %v", ref, got, want)
		}
	}
}

// referencedGuard refuses, as zot does, to delete a manifest of a repository while an index there still lists it.
type referencedGuard struct {
	mu       sync.Mutex
	children map[string]string // child digest -> digest of the index listing it
	deleted  map[string]bool
}

func (g *referencedGuard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ref, ok := strings.Cut(r.URL.Path, "/manifests/")
		if r.Method == http.MethodDelete && ok && strings.HasPrefix(r.URL.Path, "/v2/appliance/") {
			g.mu.Lock()
			parent, listed := g.children[ref]
			refused := listed && !g.deleted[parent]
			g.deleted[ref] = !refused
			g.mu.Unlock()
			if refused {
				http.Error(w, `{"errors":[{"code":"DENIED","message":"manifest is part of an index image"}]}`, http.StatusMethodNotAllowed)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func TestPruneDeletesTheIndexBeforeItsCopies(t *testing.T) {
	guard := &referencedGuard{children: map[string]string{}, deleted: map[string]bool{}}
	addr, pool := newZot(t, guard.wrap)
	c := New(addr, pool, PushUser, "")
	old := newBundle(t, map[string]v1.Image{"ghcr.io/old:v1": newImage(t)})
	cur := newBundle(t, map[string]v1.Image{"ghcr.io/new:v2": newImage(t)})
	for version, b := range map[string]*fakeBundle{"1.0.0": old, "2.0.0": cur} {
		if err := c.PushBundle(t.Context(), b, b.metadata(version), nil); err != nil {
			t.Fatal(err)
		}
		im, err := b.index.IndexManifest()
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range im.Manifests {
			guard.children[d.Digest.String()] = digestOf(t, b.index)
		}
	}

	if err := c.Prune(t.Context(), "system", []string{"2.0.0-amd64"}); err != nil {
		t.Fatal(err)
	}

	oldIndex, err := old.index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range oldIndex.Manifests {
		if exists(t, c, "appliance/system@"+d.Digest.String()) {
			t.Errorf("the copy %s of the pruned bundle is still in zot", d.Digest)
		}
	}
	if exists(t, c, "appliance/system@"+digestOf(t, old.index)) {
		t.Error("the pruned bundle index is still in zot")
	}
	if got, err := c.Bundles(t.Context(), "system"); err != nil || !slices.Equal(got, []string{"2.0.0-amd64"}) {
		t.Errorf("Bundles = %v, %v; want [2.0.0-amd64]", got, err)
	}
}

var _ Source = (*bundle.Bundle)(nil)

func TestSplitRefOfReleaseImages(t *testing.T) {
	m, err := release.Load("../../release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range m.Images {
		repo, _, err := splitRef(img.Ref)
		if err != nil {
			t.Errorf("%s: %v", img.Ref, err)
			continue
		}
		host, path, _ := strings.Cut(repo, "/")
		if host == "docker.io" && !strings.Contains(path, "/") {
			t.Errorf("%s maps to %q, a Docker Hub repository without a namespace", img.Ref, repo)
		}
		if _, err := name.NewRepository("zot.example/" + repo); err != nil || !strings.Contains(host, ".") {
			t.Errorf("%s maps to %q, not a repository with a host: %v", img.Ref, repo, err)
		}
	}
}

func TestPruneSkipsUnreadableBundle(t *testing.T) {
	c, _ := newClient(t)
	good := newBundle(t, map[string]v1.Image{"ghcr.io/app:v2": newImage(t)})
	if err := c.PushBundle(t.Context(), good, good.metadata("2.0.0"), nil); err != nil {
		t.Fatal(err)
	}
	// A tag whose manifest is not an index cannot be read as a bundle.
	repo, err := c.repository("appliance/system")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Tag("0.9.0-amd64"), newImage(t), c.options(t.Context())...); err != nil {
		t.Fatal(err)
	}

	if err := c.Prune(t.Context(), "system", []string{"2.0.0-amd64"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Bundles(t.Context(), "system"); err != nil || !slices.Equal(got, []string{"2.0.0-amd64"}) {
		t.Errorf("Bundles = %v, %v; want [2.0.0-amd64]", got, err)
	}
}

func TestPruneStopsOnTransientErrors(t *testing.T) {
	failing := false
	addr, pool := newZot(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if failing && strings.Contains(r.URL.Path, "/manifests/") {
				http.Error(w, "down", http.StatusNotImplemented)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	c := New(addr, pool, PushUser, "")
	old := newBundle(t, map[string]v1.Image{"ghcr.io/app:v1": newImage(t)})
	if err := c.PushBundle(t.Context(), old, old.metadata("1.0.0"), nil); err != nil {
		t.Fatal(err)
	}
	failing = true
	if err := c.Prune(t.Context(), "system", nil); err == nil {
		t.Fatal("Prune while the registry fails: want an error")
	}
	failing = false
	if got, _ := c.Bundles(t.Context(), "system"); len(got) != 1 {
		t.Errorf("Bundles = %v, want the bundle kept for a retry", got)
	}
}

func TestBundlesOfUnknownKind(t *testing.T) {
	c, _ := newClient(t)
	if got, err := c.Bundles(t.Context(), "platform"); err != nil || len(got) != 0 {
		t.Errorf("Bundles = %v, %v; want none", got, err)
	}
}

func TestCopyImageAndIndex(t *testing.T) {
	src := httptest.NewServer(ggcr.New(ggcr.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(src.Close)
	host := strings.TrimPrefix(src.URL, "http://")
	img := newImage(t)
	if err := remote.Write(mustTag(t, host+"/up/app:v1"), img); err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: newImage(t), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	if err := remote.WriteIndex(mustTag(t, host+"/up/multi:v1"), idx); err != nil {
		t.Fatal(err)
	}

	c, _ := newClient(t)
	ctx := context.Background()
	if err := c.Copy(ctx, host+"/up/app:v1", "mirror/app:v1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Copy(ctx, host+"/up/multi:v1", "mirror/multi:v1"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, c, "mirror/app@"+digestOf(t, img)) || !exists(t, c, "mirror/multi@"+digestOf(t, idx)) {
		t.Error("the copied image and index are not in zot")
	}
	if err := c.Copy(ctx, host+"/up/missing:v1", "mirror/missing:v1"); err == nil {
		t.Error("Copy of a missing image: want an error")
	}
	if err := c.Copy(ctx, "bad ref", "mirror/x:v1"); err == nil {
		t.Error("Copy of an invalid source: want an error")
	}
}

func TestSplitRef(t *testing.T) {
	for _, tc := range []struct{ ref, repo, tag string }{
		{"ghcr.io/project-zot/zot-minimal:v2.1.0", "ghcr.io/project-zot/zot-minimal", "v2.1.0"},
		{"quay.io/cilium/cilium:v1.18.0@sha256:" + strings.Repeat("a", 64), "quay.io/cilium/cilium", "v1.18.0"},
		{"registry.k8s.io/pause@sha256:" + strings.Repeat("b", 64), "registry.k8s.io/pause", ""},
		{"localhost:5000/app:v1", "localhost:5000/app", "v1"},
		{"localhost:5000/app", "localhost:5000/app", ""},
		{"rabbitmq:4.3.1-management", "docker.io/library/rabbitmq", "4.3.1-management"},
		{"rancher/local-path-provisioner:v0.0.33", "docker.io/rancher/local-path-provisioner", "v0.0.33"},
		{"docker.io/traefik:v3.7.17", "docker.io/library/traefik", "v3.7.17"},
		{"index.docker.io/traefik:v3", "docker.io/library/traefik", "v3"},
		{"docker.io/library/busybox:1", "docker.io/library/busybox", "1"},
		{"index.docker.io/rancher/x:1", "docker.io/rancher/x", "1"},
	} {
		repo, tag, err := splitRef(tc.ref)
		if err != nil || repo != tc.repo || tag != tc.tag {
			t.Errorf("splitRef(%q) = %q, %q, %v; want %q, %q", tc.ref, repo, tag, err, tc.repo, tc.tag)
		}
	}
	for _, ref := range []string{"", "ghcr.io/", "@sha256:x"} {
		if _, _, err := splitRef(ref); err == nil {
			t.Errorf("splitRef(%q): want an error", ref)
		}
	}
}
