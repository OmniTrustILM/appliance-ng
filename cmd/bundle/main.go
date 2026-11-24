// Command bundle builds unsigned update bundles and verifies signed ones.
package main

import (
	"context"
	"crypto"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/OmniTrustILM/appliance-ng/internal/bundle"
	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "build":
		os.Exit(build(os.Args[2:]))
	case "verify":
		os.Exit(verify(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bundle build --kind system|platform --arch amd64|arm64 [--image <ref>@sha256:<hex>]... [--notes notes.md] [--root .] --out <dir>")
	fmt.Fprintln(os.Stderr, "       bundle verify [--key cosign.pub] --arch amd64|arm64 <layout dir or tar>")
	os.Exit(2)
}

func build(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	root := fs.String("root", ".", "repository root holding release.yaml and addons/")
	kind := fs.String("kind", "", "bundle kind, system or platform")
	arch := fs.String("arch", "", "target architecture, amd64 or arm64")
	notesFile := fs.String("notes", "", "release notes in Markdown")
	out := fs.String("out", "", "directory to write the OCI layout to")
	var built stringList
	fs.Var(&built, "image", "image built with the appliance, as <ref>@sha256:<hex>, for the system bundle; repeatable")
	_ = fs.Parse(args)
	if *kind == "" || *arch == "" || *out == "" {
		usage()
	}

	m, err := release.Load(filepath.Join(*root, "release.yaml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var notes []byte
	if *notesFile != "" {
		if notes, err = os.ReadFile(*notesFile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	md, err := bundle.Build(ctx, m, *kind, *arch, bundle.Inputs{Files: os.DirFS(*root), Notes: notes, Built: built}, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("built the %s bundle %s-%s with %d images in %s\n", md.Kind, md.Version, md.Arch, len(md.Images), *out)
	return 0
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func verify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	keyFile := fs.String("key", "", "cosign public key; the embedded cosign.pub when empty")
	arch := fs.String("arch", "", "architecture the bundle must be for, amd64 or arm64")
	_ = fs.Parse(args)
	if *arch == "" || fs.NArg() != 1 {
		usage()
	}

	pub := bundle.PublicKey()
	if *keyFile != "" {
		data, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if pub, err = bundle.ParsePublicKey(data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return verifyBundle(fs.Arg(0), pub, *arch)
}

func verifyBundle(path string, pub crypto.PublicKey, arch string) int {
	b, err := bundle.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = b.Close() }()
	md, err := b.Verify(pub, arch)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("verified the %s bundle %s-%s with %d images\n", md.Kind, md.Version, md.Arch, len(md.Images))
	return 0
}
