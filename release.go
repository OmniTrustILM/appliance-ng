// Package appliance holds the release manifest and the bundle signing key the appliance binaries are built with.
package appliance

// embed is imported for the go:embed directive below.
import _ "embed"

// ReleaseManifest is the content of release.yaml.
//
//go:embed release.yaml
var ReleaseManifest []byte

// CosignPublicKey is the content of cosign.pub, the public key update bundles are signed with.
//
//go:embed cosign.pub
var CosignPublicKey []byte
