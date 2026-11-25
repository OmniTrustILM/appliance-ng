// Package bundletest signs and packs bundle layouts for tests, as the release workflow and an offline file do.
package bundletest

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// CosignPredicate is the predicate type of a cosign signature statement.
const CosignPredicate = "https://sigstore.dev/cosign/sign/v1"

const (
	mediaTypeSigstoreBundle = "application/vnd.dev.sigstore.bundle.v0.3+json"
	mediaTypeEmpty          = "application/vnd.oci.empty.v1+json"
	dssePayloadType         = "application/vnd.in-toto+json"
	statementType           = "https://in-toto.io/Statement/v1"
)

// NewKey returns a new P-256 signing key.
func NewKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Sign signs the bundle index of the layout in dir as cosign v3 does: a sigstore bundle v0.3 with a DSSE envelope
// around an in-toto statement, stored as a referrer manifest of the index and listed in index.json.
func Sign(t testing.TB, dir string, key *ecdsa.PrivateKey) {
	t.Helper()
	Attach(t, dir, key, IndexDescriptor(t, dir).Digest, CosignPredicate)
}

// Attach stores a sigstore bundle signed with key, whose statement names subject with predicateType, as a referrer
// of the bundle index of the layout in dir.
func Attach(t testing.TB, dir string, key *ecdsa.PrivateKey, subject v1.Hash, predicateType string) {
	t.Helper()
	p := layout.Path(dir)
	index := IndexDescriptor(t, dir)
	statement := mustJSON(t, map[string]any{
		"_type":         statementType,
		"subject":       []any{map[string]any{"digest": map[string]string{"sha256": subject.Hex}}},
		"predicateType": predicateType,
	})
	digest := sha256.Sum256(fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(dssePayloadType), dssePayloadType, len(statement), statement))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hint := sha256.Sum256(spki)
	sigstore := writeJSON(t, p, mediaTypeSigstoreBundle, map[string]any{
		"mediaType":            mediaTypeSigstoreBundle,
		"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": base64.StdEncoding.EncodeToString(hint[:])}},
		"dsseEnvelope": map[string]any{
			"payload":     base64.StdEncoding.EncodeToString(statement),
			"payloadType": dssePayloadType,
			"signatures":  []any{map[string]string{"sig": base64.StdEncoding.EncodeToString(sig)}},
		},
	})
	config := writeJSON(t, p, mediaTypeEmpty, map[string]any{})
	manifest := writeJSON(t, p, types.OCIManifestSchema1, v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  mediaTypeSigstoreBundle,
		Config:        config,
		Layers:        []v1.Descriptor{sigstore},
		Annotations:   map[string]string{"dev.sigstore.bundle.content": "dsse-envelope"},
		Subject:       &index,
	})
	manifest.ArtifactType = mediaTypeEmpty
	if err := p.AppendDescriptor(manifest); err != nil {
		t.Fatal(err)
	}
}

// IndexDescriptor returns the descriptor of the bundle index in the layout in dir, its only image index.
func IndexDescriptor(t testing.TB, dir string) v1.Descriptor {
	t.Helper()
	ii, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	im, err := ii.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range im.Manifests {
		if d.MediaType == types.OCIImageIndex {
			return d
		}
	}
	t.Fatal("no bundle index in the layout")
	return v1.Descriptor{}
}

// Tar writes the layout in dir as a tar in a new directory, its entries starting with "./" as
// `tar -C dir -cf … .` writes them, and returns its path.
func Tar(t testing.TB, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "bundle.tar")
	f, err := os.Create(out) //nolint:gosec // a test file in a temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tw := tar.NewWriter(f)
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		return addEntry(tw, path, "./"+filepath.ToSlash(rel), d)
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func addEntry(tw *tar.Writer, path, name string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	h, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	h.Name = name
	if err := tw.WriteHeader(h); err != nil || d.IsDir() {
		return err
	}
	src, err := os.Open(path) //nolint:gosec // a file of the test's layout
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	_, err = io.Copy(tw, src)
	return err
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeJSON(t testing.TB, p layout.Path, mt types.MediaType, v any) v1.Descriptor {
	t.Helper()
	data := mustJSON(t, v)
	h, size, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WriteBlob(h, io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	return v1.Descriptor{MediaType: mt, Size: size, Digest: h}
}
