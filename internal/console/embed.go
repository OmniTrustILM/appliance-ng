package console

import (
	"io/fs"

	"github.com/OmniTrustILM/appliance-ng/web"
)

// Files returns the built console, rooted at the directory that holds index.html.
func Files() fs.FS {
	return files(web.Dist)
}

// files returns the console build under dist/app, or the placeholder page in dist when the console is not built.
func files(dist fs.FS) fs.FS {
	root := "dist"
	if _, err := fs.Stat(dist, "dist/app/index.html"); err == nil {
		root = "dist/app"
	}
	sub, err := fs.Sub(dist, root)
	if err != nil {
		panic(err) // root is a valid constant path
	}
	return sub
}
