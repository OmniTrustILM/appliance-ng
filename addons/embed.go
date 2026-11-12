// Package addons holds the add-on manifests applied to the cluster at first boot,
// the default platform and order.yaml, which gives their apply order and readiness waits.
package addons

import "embed"

// FS is the embedded add-on filesystem.
//
//go:embed *.yaml
var FS embed.FS
