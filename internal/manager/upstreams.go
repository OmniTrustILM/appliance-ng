package manager

import (
	"context"
	"log/slog"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/release"
)

const (
	managerDeployment = "appliance-manager"
	managerContainer  = "manager"
)

// MirrorUpstreams returns the registries the registry mirror serves: the release's and the registry the manager's own
// image comes from, which a build may take from another registry than the release pins. When that image cannot be
// read, the release's registries alone are returned.
func MirrorUpstreams(ctx context.Context, reader client.Reader, rel *release.Manifest, log *slog.Logger) []string {
	hosts := rel.RegistryHosts()
	deploy := &appsv1.Deployment{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: SystemNamespace, Name: managerDeployment}, deploy); err != nil {
		log.WarnContext(ctx, "read the manager's image; mirroring the release's registries only", "error", err)
		return hosts
	}
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name != managerContainer {
			continue
		}
		if host := release.RegistryHost(c.Image); !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
			slices.Sort(hosts)
		}
	}
	return hosts
}
