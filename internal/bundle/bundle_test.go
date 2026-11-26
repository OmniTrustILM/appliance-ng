package bundle

import (
	"bytes"
	"cmp"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func TestBuildVerifyRoundTrip(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	for _, kind := range []string{release.BundleSystem, release.BundlePlatform} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(kind+"-"+arch, func(t *testing.T) {
				dir, built := build(t, r, kind, arch, key)
				archive := tarLayout(t, dir)
				for _, path := range []string{dir, archive} {
					b, err := Open(path)
					if err != nil {
						t.Fatal(err)
					}
					got, err := b.Verify(&key.PublicKey, arch)
					if err != nil {
						t.Fatalf("%s: %v", path, err)
					}
					if !reflect.DeepEqual(got, built) {
						t.Errorf("%s: verified metadata = %+v, want %+v", path, got, built)
					}
					if err := b.Close(); err != nil {
						t.Fatal(err)
					}
				}
				entries, err := os.ReadDir(filepath.Dir(archive))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Errorf("Close left %d entries next to the tar, want only the tar", len(entries))
				}
			})
		}
	}
}

func TestMetadataListsEveryImage(t *testing.T) {
	r := newTestRelease(t)
	for _, kind := range []string{release.BundleSystem, release.BundlePlatform} {
		for _, arch := range []string{"amd64", "arm64"} {
			t.Run(kind+"-"+arch, func(t *testing.T) {
				_, md := build(t, r, kind, arch, nil)
				want := r.m.BundleImages(kind, arch)
				if len(md.Images) != len(want) {
					t.Fatalf("metadata lists %d images, want %d", len(md.Images), len(want))
				}
				for i, img := range md.Images {
					if img.Ref != want[i].Ref || img.Digest != r.digests[want[i].Ref][arch] || img.Size <= 0 {
						t.Errorf("image %d = %+v, want %s at %s", i, img, want[i].Ref, r.digests[want[i].Ref][arch])
					}
				}
				if md.Format != 1 || md.Kind != kind || md.Version != "1.2.3" || md.Arch != arch || md.Created.IsZero() {
					t.Errorf("metadata = %+v", md)
				}
			})
		}
	}
}

func TestMetadataByKind(t *testing.T) {
	r := newTestRelease(t)
	_, system := build(t, r, release.BundleSystem, "amd64", nil)
	if system.Talos != "v1.14.5" || system.Kubernetes != "v1.37.1" || system.OperatorRange != ">=1.1.0 <2.0.0" ||
		system.Operator != "" || system.ILM != "" || system.MinAppliance != "" ||
		!slices.Equal(system.Files, []string{"release.yaml", "manager.yaml"}) {
		t.Errorf("system metadata = %+v", system)
	}
	_, platform := build(t, r, release.BundlePlatform, "amd64", nil)
	if platform.Operator != "v1.1.0" || platform.ILM != "2.20.0" || platform.MinAppliance != "1.0.0" ||
		platform.Talos != "" || platform.Kubernetes != "" || platform.OperatorRange != "" ||
		!slices.Equal(platform.Files, []string{"ilm-operator.yaml"}) {
		t.Errorf("platform metadata = %+v", platform)
	}
}

func TestFilesAndNotes(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	dir, md := build(t, r, release.BundleSystem, "amd64", key)
	b, err := Open(tarLayout(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.File("release.yaml"); err == nil {
		t.Error("File before Verify: want an error")
	}
	if _, err := b.Notes(); err == nil {
		t.Error("Notes before Verify: want an error")
	}
	if _, err := b.Images(); err == nil {
		t.Error("Images before Verify: want an error")
	}
	if _, err := b.Verify(&key.PublicKey, "amd64"); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{"release.yaml": "release.yaml", "manager.yaml": "addons/manager.yaml"} {
		got, err := b.File(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, r.files[path].Data) {
			t.Errorf("File(%s) = %q, want %q", name, got, r.files[path].Data)
		}
	}
	if _, err := b.File("ilm-operator.yaml"); err == nil {
		t.Error("File(ilm-operator.yaml) of a system bundle: want an error")
	}
	notes, err := b.Notes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(notes, testNotes) {
		t.Errorf("Notes = %q, want %q", notes, testNotes)
	}

	images, err := b.Images()
	if err != nil {
		t.Fatal(err)
	}
	var seen []ImageRef
	for _, im := range images {
		seen = append(seen, im.Ref)
		if got := mustDigest(t, im.Image); got != im.Ref.Digest {
			t.Errorf("image %s digest %s, want %s", im.Ref.Ref, got, im.Ref.Digest)
		}
		if _, err := im.Image.Layers(); err != nil {
			t.Error(err)
		}
	}
	if !reflect.DeepEqual(seen, md.Images) {
		t.Errorf("Images yielded %+v, want %+v", seen, md.Images)
	}
}

func TestVerifyRejects(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	cases := []struct {
		name   string
		signer *ecdsa.PrivateKey
		arch   string
		change func(t *testing.T, dir string)
		want   error
	}{
		{name: "unsigned", arch: "amd64", want: ErrUnsigned},
		{name: "other key", signer: newKey(t), arch: "amd64", want: ErrBadSignature},
		{name: "other arch", signer: key, arch: "arm64", want: ErrWrongArch},
		{name: "one blob removed", signer: key, arch: "amd64", want: ErrMissingImage, change: func(t *testing.T, dir string) {
			if err := os.Remove(blobPath(dir, childManifest(t, dir, 1).Layers[0].Digest)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "one byte flipped in an image", signer: key, arch: "amd64", want: ErrMissingImage, change: func(t *testing.T, dir string) {
			flipByte(t, blobPath(dir, childManifest(t, dir, 2).Layers[1].Digest))
		}},
		{name: "one byte flipped in the metadata", signer: key, arch: "amd64", want: ErrBadSignature, change: func(t *testing.T, dir string) {
			flipByte(t, blobPath(dir, childManifest(t, dir, 0).Config.Digest))
		}},
		{name: "one byte flipped in the notes", signer: key, arch: "amd64", want: ErrBadSignature, change: func(t *testing.T, dir string) {
			flipByte(t, blobPath(dir, childManifest(t, dir, 0).Layers[0].Digest))
		}},
		{name: "signed index replaced", signer: key, arch: "amd64", want: ErrBadSignature, change: func(t *testing.T, dir string) {
			flipByte(t, blobPath(dir, bundleIndexDescriptor(t, dir).Digest))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := build(t, r, release.BundleSystem, "amd64", tc.signer)
			if tc.change != nil {
				tc.change(t, dir)
			}
			b, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Verify(&key.PublicKey, tc.arch); !errors.Is(err, tc.want) {
				t.Errorf("Verify = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("truncated tar", func(t *testing.T) {
		dir, _ := build(t, r, release.BundleSystem, "amd64", key)
		archive := tarLayout(t, dir)
		entries := tarEntries(t, archive)
		largest := slices.MaxFunc(entries, func(a, b tarEntry) int { return cmp.Compare(a.size, b.size) })
		info, err := os.Stat(archive)
		if err != nil {
			t.Fatal(err)
		}
		cuts := map[string]int64{
			"inside an entry":        largest.data + largest.size/2,
			"after the first entry":  entries[0].end,
			"without the end marker": entries[len(entries)-1].end,
			"inside the end marker":  info.Size() - 512,
		}
		for name, size := range cuts {
			t.Run(name, func(t *testing.T) {
				cut := filepath.Join(t.TempDir(), "bundle.tar")
				data, err := os.ReadFile(archive)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cut, data[:size], 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Open(cut); !errors.Is(err, ErrTruncated) {
					t.Errorf("Open = %v, want %v", err, ErrTruncated)
				}
				entries, err := os.ReadDir(filepath.Dir(cut))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 {
					t.Errorf("a refused tar left %d entries next to it, want only the tar", len(entries))
				}
			})
		}
	})

	t.Run("format 2", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "layout")
		md := Metadata{Format: 2, Kind: release.BundleSystem, Version: "1.2.3", Arch: "amd64"}
		if err := writeLayout(dir, md, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		sign(t, dir, key)
		b, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Verify(&key.PublicKey, "amd64"); !errors.Is(err, ErrFormatTooNew) {
			t.Errorf("Verify = %v, want %v", err, ErrFormatTooNew)
		}
	})
}

func TestBuildRejects(t *testing.T) {
	r := newTestRelease(t)
	cases := []struct {
		name       string
		kind, arch string
		change     func(m *release.Manifest)
	}{
		{name: "unknown kind", kind: "other", arch: "amd64"},
		{name: "unknown arch", kind: release.BundleSystem, arch: "riscv64"},
		{name: "digest differs from release.yaml", kind: release.BundleSystem, arch: "amd64", change: func(m *release.Manifest) {
			m.Images[0].Digest = m.Images[2].Digest
		}},
		{name: "per-arch digest differs from release.yaml", kind: release.BundlePlatform, arch: "arm64", change: func(m *release.Manifest) {
			m.Images[2].ArchDigests = map[string]string{"amd64": m.Images[2].ArchDigests["amd64"], "arm64": m.Images[2].ArchDigests["amd64"]}
		}},
		{name: "image lacks the arch", kind: release.BundleSystem, arch: "arm64", change: func(m *release.Manifest) {
			m.Images[1].Arches = nil
		}},
		{name: "add-on file differs from release.yaml", kind: release.BundlePlatform, arch: "amd64", change: func(m *release.Manifest) {
			m.Addons[1].SHA256 = m.Addons[0].SHA256
		}},
		{name: "add-on file not in release.yaml", kind: release.BundleSystem, arch: "amd64", change: func(m *release.Manifest) {
			m.Addons = m.Addons[1:]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := *r.m
			m.Images = slices.Clone(r.m.Images)
			m.Addons = slices.Clone(r.m.Addons)
			if tc.change != nil {
				tc.change(&m)
			}
			if _, err := Build(t.Context(), &m, tc.kind, tc.arch, Inputs{Files: r.files, Notes: testNotes}, filepath.Join(t.TempDir(), "layout")); err == nil {
				t.Error("Build: want an error")
			}
		})
	}
}

func TestBuildAddsBuiltImages(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	installer := r.pushImage(t, r.host+"/installer:1.2.3-amd64", "amd64")
	dir := filepath.Join(t.TempDir(), "layout")
	md, err := Build(t.Context(), r.m, release.BundleSystem, "amd64", Inputs{Files: r.files, Notes: testNotes, Built: []string{installer.Ref + "@" + installer.Digest}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	sign(t, dir, key)
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	got, err := b.Verify(&key.PublicKey, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, md) {
		t.Errorf("verified metadata = %+v, want %+v", got, md)
	}
	last := got.Images[len(got.Images)-1]
	if len(got.Images) != len(r.m.BundleImages(release.BundleSystem, "amd64"))+1 || last.Ref != installer.Ref || last.Digest != installer.Digest {
		t.Errorf("images = %+v, want the release images and then %s at %s", got.Images, installer.Ref, installer.Digest)
	}
}

func TestBuildRejectsBuiltImages(t *testing.T) {
	r := newTestRelease(t)
	installer := r.pushImage(t, r.host+"/installer:1.2.3-amd64", "amd64")
	cases := []struct {
		name, kind, ref string
	}{
		{name: "no digest", kind: release.BundleSystem, ref: installer.Ref},
		{name: "not a sha256 digest", kind: release.BundleSystem, ref: installer.Ref + "@md5:00"},
		{name: "digest of another image", kind: release.BundleSystem, ref: installer.Ref + "@" + r.m.Images[2].Digest},
		{name: "platform bundle", kind: release.BundlePlatform, ref: installer.Ref + "@" + installer.Digest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(t.Context(), r.m, tc.kind, "amd64", Inputs{Files: r.files, Notes: testNotes, Built: []string{tc.ref}}, filepath.Join(t.TempDir(), "layout")); err == nil {
				t.Error("Build: want an error")
			}
		})
	}
}

func TestVerifyRefusesOtherStatements(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)

	t.Run("signature of another digest", func(t *testing.T) {
		dir, _ := build(t, r, release.BundleSystem, "amd64", nil)
		attach(t, dir, key, bundleIndex(t, dir).Manifests[0].Digest, cosignPredicate)
		if _, err := verifyLayout(t, dir, key); !errors.Is(err, ErrBadSignature) {
			t.Errorf("Verify = %v, want %v", err, ErrBadSignature)
		}
	})
	t.Run("SBOM attestation only", func(t *testing.T) {
		dir, _ := build(t, r, release.BundleSystem, "amd64", nil)
		attach(t, dir, key, bundleIndexDescriptor(t, dir).Digest, "https://spdx.dev/Document")
		if _, err := verifyLayout(t, dir, key); !errors.Is(err, ErrUnsigned) {
			t.Errorf("Verify = %v, want %v", err, ErrUnsigned)
		}
	})
	t.Run("signature next to an SBOM attestation", func(t *testing.T) {
		dir, _ := build(t, r, release.BundleSystem, "amd64", nil)
		attach(t, dir, key, bundleIndexDescriptor(t, dir).Digest, "https://spdx.dev/Document")
		sign(t, dir, key)
		if _, err := verifyLayout(t, dir, key); err != nil {
			t.Errorf("Verify = %v, want nil", err)
		}
	})
}

func verifyLayout(t *testing.T, dir string, key *ecdsa.PrivateKey) (Metadata, error) {
	t.Helper()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b.Verify(&key.PublicKey, "amd64")
}

func TestEmbeddedKeyParses(t *testing.T) {
	pub, ok := PublicKey().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("PublicKey() = %T, want an ECDSA P-256 key", PublicKey())
	}
}

func TestParsePublicKeyRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"not PEM":   []byte("key"),
		"not a key": []byte("-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"),
	} {
		if _, err := ParsePublicKey(data); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func flipByte(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusesOtherInput(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing.tar")); err == nil {
		t.Error("Open of a missing file: want an error")
	}

	key := newKey(t)
	dir := filepath.Join(t.TempDir(), "layout")
	if err := writeLayout(dir, Metadata{Format: 1, Kind: "other", Version: "1.2.3", Arch: "amd64"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	sign(t, dir, key)
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(&key.PublicKey, "amd64"); err == nil {
		t.Error("Verify of metadata with an unknown kind: want an error")
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(edKey.Public(), "amd64"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("Verify with an Ed25519 key = %v, want %v", err, ErrBadSignature)
	}
	if err := os.Remove(filepath.Join(dir, "index.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(&key.PublicKey, "amd64"); err == nil {
		t.Error("Verify of a layout without index.json: want an error")
	}
}

func TestImagesReportsUnreadableImages(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	dir, md := build(t, r, release.BundleSystem, "amd64", key)
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(&key.PublicKey, "amd64"); err != nil {
		t.Fatal(err)
	}
	h, err := v1.NewHash(md.Images[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "blobs", "sha256", h.Hex)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Images(); err == nil || !strings.Contains(err.Error(), md.Images[0].Ref) {
		t.Errorf("Images with a missing manifest = %v, want an error naming %s", err, md.Images[0].Ref)
	}
	if err := os.Remove(filepath.Join(dir, "index.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Images(); err == nil {
		t.Error("Images without index.json: want an error")
	}
}

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func TestBuildVerifyKeepsAliasedImages(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	first := r.pushImage(t, r.host+"/busybox:1.37", "amd64")
	img, err := remote.Image(mustParse(t, first.Ref))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{r.host + "/busybox:latest", r.host + "/mirror/busybox:1.37"} {
		if err := remote.Write(mustParse(t, ref), img); err != nil {
			t.Fatal(err)
		}
		r.m.Images = append(r.m.Images, release.Image{Ref: ref, Digest: first.Digest, Bundle: release.BundleSystem, Arches: []string{"amd64"}})
	}
	r.m.Images = append(r.m.Images, first)
	dir, built := build(t, r, release.BundleSystem, "amd64", key)

	got, err := verifyLayout(t, dir, key)

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, built) {
		t.Errorf("verified metadata = %+v, want %+v", got, built)
	}
}

func mustParse(t *testing.T, ref string) name.Reference {
	t.Helper()
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// TestVerifySignedLeavesOutTheImages checks a bundle whose images are elsewhere: the signature, metadata and arch are
// checked, its notes and files are available, and its images are not.
func TestVerifySignedLeavesOutTheImages(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	dir, want := build(t, r, release.BundlePlatform, "amd64", key)
	for _, m := range bundleIndex(t, dir).Manifests[1:] {
		if err := os.Remove(blobPath(dir, m.Digest)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Verify(&key.PublicKey, "amd64"); !errors.Is(err, ErrMissingImage) {
		t.Fatalf("Verify without images = %v, want %v", err, ErrMissingImage)
	}
	md, err := b.VerifySigned(&key.PublicKey, "amd64")
	if err != nil || md.Version != want.Version || md.Kind != want.Kind {
		t.Fatalf("VerifySigned = %+v, %v", md, err)
	}
	if notes, err := b.Notes(); err != nil || string(notes) != string(testNotes) {
		t.Errorf("Notes = %q, %v", notes, err)
	}
	if _, err := b.File("ilm-operator.yaml"); err != nil {
		t.Errorf("File: %v", err)
	}
	if _, err := b.Images(); err == nil {
		t.Error("Images of a bundle verified without its images succeeded")
	}
	if _, err := b.VerifySigned(&newKey(t).PublicKey, "amd64"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("VerifySigned with another key = %v, want %v", err, ErrBadSignature)
	}
	if _, err := b.VerifySigned(&key.PublicKey, "arm64"); !errors.Is(err, ErrWrongArch) {
		t.Errorf("VerifySigned for arm64 = %v, want %v", err, ErrWrongArch)
	}
}

// TestVerifySignedRefusesImagesTheIndexDoesNotName checks that the images the metadata lists are in the signed bundle
// index under their reference and digest, without reading their blobs.
func TestVerifySignedRefusesImagesTheIndexDoesNotName(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	cases := map[string]func(d *v1.Descriptor){
		"other reference": func(d *v1.Descriptor) { d.Annotations[annotationRefName] += "-other" },
		"other digest":    func(d *v1.Descriptor) { d.Digest.Hex = strings.Repeat("0", 64) },
		"not an image":    func(d *v1.Descriptor) { d.MediaType = types.OCIImageIndex },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := build(t, r, release.BundlePlatform, "amd64", nil)
			resign(t, dir, key, func(im *v1.IndexManifest) { change(&im.Manifests[1]) })
			b, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.VerifySigned(&key.PublicKey, "amd64"); !errors.Is(err, ErrMissingImage) {
				t.Errorf("VerifySigned = %v, want %v", err, ErrMissingImage)
			}
		})
	}
}

// resign replaces the bundle index of the layout in dir with a changed copy and signs that with key.
func resign(t *testing.T, dir string, key *ecdsa.PrivateKey, change func(im *v1.IndexManifest)) {
	t.Helper()
	im := bundleIndex(t, dir)
	change(im)
	data, err := json.Marshal(im)
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	p := layout.Path(dir)
	if err := p.WriteBlob(digest, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveDescriptors(match.MediaTypes(string(types.OCIImageIndex))); err != nil {
		t.Fatal(err)
	}
	if err := p.AppendDescriptor(v1.Descriptor{MediaType: types.OCIImageIndex, Digest: digest, Size: size}); err != nil {
		t.Fatal(err)
	}
	sign(t, dir, key)
}

func TestValidFileName(t *testing.T) {
	for name, want := range map[string]bool{
		"release.yaml": true,
		"":             false,
		"/":            false,
		"a/b.yaml":     false,
		"/a.yaml":      false,
		`a\b.yaml`:     false,
		".hidden":      false,
		"..":           false,
		NotesFile:      false,
		MetadataFile:   false,
	} {
		if got := validFileName(name); got != want {
			t.Errorf("validFileName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestVerifyRefusesEntriesTheMetadataDoesNotList refuses a bundle index with an image the metadata does not list, or
// whose metadata is not an image manifest.
func TestVerifyRefusesEntriesTheMetadataDoesNotList(t *testing.T) {
	r := newTestRelease(t)
	key := newKey(t)
	cases := map[string]func(im *v1.IndexManifest){
		"surplus image": func(im *v1.IndexManifest) {
			extra := im.Manifests[1]
			extra.Annotations = map[string]string{annotationRefName: "ghcr.io/extra:v1"}
			im.Manifests = append(im.Manifests, extra)
		},
		"metadata not a manifest": func(im *v1.IndexManifest) { im.Manifests[0].MediaType = types.OCIImageIndex },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := build(t, r, release.BundleSystem, "amd64", nil)
			resign(t, dir, key, change)
			b, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.Verify(&key.PublicKey, "amd64"); err == nil {
				t.Error("Verify succeeded")
			}
			if _, err := b.VerifySigned(&key.PublicKey, "amd64"); err == nil {
				t.Error("VerifySigned succeeded")
			}
		})
	}
}
