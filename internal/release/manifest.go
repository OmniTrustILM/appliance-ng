// Package release models the release manifest that pins every version and image
// of the appliance, and validates it against the add-on manifests.
package release

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Manifest is the content of release.yaml.
type Manifest struct {
	Version        string           `json:"version"`
	Talos          TalosPin         `json:"talos"`
	Platform       PlatformDefaults `json:"platform"`
	ImplicitImages []string         `json:"implicitImages,omitempty"`
	Addons         []AddonFile      `json:"addons"`
	Images         []Image          `json:"images"`
}

// TalosPin pins the Talos release the appliance is built on.
type TalosPin struct {
	Version    string `json:"version"`
	Kubernetes string `json:"kubernetes"`
	// Imager is the imager image reference including its digest.
	Imager string `json:"imager"`
}

// Image is a container image shipped in the offline cache.
type Image struct {
	// Ref is the reference exactly as the manifests write it; it may carry a digest.
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// Arches limits the image to the listed architectures; empty means all.
	Arches []string `json:"arches,omitempty"`
}

// AddonFile is a manifest file applied to the cluster at install time.
type AddonFile struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Version string `json:"version"`
	// Source is the upstream URL or helm:<chart>@<version>.
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// PlatformDefaults are the defaults of the ILM platform resource.
type PlatformDefaults struct {
	ILMVersion string `json:"ilmVersion"`
	HostName   string `json:"hostName"`
}

// Load reads and checks the manifest at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the tool's own input
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	strict := func(d *json.Decoder) *json.Decoder {
		d.DisallowUnknownFields()
		return d
	}
	if err := yaml.Unmarshal(data, &m, strict); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	for _, img := range m.Images {
		if err := img.check(); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func (i Image) check() error {
	if i.Digest == "" {
		return fmt.Errorf("image %q has no digest", i.Ref)
	}
	if _, digest, found := strings.Cut(i.Ref, "@"); found && digest != i.Digest {
		return fmt.Errorf("image %q: ref digest differs from digest %s", i.Ref, i.Digest)
	}
	return nil
}
