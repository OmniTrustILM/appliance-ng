package bundle

import (
	"archive/tar"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle/bundletest"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

var testNotes = []byte("# Release 1.2.3\n")

// testRelease is a release manifest whose images live in an in-process registry.
type testRelease struct {
	m     *release.Manifest
	host  string
	files fstest.MapFS
	// digests maps an image reference and an arch to the digest of the image manifest for that arch.
	digests map[string]map[string]string
}

func newTestRelease(t *testing.T) *testRelease {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	r := &testRelease{
		host: host,
		files: fstest.MapFS{
			"release.yaml":             {Data: []byte("version: 1.2.3\n")},
			"addons/manager.yaml":      {Data: []byte("kind: Deployment\n")},
			"addons/ilm-operator.yaml": {Data: []byte("kind: Deployment\nmetadata: {name: ilm-operator}\n")},
		},
		digests: map[string]map[string]string{},
	}
	r.m = &release.Manifest{
		Version:       "1.2.3",
		Compatibility: release.Compatibility{Operator: ">=1.1.0 <2.0.0", MinAppliance: "1.0.0"},
		Talos:         release.TalosPin{Version: "v1.14.5", Kubernetes: "v1.37.1"},
		Platform:      release.PlatformDefaults{ILMVersion: "2.20.0"},
		Addons: []release.AddonFile{
			{Name: "appliance-manager", File: "manager.yaml", Version: "local", SHA256: fileSHA(t, r.files, "addons/manager.yaml")},
			{Name: "ilm-operator", File: "ilm-operator.yaml", Version: "v1.1.0", SHA256: fileSHA(t, r.files, "addons/ilm-operator.yaml")},
		},
		Images: []release.Image{
			r.pushIndex(t, host+"/kube-apiserver:v1.37.1", release.BundleSystem, false),
			r.pushImage(t, host+"/installer-base:v1.14.5", "amd64"),
			r.pushIndex(t, host+"/ilm/core:2.20.0", release.BundlePlatform, true),
			r.pushIndex(t, host+"/ilm/auth:1.8.0", release.BundlePlatform, false),
		},
	}
	return r
}

// pushIndex pushes a two-arch image index; withDigest writes the digest into the reference as well.
func (r *testRelease) pushIndex(t *testing.T, ref, bundle string, withDigest bool) release.Image {
	t.Helper()
	r.digests[ref] = map[string]string{}
	idx := v1.ImageIndex(empty.Index)
	for _, arch := range []string{"amd64", "arm64"} {
		img := randomImage(t, arch, types.DockerManifestSchema2)
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
		r.digests[ref][arch] = mustDigest(t, img)
	}
	tag, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}
	digest := mustDigest(t, idx)
	img := release.Image{Digest: digest, Bundle: bundle}
	if withDigest {
		r.digests[ref+"@"+digest] = r.digests[ref]
		img.ArchDigests = r.digests[ref]
		ref += "@" + digest
	}
	img.Ref = ref
	return img
}

// pushImage pushes a single-arch OCI image of the system bundle limited to arch.
func (r *testRelease) pushImage(t *testing.T, ref, arch string) release.Image {
	t.Helper()
	img := randomImage(t, arch, types.OCIManifestSchema1)
	tag, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	digest := mustDigest(t, img)
	r.digests[ref] = map[string]string{arch: digest}
	return release.Image{Ref: ref, Digest: digest, Bundle: release.BundleSystem, Arches: []string{arch}}
}

func randomImage(t *testing.T, arch string, mt types.MediaType) v1.Image {
	t.Helper()
	img, err := random.Image(512, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf.OS, cf.Architecture = "linux", arch
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	if mt == types.OCIManifestSchema1 {
		img = mutate.ConfigMediaType(mutate.MediaType(img, mt), types.OCIConfigJSON)
	}
	return img
}

func mustDigest(t *testing.T, d interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	h, err := d.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

func fileSHA(t *testing.T, fsys fs.FS, path string) string {
	t.Helper()
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	return bundletest.NewKey(t)
}

// build builds a bundle into a new directory and signs it with key unless key is nil.
func build(t *testing.T, r *testRelease, kind, arch string, key *ecdsa.PrivateKey) (string, Metadata) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "layout")
	md, err := Build(t.Context(), r.m, kind, arch, Inputs{Files: r.files, Notes: testNotes}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if key != nil {
		sign(t, dir, key)
	}
	return dir, md
}

func sign(t *testing.T, dir string, key *ecdsa.PrivateKey) {
	t.Helper()
	bundletest.Sign(t, dir, key)
}

func attach(t *testing.T, dir string, key *ecdsa.PrivateKey, subject v1.Hash, predicateType string) {
	t.Helper()
	bundletest.Attach(t, dir, key, subject, predicateType)
}

func bundleIndexDescriptor(t *testing.T, dir string) v1.Descriptor {
	t.Helper()
	return bundletest.IndexDescriptor(t, dir)
}

func bundleIndex(t *testing.T, dir string) *v1.IndexManifest {
	t.Helper()
	data, err := layout.Path(dir).Bytes(bundleIndexDescriptor(t, dir).Digest)
	if err != nil {
		t.Fatal(err)
	}
	im, err := v1.ParseIndexManifest(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return im
}

// childManifest returns the manifest of the bundle index's child i.
func childManifest(t *testing.T, dir string, i int) *v1.Manifest {
	t.Helper()
	data, err := layout.Path(dir).Bytes(bundleIndex(t, dir).Manifests[i].Digest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := v1.ParseManifest(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func blobPath(dir string, h v1.Hash) string {
	return filepath.Join(dir, "blobs", h.Algorithm, h.Hex)
}

func tarLayout(t *testing.T, dir string) string {
	t.Helper()
	return bundletest.Tar(t, dir)
}

// tarEntry is where a regular file of a tar starts its data, its size and where its padded data ends.
type tarEntry struct{ data, size, end int64 }

// tarEntries returns the regular files of the tar at path in order.
func tarEntries(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	var entries []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, tarEntry{data: data, size: h.Size, end: data + (h.Size+511)/512*512})
	}
}
