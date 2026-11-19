package machineconfig

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
	"github.com/siderolabs/talos/pkg/machinery/config/types/runtime/extensions"

	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
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

// nodePatches returns the strategic merge patches for the node settings of in: host name, static address and
// default route, name servers, time servers, proxy and the hand-over file.
func nodePatches(in Input) ([]configpatcher.Patch, error) {
	var docs []config.Document
	docs = appendIdentityDocs(docs, in)
	docs = appendServerDocs(docs, in)
	docs = appendProxyDoc(docs, in)
	docs = appendHandoverDoc(docs, in)

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

// appendIdentityDocs appends the documents for the host name and the static address with its default route.
func appendIdentityDocs(docs []config.Document, in Input) []config.Document {
	if in.HostName != "" {
		docs = append(docs, nodeconfig.HostnameConfig(in.HostName))
	}
	if in.Address.IsValid() {
		docs = append(docs, nodeconfig.LinkConfig(in.Link, in.Address, in.Gateway))
	}
	return docs
}

// appendServerDocs appends the documents for the name servers and the time servers.
func appendServerDocs(docs []config.Document, in Input) []config.Document {
	return append(docs, nodeconfig.ResolverConfig(in.DNS), nodeconfig.TimeSyncConfig(in.NTP))
}

// appendProxyDoc appends the document for the proxy environment when there is a proxy.
func appendProxyDoc(docs []config.Document, in Input) []config.Document {
	if doc := nodeconfig.EnvironmentConfig(in.Proxy, in.NodeIP); doc != nil {
		docs = append(docs, doc)
	}
	return docs
}

// appendHandoverDoc appends the document for the hand-over file.
func appendHandoverDoc(docs []config.Document, in Input) []config.Document {
	if len(in.Handover) == 0 {
		return docs
	}
	doc := extensions.NewServicesConfigV1Alpha1()
	doc.ServiceName = HandoverService
	doc.ServiceConfigFiles = extensions.ConfigFileList{{ConfigFileContent: string(in.Handover), ConfigFileMountPath: HandoverPath}}
	return append(docs, doc)
}
