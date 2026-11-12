package release

import (
	"slices"
	"strings"
)

// CacheList returns the sorted, unique references to cache for arch, including
// installerRef when it is not empty.
func CacheList(m *Manifest, arch, installerRef string) []string {
	var refs []string
	for _, img := range m.Images {
		if len(img.Arches) == 0 || slices.Contains(img.Arches, arch) {
			refs = append(refs, img.Ref)
		}
	}
	if installerRef != "" {
		refs = append(refs, installerRef)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
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
