// Package web holds the web console.
package web

import "embed"

// Dist holds dist/index.html, a placeholder page, and the console build in dist/app when it was built.
//
//go:embed all:dist
var Dist embed.FS
