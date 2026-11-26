// Package bundle builds, reads and verifies signed update bundles: per-arch OCI image indexes holding a metadata
// artifact and every image of one kind of the release, signed with cosign.
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

// FormatVersion is the newest metadata format this package writes and reads.
const FormatVersion = 1

// Media and artifact types of a bundle and of its cosign signature.
const (
	artifactTypeBundle      = "application/vnd.otilm.appliance.bundle.v1+json"
	mediaTypeBundleConfig   = "application/vnd.otilm.appliance.bundle.config.v1+json"
	mediaTypeNotes          = "text/markdown"
	mediaTypeManifests      = "application/vnd.otilm.appliance.manifests.v1.tar"
	mediaTypeSigstoreBundle = "application/vnd.dev.sigstore.bundle.v0.3+json"
	dssePayloadType         = "application/vnd.in-toto+json"
	statementType           = "https://in-toto.io/Statement/v1"
	cosignPredicate         = "https://sigstore.dev/cosign/sign/v1"

	annotationRefName = "org.opencontainers.image.ref.name"
	annotationTitle   = "org.opencontainers.image.title"
	annotationVersion = "org.opencontainers.image.version"

	notesTitle     = "notes.md"
	manifestsTitle = "manifests.tar"
)

// Names of the notes and the metadata in a stored bundle's directory, beside the bundle's files.
const (
	NotesFile    = "notes.md"
	MetadataFile = "bundle.json"
)

// Reasons a bundle is refused.
var (
	ErrUnsigned     = errors.New("the bundle is not signed")
	ErrBadSignature = errors.New("the bundle signature is not valid")
	ErrWrongArch    = errors.New("the bundle is for another architecture")
	ErrMissingImage = errors.New("the bundle lacks an image or has a damaged one")
	ErrFormatTooNew = errors.New("the bundle format is newer than this appliance reads")
	ErrTruncated    = errors.New("the bundle file is truncated")
)

// Metadata describes a bundle; it is the bundle's bundle.json.
type Metadata struct {
	Format  int       `json:"format"`
	Kind    string    `json:"kind"`
	Version string    `json:"version"`
	Arch    string    `json:"arch"`
	Created time.Time `json:"created"`
	// Talos, Kubernetes and OperatorRange are set in a system bundle.
	Talos         string `json:"talos,omitempty"`
	Kubernetes    string `json:"kubernetes,omitempty"`
	OperatorRange string `json:"operatorRange,omitempty"`
	// Operator, ILM and MinAppliance are set in a platform bundle.
	Operator     string `json:"operator,omitempty"`
	ILM          string `json:"ilm,omitempty"`
	MinAppliance string `json:"minAppliance,omitempty"`
	// Images are the images of the bundle in release.yaml order.
	Images []ImageRef `json:"images"`
	// Files are the names of the manifests the bundle carries.
	Files []string `json:"files"`
}

// ImageRef is an image of a bundle.
type ImageRef struct {
	// Ref is the reference exactly as release.yaml writes it.
	Ref string `json:"ref"`
	// Digest is the digest of the image manifest for the bundle's arch.
	Digest string `json:"digest"`
	// Size is the size of the image manifest, its config and its layers in bytes.
	Size int64 `json:"size"`
}

// parseMetadata parses bundle.json, refusing a format newer than FormatVersion.
func parseMetadata(data []byte) (Metadata, error) {
	var format struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &format); err != nil {
		return Metadata{}, fmt.Errorf("parse bundle metadata: %w", err)
	}
	if format.Format > FormatVersion {
		return Metadata{}, fmt.Errorf("%w: format %d", ErrFormatTooNew, format.Format)
	}
	var md Metadata
	if err := json.Unmarshal(data, &md); err != nil {
		return Metadata{}, fmt.Errorf("parse bundle metadata: %w", err)
	}
	if md.Format < 1 || !slices.Contains([]string{release.BundleSystem, release.BundlePlatform}, md.Kind) {
		return Metadata{}, fmt.Errorf("bundle metadata: format %d, kind %q", md.Format, md.Kind)
	}
	return md, nil
}
