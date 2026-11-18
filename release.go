// Package appliance holds the release manifest the appliance binaries are built with.
package appliance

// embed is imported for the go:embed directive below.
import _ "embed"

// ReleaseManifest is the content of release.yaml.
//
//go:embed release.yaml
var ReleaseManifest []byte
