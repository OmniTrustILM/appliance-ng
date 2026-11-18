// Package web holds the built web console.
package web

import "embed"

// Dist is the built console; its files are under dist/.
//
//go:embed dist
var Dist embed.FS
