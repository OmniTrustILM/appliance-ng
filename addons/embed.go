// Package addons holds the add-on manifests applied to the cluster at first boot,
// the default platform and order.yaml, which gives their apply order and readiness waits.
package addons

import "embed"

// FS is the embedded add-on filesystem, with the Helm chart archives the manager renders under charts/.
//
//go:embed *.yaml charts/*.tgz
var FS embed.FS
