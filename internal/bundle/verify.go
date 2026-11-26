package bundle

import (
	"archive/tar"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	appliance "github.com/OmniTrustILM/appliance-ng"
)

// testKey is the base64 of a PEM public key that replaces cosign.pub in a test build, set with
// -ldflags "-X github.com/OmniTrustILM/appliance-ng/internal/bundle.testKey=<base64>". A release build leaves it empty.
var testKey string

const pemPublicKey = "-----BEGIN PUBLIC KEY-----"

var embeddedKey = sync.OnceValue(func() crypto.PublicKey {
	key, err := signingKey(testKey)
	if err != nil {
		panic(err)
	}
	return key
})

// PublicKey returns the bundle signing key: the test key of a test build, else the key embedded from cosign.pub.
func PublicKey() crypto.PublicKey {
	return embeddedKey()
}

// signingKey parses test, the base64 of a PEM public key, or cosign.pub when test is empty.
func signingKey(test string) (crypto.PublicKey, error) {
	if test == "" {
		key, err := ParsePublicKey(appliance.CosignPublicKey)
		if err != nil {
			return nil, fmt.Errorf("embedded cosign.pub: %w", err)
		}
		return key, nil
	}
	data, err := base64.StdEncoding.DecodeString(test)
	if err != nil {
		return nil, fmt.Errorf("bundle test key: %w", err)
	}
	// The release check finds a test key by the base64 of its first line, so nothing may come before or after it.
	if _, rest := pem.Decode(data); !bytes.HasPrefix(data, []byte(pemPublicKey)) || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("bundle test key: not exactly one PEM public key")
	}
	key, err := ParsePublicKey(data)
	if err != nil {
		return nil, fmt.Errorf("bundle test key: %w", err)
	}
	return key, nil
}

// ParsePublicKey parses a PEM ECDSA public key as cosign writes it.
func ParsePublicKey(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	if _, ok := key.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("the public key is %T, not ECDSA", key)
	}
	return key, nil
}

// Verify checks, in order, the bundle's cosign signature against pub, its metadata, that it is for arch, and that
// every image it lists is present and intact. It returns the metadata and makes the bundle's content available.
func (b *Bundle) Verify(pub crypto.PublicKey, arch string) (Metadata, error) {
	b.verified = nil
	v, index, err := b.verifySigned(pub, arch)
	if err != nil {
		return Metadata{}, err
	}
	if err := b.checkImages(index, v.metadata.Images); err != nil {
		return Metadata{}, err
	}
	v.images = true
	b.verified = v
	return v.metadata, nil
}

// VerifySigned checks the signature, the metadata and the arch as Verify does, and that the bundle index lists every
// image of the metadata, but not the images' blobs, for a bundle whose images stay in its registry. Its files and notes
// are available afterwards, its images are not.
func (b *Bundle) VerifySigned(pub crypto.PublicKey, arch string) (Metadata, error) {
	b.verified = nil
	v, index, err := b.verifySigned(pub, arch)
	if err != nil {
		return Metadata{}, err
	}
	if _, err := listedImages(index, v.metadata.Images); err != nil {
		return Metadata{}, err
	}
	b.verified = v
	return v.metadata, nil
}

func (b *Bundle) verifySigned(pub crypto.PublicKey, arch string) (*verified, *v1.IndexManifest, error) {
	key, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("%w: unsupported public key %T", ErrBadSignature, pub)
	}
	top, err := b.layoutIndex()
	if err != nil {
		return nil, nil, err
	}
	indexDesc, index, err := b.bundleIndex(top)
	if err != nil {
		return nil, nil, err
	}
	if err := b.checkSignature(key, top, indexDesc.Digest); err != nil {
		return nil, nil, err
	}
	v, err := b.readMetadata(index)
	if err != nil {
		return nil, nil, err
	}
	if v.metadata.Arch != arch {
		return nil, nil, fmt.Errorf("%w: %s, not %s", ErrWrongArch, v.metadata.Arch, arch)
	}
	if err := b.checkContent(v); err != nil {
		return nil, nil, err
	}
	v.index = indexDesc.Digest
	return v, index, nil
}

// checkContent checks that the notes and every file the metadata names can be read, and that each name can stand
// beside the notes and the metadata in a stored bundle's directory.
func (b *Bundle) checkContent(v *verified) error {
	if _, err := b.readBlob(v.notes, maxFilesSize); err != nil {
		return fmt.Errorf("read the bundle notes: %w", err)
	}
	archive, err := b.readBlob(v.files, maxFilesSize)
	if err != nil {
		return fmt.Errorf("read the bundle manifests: %w", err)
	}
	names, err := tarNames(archive)
	if err != nil {
		return fmt.Errorf("read the bundle manifests: %w", err)
	}
	for _, name := range v.metadata.Files {
		if !validFileName(name) {
			return fmt.Errorf("the bundle names a file %q", name)
		}
		if !names[name] {
			return fmt.Errorf("the bundle lacks its file %s", name)
		}
	}
	return nil
}

// validFileName reports whether name is a plain, visible file name, without a separator, other than the notes and the
// metadata.
func validFileName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".") &&
		name != NotesFile && name != MetadataFile
}

// tarNames reads every entry of a tar and returns their names.
func tarNames(archive []byte) (map[string]bool, error) {
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return nil, err
		}
		names[h.Name] = true
	}
}

func (b *Bundle) layoutIndex() (*v1.IndexManifest, error) {
	f, err := os.Open(filepath.Join(b.dir, "index.json"))
	if err != nil {
		return nil, fmt.Errorf("read the bundle layout: %w", err)
	}
	defer func() { _ = f.Close() }()
	top, err := v1.ParseIndexManifest(io.LimitReader(f, maxManifestSize))
	if err != nil {
		return nil, fmt.Errorf("read the bundle layout: %w", err)
	}
	return top, nil
}

// bundleIndex returns the only image index of the layout whose artifact type is a bundle.
func (b *Bundle) bundleIndex(top *v1.IndexManifest) (v1.Descriptor, *v1.IndexManifest, error) {
	var found []v1.Descriptor
	var index *v1.IndexManifest
	for _, d := range top.Manifests {
		if d.MediaType != types.OCIImageIndex {
			continue
		}
		data, err := b.readBlob(d, maxManifestSize)
		if err != nil {
			return v1.Descriptor{}, nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
		}
		im, err := v1.ParseIndexManifest(bytes.NewReader(data))
		if err != nil {
			return v1.Descriptor{}, nil, fmt.Errorf("%w: index %s: %w", ErrBadSignature, d.Digest, err)
		}
		if im.ArtifactType == artifactTypeBundle {
			found = append(found, d)
			index = im
		}
	}
	if len(found) != 1 {
		return v1.Descriptor{}, nil, fmt.Errorf("the layout holds %d bundles, want one", len(found))
	}
	return found[0], index, nil
}

// checkSignature accepts the bundle index digest when a cosign signature among the layout's referrers of it verifies
// with key.
func (b *Bundle) checkSignature(key *ecdsa.PublicKey, top *v1.IndexManifest, digest v1.Hash) error {
	var lastErr error
	for _, d := range top.Manifests {
		if d.MediaType != types.OCIManifestSchema1 {
			continue
		}
		data, err := b.readBlob(d, maxManifestSize)
		if err != nil {
			lastErr = err
			continue
		}
		m, err := v1.ParseManifest(bytes.NewReader(data))
		if err != nil || m.Subject == nil || m.Subject.Digest != digest || m.ArtifactType != mediaTypeSigstoreBundle ||
			len(m.Layers) != 1 || m.Layers[0].MediaType != mediaTypeSigstoreBundle {
			continue
		}
		layer, err := b.readBlob(m.Layers[0], maxManifestSize)
		if err == nil {
			err = verifySigstoreBundle(key, layer, digest)
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errNotSignature) {
			continue
		}
		lastErr = fmt.Errorf("signature %s: %w", d.Digest, err)
	}
	if lastErr == nil {
		return ErrUnsigned
	}
	return fmt.Errorf("%w: %w", ErrBadSignature, lastErr)
}

// errNotSignature marks a sigstore bundle that is an attestation, such as an SBOM, rather than a signature.
var errNotSignature = errors.New("not a cosign signature")

type sigstoreBundle struct {
	MediaType    string `json:"mediaType"`
	DSSEEnvelope struct {
		Payload     string          `json:"payload"`
		PayloadType string          `json:"payloadType"`
		Signatures  []dsseSignature `json:"signatures"`
	} `json:"dsseEnvelope"`
}

type dsseSignature struct {
	Sig string `json:"sig"`
}

type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
}

// verifySigstoreBundle checks a sigstore bundle's DSSE envelope signature and that its in-toto statement is a cosign
// signature of digest.
func verifySigstoreBundle(key *ecdsa.PublicKey, raw []byte, digest v1.Hash) error {
	var sb sigstoreBundle
	if err := json.Unmarshal(raw, &sb); err != nil {
		return err
	}
	env := sb.DSSEEnvelope
	if sb.MediaType != mediaTypeSigstoreBundle || env.PayloadType != dssePayloadType {
		return fmt.Errorf("sigstore bundle %q with payload %q", sb.MediaType, env.PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pae(env.PayloadType, payload))
	if !verifiesAny(key, sum[:], env.Signatures) {
		return errors.New("no signature verifies with the public key")
	}
	var st statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return err
	}
	if st.Type != statementType {
		return fmt.Errorf("statement %q", st.Type)
	}
	if st.PredicateType != cosignPredicate {
		return fmt.Errorf("%w: predicate %q", errNotSignature, st.PredicateType)
	}
	for _, s := range st.Subject {
		if s.Digest["sha256"] == digest.Hex {
			return nil
		}
	}
	return errors.New("the signed statement does not name the bundle index")
}

func verifiesAny(key *ecdsa.PublicKey, hash []byte, sigs []dsseSignature) bool {
	for _, s := range sigs {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err == nil && ecdsa.VerifyASN1(key, hash, sig) {
			return true
		}
	}
	return false
}

// pae is the DSSE pre-authentication encoding of a payload.
func pae(payloadType string, payload []byte) []byte {
	return fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload)
}

// readMetadata reads the metadata artifact, the first entry of the signed bundle index, and checks its layers.
func (b *Bundle) readMetadata(index *v1.IndexManifest) (*verified, error) {
	if len(index.Manifests) == 0 || index.Manifests[0].ArtifactType != artifactTypeBundle ||
		index.Manifests[0].MediaType != types.OCIManifestSchema1 {
		return nil, errors.New("the bundle index does not start with the bundle metadata")
	}
	data, err := b.readBlob(index.Manifests[0], maxManifestSize)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	m, err := v1.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("bundle metadata: %w", err)
	}
	config, err := b.readBlob(m.Config, maxManifestSize)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
	}
	md, err := parseMetadata(config)
	if err != nil {
		return nil, err
	}
	v := &verified{metadata: md}
	for _, l := range m.Layers {
		switch l.Annotations[annotationTitle] {
		case notesTitle:
			v.notes = l
		case manifestsTitle:
			v.files = l
		default:
			continue
		}
		if err := b.checkBlob(l); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadSignature, err)
		}
	}
	if v.notes.Digest.Hex == "" || v.files.Digest.Hex == "" {
		return nil, errors.New("the bundle metadata lacks the notes or the manifests")
	}
	return v, nil
}

// listedImages returns the descriptor in the bundle index of each image, found by its reference and its digest. The
// index may list nothing else besides the metadata, as a push copies all it lists.
func listedImages(index *v1.IndexManifest, images []ImageRef) ([]v1.Descriptor, error) {
	// Several references may name the same manifest, so a child is known by its reference and its digest.
	type key struct{ ref, digest string }
	children := map[key]v1.Descriptor{}
	for _, d := range index.Manifests[1:] {
		children[key{d.Annotations[annotationRefName], d.Digest.String()}] = d
	}
	listed := make([]v1.Descriptor, 0, len(images))
	named := map[key]bool{}
	for _, ref := range images {
		k := key{ref.Ref, ref.Digest}
		d, ok := children[k]
		if !ok || !d.MediaType.IsImage() {
			return nil, fmt.Errorf("%w: %s %s is not in the bundle index", ErrMissingImage, ref.Ref, ref.Digest)
		}
		listed = append(listed, d)
		named[k] = true
	}
	for k := range children {
		if !named[k] {
			return nil, fmt.Errorf("the bundle index lists %s %s, which the metadata does not", k.ref, k.digest)
		}
	}
	return listed, nil
}

// checkImages checks that every listed image is in the bundle index with its reference and that all its blobs are
// present and intact.
func (b *Bundle) checkImages(index *v1.IndexManifest, images []ImageRef) error {
	listed, err := listedImages(index, images)
	if err != nil {
		return err
	}
	for i, ref := range images {
		size, err := b.checkImage(listed[i])
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrMissingImage, ref.Ref, err)
		}
		if size != ref.Size {
			return fmt.Errorf("%w: %s has %d bytes, the metadata lists %d", ErrMissingImage, ref.Ref, size, ref.Size)
		}
	}
	return nil
}

// checkImage checks the manifest, config and layers of an image and returns their total size.
func (b *Bundle) checkImage(d v1.Descriptor) (int64, error) {
	data, err := b.readBlob(d, maxManifestSize)
	if err != nil {
		return 0, err
	}
	m, err := v1.ParseManifest(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	size := d.Size
	for _, blob := range append([]v1.Descriptor{m.Config}, m.Layers...) {
		if err := b.checkBlob(blob); err != nil {
			return 0, err
		}
		size += blob.Size
	}
	return size, nil
}
