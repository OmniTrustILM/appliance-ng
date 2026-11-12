package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

var imageRefPattern = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?/[^ ]+:[^ ]+$`)

// Validate checks every add-on file against its pinned hash and reports each
// image the files use that the manifest does not list.
func Validate(m *Manifest, addons fs.FS) []error {
	listed := make(map[string]bool, len(m.Images))
	for _, img := range m.Images {
		listed[normalize(img.Ref)] = true
	}
	implicit := make(map[string]bool, len(m.ImplicitImages))
	for _, name := range m.ImplicitImages {
		implicit[name] = true
	}

	var errs []error
	reported := map[string]bool{}
	for _, addon := range m.Addons {
		data, err := readAddon(addons, addon)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		images, err := imagesIn(data, implicit)
		if err != nil {
			errs = append(errs, fmt.Errorf("addon %s (%s): %w", addon.Name, addon.File, err))
			continue
		}
		for _, img := range images {
			if ref := normalize(img); !listed[ref] && !reported[ref] {
				reported[ref] = true
				errs = append(errs, fmt.Errorf("addon %s (%s): image %s is not listed in the manifest", addon.Name, addon.File, ref))
			}
		}
	}
	return errs
}

func readAddon(addons fs.FS, addon AddonFile) ([]byte, error) {
	if addons == nil {
		return nil, fmt.Errorf("addon %s: no add-on files available to read %s", addon.Name, addon.File)
	}
	data, err := fs.ReadFile(addons, addon.File)
	if err != nil {
		return nil, fmt.Errorf("addon %s: %w", addon.Name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != addon.SHA256 {
		return nil, fmt.Errorf("addon %s (%s): sha256 is %s, manifest pins %s", addon.Name, addon.File, got, addon.SHA256)
	}
	return data, nil
}

// imagesIn returns the image references used by the documents of a manifest file.
func imagesIn(data []byte, implicit map[string]bool) ([]string, error) {
	var images []string
	reader := k8syaml.NewYAMLReader(bufioReader(data))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return images, nil
		}
		if err != nil {
			return nil, err
		}
		var obj map[string]any
		if err := k8syaml.Unmarshal(doc, &obj); err != nil {
			return nil, err
		}
		if obj["kind"] == "ConfigMap" {
			collectStrings(obj["data"], implicit, &images)
			continue
		}
		walk(obj, implicit, &images)
	}
}

// walk finds image fields, container args and env values in a workload.
func walk(node any, implicit map[string]bool, images *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			switch key {
			case "image":
				if s, ok := child.(string); ok && s != "" {
					*images = append(*images, s)
				}
			case "args", "env":
				collectStrings(child, implicit, images)
			}
			walk(child, implicit, images)
		}
	case []any:
		for _, child := range v {
			walk(child, implicit, images)
		}
	}
}

// collectStrings adds every token of the string values below node that looks
// like an image reference.
func collectStrings(node any, implicit map[string]bool, images *[]string) {
	switch v := node.(type) {
	case string:
		for _, token := range strings.FieldsFunc(v, isSeparator) {
			if imageRefPattern.MatchString(token) || implicit[token] {
				*images = append(*images, token)
			}
		}
	case map[string]any:
		for _, child := range v {
			collectStrings(child, implicit, images)
		}
	case []any:
		for _, child := range v {
			collectStrings(child, implicit, images)
		}
	}
}

func isSeparator(r rune) bool {
	return strings.ContainsRune(" \t\r\n=,\"'", r)
}

func bufioReader(data []byte) *bufio.Reader {
	return bufio.NewReader(bytes.NewReader(data))
}

// AddonImageNames returns the sorted names, without tag and digest and as
// written in the manifest, of the listed images that the add-on files use.
func AddonImageNames(m *Manifest, addons fs.FS) ([]string, error) {
	implicit := make(map[string]bool, len(m.ImplicitImages))
	for _, name := range m.ImplicitImages {
		implicit[name] = true
	}
	used := map[string]bool{}
	for _, addon := range m.Addons {
		data, err := readAddon(addons, addon)
		if err != nil {
			return nil, err
		}
		images, err := imagesIn(data, implicit)
		if err != nil {
			return nil, fmt.Errorf("addon %s (%s): %w", addon.Name, addon.File, err)
		}
		for _, img := range images {
			used[normalize(img)] = true
		}
	}

	var names []string
	for _, img := range m.Images {
		if used[normalize(img.Ref)] {
			names = append(names, imageName(img.Ref))
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// imageName strips the digest and the tag from ref.
func imageName(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if hasTag(name) {
		name = name[:strings.LastIndex(name, ":")]
	}
	return name
}
