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
	private := privateImages(m)
	var refs []string
	for _, img := range m.Images {
		if !private[normalize(img.Ref)] && img.forArch(arch) {
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

// privateImages returns the normalized references of the private connectors' images.
func privateImages(m *Manifest) map[string]bool {
	private := map[string]bool{}
	for _, c := range m.Connectors {
		if c.Private {
			private[normalize(c.Image)] = true
		}
	}
	return private
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

// RegistryHosts returns the sorted, unique registries the images are pulled from, in lowercase, as a container
// runtime names them: docker.io for Docker Hub.
func (m *Manifest) RegistryHosts() []string {
	var hosts []string
	for _, img := range m.Images {
		hosts = append(hosts, registryHost(img.Ref))
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// ArchPins maps the digest each image reference pins to the digest of its image manifest for arch, for the images
// whose manifest for arch has another digest.
func (m *Manifest) ArchPins(arch string) map[string]string {
	pins := map[string]string{}
	for _, img := range m.Images {
		if d, ok := img.ArchDigests[arch]; ok && d != img.Digest {
			pins[img.Digest] = d
		}
	}
	return pins
}

// registryHost is the registry ref is pulled from, in lowercase.
func registryHost(ref string) string {
	host, _, _ := strings.Cut(Repository(imageName(ref)), "/")
	return strings.ToLower(host)
}

// Repository returns the repository of name, an image name without tag and digest, as a container runtime resolves
// it: a name without a registry host, or with index.docker.io, is on Docker Hub, docker.io, and a single-component
// Docker Hub name is in library.
func Repository(name string) string {
	host, path, ok := strings.Cut(name, "/")
	switch {
	case !ok:
		return dockerHub + "/library/" + name
	case !isRegistry(host):
		return dockerHub + "/" + name
	case host == dockerHub || host == "index.docker.io":
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
		return dockerHub + "/" + path
	}
	return name
}

const dockerHub = "docker.io"

// RegistryHost is the registry the connector's image is pulled from, in lowercase.
func (c Connector) RegistryHost() string {
	host, _, _ := strings.Cut(normalize(c.Image), "/")
	return strings.ToLower(host)
}
