package console

import (
	"io/fs"

	"github.com/OmniTrustILM/appliance-ng/web"
)

// Files returns the built console, rooted at the directory that holds index.html.
func Files() fs.FS {
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err) // "dist" is a valid constant path
	}
	return sub
}
