package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/OmniTrustILM/operator/pkg/bom"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	platformAddon       = "platform"
	operatorManifestURL = "https://github.com/OmniTrustILM/operator/releases/download/v%s/ilm-operator.yaml"

	releaseFile   = "release.yaml"
	platformFile  = "addons/platform.yaml"
	downloadsFile = "addons/src/downloads.txt"
)

// runtimeDependencies are the repositories whose versions the manager's runtime defaults name.
var runtimeDependencies = []string{"ghcr.io/cloudnative-pg/postgresql", "index.docker.io/library/rabbitmq"}

var (
	refField       = regexp.MustCompile(`ref: "([^"]+)"`)
	ilmVersionLine = regexp.MustCompile(`^(\s*ilmVersion:\s*)\S+$`)
	specVersion    = regexp.MustCompile(`^(\s*version:\s*)"[^"]*"$`)
)

func pin(args []string) int {
	fs := flag.NewFlagSet("pin", flag.ExitOnError)
	root := fs.String("root", ".", "repository root holding release.yaml and addons/")
	operator := fs.String("operator", "", "ILM operator release, such as 1.2.0")
	ilm := fs.String("ilm", bom.DefaultVersion, "ILM platform version; the operator's default version by default")
	olderILM := fs.Bool("older-ilm", false, "allow an ILM version older than platform.ilmVersion, for a test build")
	_ = fs.Parse(args)
	if *operator == "" {
		usage()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	op := strings.TrimPrefix(*operator, "v")
	if err := (pinner{transport: remote.DefaultTransport, olderILM: *olderILM}).pin(ctx, *root, op, *ilm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("pinned ILM %s with operator %s\n", *ilm, op)
	return 0
}

// pinner pins an ILM operator release and an ILM platform version in release.yaml and the add-on files.
type pinner struct {
	transport http.RoundTripper
	// olderILM lets the ILM version move backwards, for a test build; the operator never does.
	olderILM bool
}

// pin rewrites, under root, the ilm-operator add-on (its file, entry and download line), the platform add-on and
// platform.ilmVersion, and the digest of each platform image the operator's bill of materials and manifest name.
// It refuses to move either version backwards, the ILM version unless olderILM is set, and changes no file when a
// check, a download or a write fails.
func (p pinner) pin(ctx context.Context, root, operator, ilm string) error {
	m, err := release.Load(filepath.Join(root, releaseFile))
	if err != nil {
		return err
	}
	current, err := p.checkVersions(m, operator, ilm)
	if err != nil {
		return err
	}

	refs, err := release.BOMImageRefs(ilm)
	if err != nil {
		return err
	}
	if err := refuseDependencyChange(m, refs); err != nil {
		return err
	}
	source := fmt.Sprintf(operatorManifestURL, operator)
	manifest, err := p.download(ctx, source)
	if err != nil {
		return err
	}
	operatorImages, err := release.ImagesIn(manifest)
	if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	refs = append(refs, operatorImages...)

	files := map[string][]byte{"addons/ilm-operator.yaml": manifest}
	for _, file := range []string{releaseFile, platformFile, downloadsFile} {
		if files[file], err = os.ReadFile(filepath.Join(root, file)); err != nil {
			return err
		}
	}
	if files[platformFile], err = editLine(files[platformFile], "the platform version",
		specVersion.MatchString, func(line string) string { return specVersion.ReplaceAllString(line, `${1}"`+ilm+`"`) }); err != nil {
		return err
	}
	if files[downloadsFile], err = editLine(files[downloadsFile], "the "+release.OperatorAddon+" download",
		func(line string) bool { return strings.HasPrefix(line, current.File+" ") },
		func(string) string { return current.File + " " + source }); err != nil {
		return err
	}
	if files[releaseFile], err = p.editRelease(ctx, files, ilm, "v"+operator, source, refs); err != nil {
		return err
	}

	return writeAll(root, files)
}

// checkVersions refuses to move the operator of m backwards, and its ILM version unless olderILM is set, and returns
// the operator's add-on.
func (p pinner) checkVersions(m *release.Manifest, operator, ilm string) (release.AddonFile, error) {
	current, err := addon(m, release.OperatorAddon)
	if err != nil {
		return release.AddonFile{}, err
	}
	if err := refuseDowngrade("operator", current.Version, operator); err != nil {
		return release.AddonFile{}, err
	}
	if !p.olderILM {
		if err := refuseDowngrade("ILM", m.Platform.ILMVersion, ilm); err != nil {
			return release.AddonFile{}, err
		}
	}
	return current, nil
}

// writeAll writes every file to a temporary file next to it first and renames them into place only when all were
// written, so that a failed write leaves the files as they were. A failed rename can leave earlier files replaced.
func writeAll(root string, files map[string][]byte) error {
	temps := map[string]string{}
	defer func() {
		for _, tmp := range temps {
			_ = os.Remove(tmp)
		}
	}()
	for file, data := range files {
		path := filepath.Join(root, file)
		tmp := path + ".pin"
		if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // repository files
			return err
		}
		temps[path] = tmp
	}
	for path, tmp := range temps {
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		delete(temps, path)
	}
	return nil
}

// editRelease rewrites release.yaml line by line, so that its comments, layout and order stay as they are.
func (p pinner) editRelease(ctx context.Context, files map[string][]byte, ilm, operator, source string, refs []string) ([]byte, error) {
	data := files[releaseFile]
	data, err := editLine(data, "the "+release.OperatorAddon+" add-on", addonLine(release.OperatorAddon), func(line string) string {
		line = setField(line, "version", operator)
		line = setField(line, "source", source)
		return setField(line, "sha256", sum(files["addons/ilm-operator.yaml"]))
	})
	if err != nil {
		return nil, err
	}
	if data, err = editLine(data, "the "+platformAddon+" add-on", addonLine(platformAddon), func(line string) string {
		return setField(setField(line, "version", ilm), "sha256", sum(files[platformFile]))
	}); err != nil {
		return nil, err
	}
	if data, err = editLine(data, "platform.ilmVersion", ilmVersionLine.MatchString, func(line string) string {
		return ilmVersionLine.ReplaceAllString(line, "${1}"+ilm)
	}); err != nil {
		return nil, err
	}
	return p.pinImages(ctx, data, refs)
}

// pinImages rewrites the reference and digest of the image of each repository in refs.
func (p pinner) pinImages(ctx context.Context, data []byte, refs []string) ([]byte, error) {
	seen := map[string]bool{}
	for _, ref := range refs {
		repo, err := repository(ref)
		if err != nil {
			return nil, err
		}
		if seen[repo] {
			continue
		}
		seen[repo] = true
		digest, err := p.digest(ctx, ref)
		if err != nil {
			return nil, err
		}
		if data, err = editLine(data, "the image "+repo, imageLine(repo), func(line string) string {
			newRef := ref
			if old := refField.FindStringSubmatch(line)[1]; strings.Contains(old, "@") && !strings.Contains(ref, "@") {
				newRef += "@" + digest
			}
			return setField(setField(line, "ref", newRef), "digest", digest)
		}); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func addon(m *release.Manifest, name string) (release.AddonFile, error) {
	for _, a := range m.Addons {
		if a.Name == name {
			return a, nil
		}
	}
	return release.AddonFile{}, fmt.Errorf("release.yaml has no %s add-on", name)
}

// refuseDependencyChange refuses a bill of materials that moves PostgreSQL or RabbitMQ to another version: the
// Platform the manager renders and the PostgreSQL image it runs name those versions in code.
func refuseDependencyChange(m *release.Manifest, refs []string) error {
	for _, ref := range refs {
		next, err := name.ParseReference(ref)
		if err != nil {
			return err
		}
		if !slices.Contains(runtimeDependencies, next.Context().Name()) {
			continue
		}
		for _, img := range m.Images {
			listed, _, _ := strings.Cut(img.Ref, "@")
			current, err := name.ParseReference(listed)
			if err == nil && current.Context().Name() == next.Context().Name() && current.Name() != next.Name() {
				return fmt.Errorf("the bill of materials moves %s to %s; update the runtime defaults in internal/platform and cmd/manager by hand first", listed, ref)
			}
		}
	}
	return nil
}

func refuseDowngrade(what, current, next string) error {
	from, err := semver.NewVersion(current)
	if err != nil {
		return fmt.Errorf("current %s version %q: %w", what, current, err)
	}
	to, err := semver.NewVersion(next)
	if err != nil {
		return fmt.Errorf("%s version %q: %w", what, next, err)
	}
	if to.LessThan(from) {
		return fmt.Errorf("refusing to downgrade the %s from %s to %s", what, current, next)
	}
	return nil
}

func (p pinner) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Transport: p.transport}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// digest returns the digest the registry serves for ref, or the digest ref already names.
func (p pinner) digest(ctx context.Context, ref string) (string, error) {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return digest, nil
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(parsed, remote.WithContext(ctx), remote.WithTransport(p.transport),
		remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return desc.Digest.String(), nil
}

// repository returns the full repository name of ref, so that "rabbitmq" and "docker.io/library/rabbitmq" agree.
func repository(ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}
	return parsed.Context().Name(), nil
}

func addonLine(name string) func(string) bool {
	pattern := regexp.MustCompile(`\{\s*name:\s*` + regexp.QuoteMeta(name) + `\s*,`)
	return pattern.MatchString
}

func imageLine(repo string) func(string) bool {
	return func(line string) bool {
		match := refField.FindStringSubmatch(line)
		if match == nil {
			return false
		}
		got, err := repository(match[1])
		return err == nil && got == repo
	}
}

// setField replaces the quoted value of key in a flow-style line.
func setField(line, key, value string) string {
	pattern := regexp.MustCompile(`(\b` + regexp.QuoteMeta(key) + `:\s*)"[^"]*"`)
	return pattern.ReplaceAllString(line, "${1}"+strings.ReplaceAll(`"`+value+`"`, "$", "$$"))
}

// editLine replaces the one line of data that match selects with edit's result.
func editLine(data []byte, what string, match func(string) bool, edit func(string) string) ([]byte, error) {
	lines := bytes.Split(data, []byte("\n"))
	found := -1
	for i, line := range lines {
		if !match(string(line)) {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("more than one line holds %s", what)
		}
		found = i
	}
	if found < 0 {
		return nil, fmt.Errorf("no line holds %s; add it by hand", what)
	}
	lines[found] = []byte(edit(string(lines[found])))
	return bytes.Join(lines, []byte("\n")), nil
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}
