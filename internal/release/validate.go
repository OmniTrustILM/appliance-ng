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

// Validate checks the release version, the compatibility and the bundle of every image, every add-on file and
// chart archive against its pinned hash, and reports each image the files or the connector catalogue use that the
// manifest does not list, and a manager image placeholder that no add-on uses.
func Validate(m *Manifest, addons fs.FS) []error {
	errs, usage := validateContent(m, addons)
	return append(errs, validateBundles(m, usage)...)
}

// validateContent checks the add-on files, charts and connectors. It also returns the images the add-ons use,
// normalized, with the names of the add-ons using each.
func validateContent(m *Manifest, addons fs.FS) ([]error, map[string][]string) {
	listed := make(map[string]string, len(m.Images))
	for _, img := range m.Images {
		listed[normalize(img.Ref)] = img.Digest
	}
	implicit := make(map[string]bool, len(m.ImplicitImages))
	for _, name := range m.ImplicitImages {
		implicit[name] = true
	}

	errs := validateConnectors(m.Connectors, listed)
	manager := ""
	if m.ManagerImage != "" {
		manager = normalize(m.ManagerImage)
	}
	usage := map[string][]string{}
	addonErrs, managerUsed := validateAddons(m.Addons, addons, listed, implicit, manager, usage)
	errs = append(errs, addonErrs...)
	errs = append(errs, validateCharts(m.Charts, addons)...)
	if manager != "" && !managerUsed {
		errs = append(errs, fmt.Errorf("manager image %s is used by no add-on", m.ManagerImage))
	}
	return errs, usage
}

// validateAddons checks each add-on file against its pinned hash and reports each image it uses that listed lacks,
// once; implicit names the images used without a registry. It records in usage the add-ons using each image and
// tells whether an add-on uses the manager image.
func validateAddons(files []AddonFile, addons fs.FS, listed map[string]string, implicit map[string]bool, manager string, usage map[string][]string) ([]error, bool) {
	var errs []error
	managerUsed := false
	reported := map[string]bool{}
	for _, addon := range files {
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
			ref := normalize(img)
			if ref == manager {
				managerUsed = true
				continue
			}
			if !slices.Contains(usage[ref], addon.Name) {
				usage[ref] = append(usage[ref], addon.Name)
			}
			if _, ok := listed[ref]; !ok && !reported[ref] {
				reported[ref] = true
				errs = append(errs, fmt.Errorf("addon %s (%s): image %s is not listed in the manifest", addon.Name, addon.File, ref))
			}
		}
	}
	return errs, managerUsed
}

// validateCharts checks each chart archive against its pinned hash.
func validateCharts(charts []Chart, addons fs.FS) []error {
	var errs []error
	for _, c := range charts {
		if _, err := readAddon(addons, AddonFile{Name: c.Name, File: c.File, SHA256: c.SHA256}); err != nil {
			errs = append(errs, fmt.Errorf("chart: %w", err))
		}
	}
	return errs
}

// validateConnectors reports each connector whose image is not listed with the
// connector's digest, or whose database, schema or settings are unknown.
func validateConnectors(connectors []Connector, digests map[string]string) []error {
	var errs []error
	for _, c := range connectors {
		digest, ok := digests[normalize(c.Image)]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("connector %s: image %s is not listed in the manifest", c.Name, c.Image))
		case digest != c.Digest:
			errs = append(errs, fmt.Errorf("connector %s: digest %s differs from the listed image digest %s", c.Name, c.Digest, digest))
		}
		switch c.Database {
		case DatabaseJDBC, DatabaseEnv:
		case "":
			if c.Schema != "" {
				errs = append(errs, fmt.Errorf("connector %s: schema %s set without a database", c.Name, c.Schema))
			}
		default:
			errs = append(errs, fmt.Errorf("connector %s: unknown database %q", c.Name, c.Database))
		}
		for _, setting := range c.Settings {
			if !slices.Contains([]string{SettingProxy, SettingTrust, SettingMail}, setting) {
				errs = append(errs, fmt.Errorf("connector %s: unknown setting %q", c.Name, setting))
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

// ImagesIn returns the image references used by the documents of an add-on file.
func ImagesIn(data []byte) ([]string, error) {
	return imagesIn(data, nil)
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
