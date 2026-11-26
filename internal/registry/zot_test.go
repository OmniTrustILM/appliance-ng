package registry

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestPruneAgainstZot pushes two bundles into a real zot and prunes the older one. It runs only when
// ZOT_TEST_ENDPOINT names a zot (host:port) with the configuration of addons/zot.yaml, ZOT_TEST_CA the PEM file of
// its TLS certificate and ZOT_TEST_PASSWORD the password of the push user. With ZOT_TEST_ROOT, zot's root directory
// on this host, and ZOT_TEST_GC_WAIT, long enough for zot's garbage collection to visit every repository, it also
// checks that garbage collection keeps an untagged manifest under an upstream name and the copies a bundle index lists,
// collects a copy no bundle index lists, and frees the blobs of a pruned image:
//
//	ZOT_TEST_ENDPOINT=127.0.0.1:5443 ZOT_TEST_CA=tls.crt ZOT_TEST_PASSWORD=... \
//	  ZOT_TEST_ROOT=/path/to/zot/data ZOT_TEST_GC_WAIT=60s go test ./internal/registry -run TestPruneAgainstZot -v
func TestPruneAgainstZot(t *testing.T) {
	c, root, gcWait := zotFromEnv(t)

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	ns := "ghcr.io/zot-test-" + hex.EncodeToString(suffix) + "/"
	shared, onlyOld, onlyNew, oldMoving, newMoving := newImage(t), newImage(t), newImage(t), newImage(t), newImage(t)
	old := newBundle(t, map[string]v1.Image{ns + "shared:v1": shared, ns + "old:v1": onlyOld, ns + "moving:tag": oldMoving})
	cur := newBundle(t, map[string]v1.Image{ns + "shared:v1": shared, ns + "new:v2": onlyNew, ns + "moving:tag": newMoving})
	// The older bundle goes first and creates the tag moving, which the newer one leaves on its image.
	for _, b := range []struct {
		version string
		bundle  *fakeBundle
	}{{"1.0.0", old}, {"2.0.0", cur}} {
		if err := c.PushBundle(t.Context(), b.bundle, b.bundle.metadata(b.version), nil); err != nil {
			t.Fatal(err)
		}
	}
	orphan := pushOrphan(t, c)

	if gcWait > 0 {
		time.Sleep(gcWait)
		if !exists(t, c, ns+"moving@"+digestOf(t, oldMoving)) || !onDisk(root, ns+"moving", layerOf(t, oldMoving)) {
			t.Fatal("zot collected the untagged manifest of the previous bundle")
		}
		checkBundleRetention(t, c, root, []v1.ImageIndex{old.index, cur.index}, orphan, gcWait)
	}

	if err := c.Prune(t.Context(), "system", []string{"2.0.0-amd64"}); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		ns + "shared@" + digestOf(t, shared):         true,
		ns + "shared:v1":                             true,
		ns + "new@" + digestOf(t, onlyNew):           true,
		ns + "moving:tag":                            true,
		ns + "moving@" + digestOf(t, newMoving):      true,
		ns + "old@" + digestOf(t, onlyOld):           false,
		ns + "old:v1":                                false,
		ns + "moving@" + digestOf(t, oldMoving):      true,
		"appliance/system:1.0.0-amd64":               false,
		"appliance/system@" + digestOf(t, old.index): false,
		"appliance/system:2.0.0-amd64":               true,
	}
	for _, d := range manifestsOf(t, old.index) {
		want["appliance/system@"+d.Digest.String()] = false
	}
	want["appliance/system@"+digestOf(t, shared)] = true
	wantRefs(t, c, want)
	if got, err := c.Bundles(t.Context(), "system"); err != nil || !slices.Equal(got, []string{"2.0.0-amd64"}) {
		t.Errorf("Bundles = %v, %v; want [2.0.0-amd64]", got, err)
	}

	if got := tagDigest(t, c, ns+"moving:tag"); got != digestOf(t, oldMoving) {
		t.Errorf("moving:tag = %s, want the older bundle's image %s", got, digestOf(t, oldMoving))
	}

	if gcWait > 0 {
		checkPrunedBlobs(t, root, 3*gcWait, ns+"old", onlyOld, map[string]v1.Image{ns + "shared": shared, ns + "moving": oldMoving})
	}
}

// checkPrunedBlobs checks that garbage collection frees the blobs of the pruned image of repo within wait, and keeps
// those of the kept images, by repository.
func checkPrunedBlobs(t *testing.T, root string, wait time.Duration, repo string, pruned v1.Image, kept map[string]v1.Image) {
	t.Helper()
	if !gone(root, repo, layerOf(t, pruned), wait) {
		t.Error("zot did not free the blobs of the pruned image")
	}
	for keptRepo, img := range kept {
		if !onDisk(root, keptRepo, layerOf(t, img)) {
			t.Errorf("zot freed a blob of the kept image in %s", keptRepo)
		}
	}
}

// pushOrphan pushes an image by its digest alone into appliance/system, as a copy whose bundle index is gone.
func pushOrphan(t *testing.T, c *Client) v1.Image {
	t.Helper()
	img := newImage(t)
	repo, err := c.repository(bundleRepository("system"))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Digest(digestOf(t, img)), img, c.options(t.Context())...); err != nil {
		t.Fatal(err)
	}
	return img
}

// checkBundleRetention checks that garbage collection kept every copy the tagged bundle indexes list in
// appliance/system and collects the orphan copy, with its blob, within wait.
func checkBundleRetention(t *testing.T, c *Client, root string, indexes []v1.ImageIndex, orphan v1.Image, wait time.Duration) {
	t.Helper()
	for _, index := range indexes {
		for _, d := range manifestsOf(t, index) {
			if !exists(t, c, "appliance/system@"+d.Digest.String()) {
				t.Errorf("zot collected appliance/system@%s, listed by a tagged bundle index", d.Digest)
			}
		}
	}
	if !gone(root, "appliance/system", layerOf(t, orphan), wait) || exists(t, c, "appliance/system@"+digestOf(t, orphan)) {
		t.Error("zot kept a copy in appliance/system that no bundle index lists")
	}
}

// zotFromEnv returns a client of the zot the environment names, or skips the test, and the root directory and the
// wait for the garbage collection checks, zero without them.
func zotFromEnv(t *testing.T) (*Client, string, time.Duration) {
	t.Helper()
	endpoint := os.Getenv("ZOT_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("ZOT_TEST_ENDPOINT is not set")
	}
	pem, err := os.ReadFile(os.Getenv("ZOT_TEST_CA"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("ZOT_TEST_CA holds no certificate")
	}
	var gcWait time.Duration
	root := os.Getenv("ZOT_TEST_ROOT")
	if s := os.Getenv("ZOT_TEST_GC_WAIT"); s != "" && root != "" {
		if gcWait, err = time.ParseDuration(s); err != nil {
			t.Fatal(err)
		}
	}
	return New(endpoint, pool, PushUser, os.Getenv("ZOT_TEST_PASSWORD")), root, gcWait
}

// wantRefs checks which of the references in want zot holds.
func wantRefs(t *testing.T, c *Client, want map[string]bool) {
	t.Helper()
	for ref, w := range want {
		if got := exists(t, c, ref); got != w {
			t.Errorf("%s exists = %v, want %v", ref, got, w)
		}
	}
}

func manifestsOf(t *testing.T, index v1.ImageIndex) []v1.Descriptor {
	t.Helper()
	im, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	return im.Manifests
}

// gone waits up to wait for the blob digest of repo to leave zot's root directory root, and reports whether it did.
func gone(root, repo, digest string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for onDisk(root, repo, digest) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Second)
	}
	return true
}

func layerOf(t *testing.T, img v1.Image) string {
	t.Helper()
	layers, err := img.Layers()
	if err != nil || len(layers) == 0 {
		t.Fatalf("layers: %v", err)
	}
	return digestOf(t, layers[0])
}

// onDisk reports whether zot, with its root directory root, holds the blob digest of repo.
func onDisk(root, repo, digest string) bool {
	algorithm, encoded, _ := strings.Cut(digest, ":")
	_, err := os.Stat(filepath.Join(root, repo, "blobs", algorithm, encoded))
	return err == nil
}
