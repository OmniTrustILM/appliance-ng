// Command release validates the release manifest and lists the images to cache.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "validate":
		os.Exit(validate(os.Args[2:]))
	case "images":
		os.Exit(images(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release validate [-f release.yaml] [--addons addons]")
	fmt.Fprintln(os.Stderr, "       release images [-f release.yaml] --arch amd64|arm64 [--installer <ref>]")
	os.Exit(2)
}

func validate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	file := fs.String("f", "release.yaml", "release manifest")
	addons := fs.String("addons", "addons", "directory with the add-on files")
	_ = fs.Parse(args)

	m, err := release.Load(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	errs := release.Validate(m, os.DirFS(*addons))
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, err)
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}

func images(args []string) int {
	fs := flag.NewFlagSet("images", flag.ExitOnError)
	file := fs.String("f", "release.yaml", "release manifest")
	arch := fs.String("arch", "", "target architecture, amd64 or arm64")
	installer := fs.String("installer", "", "installer image reference to add")
	_ = fs.Parse(args)
	if *arch != "amd64" && *arch != "arm64" {
		fmt.Fprintln(os.Stderr, "--arch must be amd64 or arm64")
		return 2
	}

	m, err := release.Load(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, ref := range release.CacheList(m, *arch, *installer) {
		fmt.Println(ref)
	}
	return 0
}
