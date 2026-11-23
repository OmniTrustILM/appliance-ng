package release

import (
	"fmt"
	"slices"
	"strings"
)

// CacheList returns the sorted, unique references to cache for arch, including
// the non-empty references of the images built with the appliance, such as the
// installer and the manager. Images of private connectors are left out: they
// need registry credentials at install time.
func CacheList(m *Manifest, arch string, built ...string) []string {
	private := map[string]bool{}
	for _, c := range m.Connectors {
		if c.Private {
			private[normalize(c.Image)] = true
		}
	}
	var refs []string
	for _, img := range m.Images {
		if private[normalize(img.Ref)] {
			continue
		}
		if len(img.Arches) == 0 || slices.Contains(img.Arches, arch) {
			refs = append(refs, img.Ref)
		}
	}
	for _, ref := range built {
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// PinnedImage is the reference, with its digest, of the image the manifest pins for name, which may be short, as
// the implicit images are named. A digest in name must be the pinned one.
func (m *Manifest) PinnedImage(name string) (string, error) {
	nameOnly, digest, hasDigest := strings.Cut(name, "@")
	want := normalize(nameOnly)
	for _, img := range m.Images {
		ref, _, _ := strings.Cut(img.Ref, "@")
		if normalize(ref) == want && (!hasDigest || digest == img.Digest) {
			return want + "@" + img.Digest, nil
		}
	}
	return "", fmt.Errorf("the release pins no image %s", name)
}

// normalize expands short image names the way a container runtime does:
// docker.io is the default registry, library the default Docker Hub
// namespace and latest the default tag.
func normalize(ref string) string {
	name, digest, hasDigest := strings.Cut(ref, "@")
	first, rest, hasSlash := strings.Cut(name, "/")
	if !hasSlash || !isRegistry(first) {
		if !hasSlash {
			name = "library/" + name
		}
		name = "docker.io/" + name
	} else if rest == "" {
		return ref
	}
	if !hasTag(name) && !hasDigest {
		name += ":latest"
	}
	if hasDigest {
		name += "@" + digest
	}
	return name
}

func isRegistry(component string) bool {
	return strings.ContainsAny(component, ".:") || component == "localhost"
}

func hasTag(name string) bool {
	return strings.Contains(name[strings.LastIndex(name, "/")+1:], ":")
}

// RegistryHost is the registry the connector's image is pulled from, in lowercase.
func (c Connector) RegistryHost() string {
	host, _, _ := strings.Cut(normalize(c.Image), "/")
	return strings.ToLower(host)
}
