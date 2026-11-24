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
	// ManagerImage is the placeholder reference the add-ons use for the Appliance Manager image, which is built
	// with the appliance and has no fixed reference; the first-boot service replaces it with the built one.
	ManagerImage string      `json:"managerImage,omitempty"`
	Addons       []AddonFile `json:"addons"`
	// Charts are the Helm charts the manager renders itself, vendored next to the add-on files.
	Charts     []Chart     `json:"charts,omitempty"`
	Images     []Image     `json:"images"`
	Connectors []Connector `json:"connectors,omitempty"`
}

// Chart is a Helm chart archive the manager renders as templates, never installed as a Helm release.
type Chart struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Version string `json:"version"`
	// Source is where the archive was downloaded from.
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// Chart returns the chart the manifest pins under name.
func (m *Manifest) Chart(name string) (Chart, error) {
	for _, c := range m.Charts {
		if c.Name == name {
			return c, nil
		}
	}
	return Chart{}, fmt.Errorf("the release pins no chart %s", name)
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

// Database kinds of Connector.Database.
const (
	// DatabaseJDBC passes the database as JDBC_URL, JDBC_USERNAME and JDBC_PASSWORD.
	DatabaseJDBC = "jdbc"
	// DatabaseEnv passes the database as DATABASE_HOST, _PORT, _NAME, _SCHEMA, _USER and _PASSWORD.
	DatabaseEnv = "env"
)

// Connector settings of Connector.Settings: the Appliance areas a connector reads.
const (
	SettingProxy = "proxy"
	SettingTrust = "trust"
	SettingMail  = "mail"
)

// Interface versions of Connector.Interface.
const (
	InterfaceV1 = "v1"
	InterfaceV2 = "v2"
)

// Connector is an ILM connector the appliance can install.
type Connector struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Image is the reference with its tag; it must also be listed in Images.
	Image  string `json:"image"`
	Digest string `json:"digest"`
	Port   int32  `json:"port"`
	// Interface is the connector interface version, InterfaceV1 or InterfaceV2; empty means InterfaceV2. The operator
	// registers a connector with ILM as v2 only, so a v1 connector is installed unregistered.
	Interface string `json:"interface,omitempty"`
	// Private images need registry credentials and are not cached.
	Private bool `json:"private,omitempty"`
	// HealthPath is the liveness path; a path ending in /liveness has a /readiness sibling.
	HealthPath string            `json:"healthPath"`
	Env        map[string]string `json:"env,omitempty"`
	Settings   []string          `json:"settings,omitempty"`
	// Database is DatabaseJDBC, DatabaseEnv or empty for none.
	Database string `json:"database,omitempty"`
	// Schema is the connector's database schema when the connector reads it from its environment.
	Schema string `json:"schema,omitempty"`
	// Keys names the variables that hold a random key generated on the device.
	Keys []string `json:"keys,omitempty"`
}

// V1Only reports whether the connector speaks interface v1 only.
func (c Connector) V1Only() bool { return c.Interface == InterfaceV1 }

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
	return Parse(data, path)
}

// Parse checks and returns the manifest in data; source names it in errors.
func Parse(data []byte, source string) (*Manifest, error) {
	var m Manifest
	strict := func(d *json.Decoder) *json.Decoder {
		d.DisallowUnknownFields()
		return d
	}
	if err := yaml.Unmarshal(data, &m, strict); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", source, err)
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
