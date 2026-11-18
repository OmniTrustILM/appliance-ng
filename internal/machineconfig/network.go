package machineconfig

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/siderolabs/go-pointer"
	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/meta"
	"github.com/siderolabs/talos/pkg/machinery/config/types/network"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/nethelpers"
)

const (
	// HandoverService is the extension service that receives the hand-over file.
	HandoverService = "firstboot"
	// HandoverPath is where the hand-over file is mounted in the firstboot service.
	HandoverPath = "/var/lib/appliance/handover.json"
)

// checkNetwork checks that a static address comes with its link and a gateway in its network, and that the node
// address is the static address.
func checkNetwork(in Input, nodeIP netip.Addr) error {
	if !in.Address.IsValid() {
		return nil
	}

	switch {
	case !in.Address.Addr().Is4() || in.Address.Addr() != nodeIP:
		return fmt.Errorf("static address %s is not the node address %s", in.Address, nodeIP)
	case in.Link == "":
		return errors.New("static address needs the link that holds it")
	case !in.Gateway.Is4() || !in.Address.Masked().Contains(in.Gateway):
		return fmt.Errorf("gateway %s is not in the network of %s", in.Gateway, in.Address)
	}

	return nil
}

// noProxy returns the no-proxy list of in: what the cluster reaches on the node itself, through its pod and service
// networks and by its cluster names comes first, so that etcd, the kubelet and in-cluster pulls never go through the
// proxy, then the administrator's entries.
func noProxy(in Input) string {
	entries := []string{
		"localhost", "127.0.0.1", in.NodeIP, constants.DefaultIPv4PodCIDR, constants.DefaultIPv4ServiceCIDR,
		".svc", "." + constants.DefaultDNSDomain,
	}
	if in.Proxy.NoProxy != "" {
		entries = append(entries, in.Proxy.NoProxy)
	}
	return strings.Join(entries, ",")
}

// nodePatches returns the strategic merge patches for the node settings of in: host name, static address and
// default route, name servers, time servers, proxy and the hand-over file.
func nodePatches(in Input) ([]configpatcher.Patch, error) {
	var docs []config.Document

	if in.HostName != "" {
		doc := network.NewHostnameConfigV1Alpha1()
		doc.ConfigAuto = pointer.To(nethelpers.AutoHostnameKindOff)
		doc.ConfigHostname = in.HostName
		docs = append(docs, doc)
	}

	if in.Address.IsValid() {
		doc := network.NewLinkConfigV1Alpha1(in.Link)
		doc.LinkUp = pointer.To(true)
		doc.LinkAddresses = []network.AddressConfig{{AddressAddress: in.Address}}
		doc.LinkRoutes = []network.RouteConfig{{RouteGateway: meta.Addr{Addr: in.Gateway}}}
		docs = append(docs, doc)
	}

	if len(in.DNS) > 0 {
		doc := network.NewResolverConfigV1Alpha1()
		for _, addr := range in.DNS {
			doc.ResolverNameservers = append(doc.ResolverNameservers, network.NameserverConfig{Address: meta.Addr{Addr: addr}})
		}
		docs = append(docs, doc)
	}

	if len(in.NTP) > 0 {
		doc := network.NewTimeSyncConfigV1Alpha1()
		doc.TimeNTP = &network.NTPConfig{Servers: in.NTP}
		docs = append(docs, doc)
	}

	if in.Proxy != nil {
		doc := runtime.NewEnvironmentV1Alpha1()
		doc.EnvironmentVariables = map[string]string{}
		for name, value := range map[string]string{"HTTP_PROXY": in.Proxy.HTTP, "HTTPS_PROXY": in.Proxy.HTTPS, "NO_PROXY": noProxy(in)} {
			if value != "" {
				doc.EnvironmentVariables[name] = value
			}
		}
		if len(doc.EnvironmentVariables) > 0 {
			docs = append(docs, doc)
		}
	}

	if len(in.Handover) > 0 {
		doc := extensions.NewServicesConfigV1Alpha1()
		doc.ServiceName = HandoverService
		doc.ServiceConfigFiles = extensions.ConfigFileList{{ConfigFileContent: string(in.Handover), ConfigFileMountPath: HandoverPath}}
		docs = append(docs, doc)
	}

	patches := make([]configpatcher.Patch, 0, len(docs))

	for _, doc := range docs {
		data, err := encoder.NewEncoder(doc, encoder.WithComments(encoder.CommentsDisabled)).Encode()
		if err != nil {
			return nil, fmt.Errorf("encode %s patch: %w", doc.Kind(), err)
		}

		patch, err := configpatcher.LoadPatch(data)
		if err != nil {
			return nil, fmt.Errorf("load %s patch: %w", doc.Kind(), err)
		}

		patches = append(patches, patch)
	}

	return patches, nil
}
