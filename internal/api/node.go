package api

import (
	"context"
	"net/netip"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/nodeconfig"
)

// PutNode replaces the gateway of the first node, the name and time servers and the proxy of the Appliance; the
// manager applies them to the node. The node address and host name are fixed at setup.
func (s *Server) PutNode(ctx context.Context, req PutNodeRequestObject) (PutNodeResponseObject, error) {
	if req.Body == nil {
		return PutNode400JSONResponse{BadRequestJSONResponse: BadRequestJSONResponse{Code: codeInvalid, Message: "Enter the node settings."}}, nil
	}
	a, err := s.appliance(ctx)
	if apierrors.IsNotFound(err) {
		return PutNode409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	if err != nil {
		return nil, err
	}
	first := a.Spec.FirstNode()
	if first == nil {
		return PutNode409JSONResponse{ConflictJSONResponse: notSetUp()}, nil
	}
	in, ferr := parseNodeUpdate(*req.Body, *first)
	if ferr != nil {
		return PutNode400JSONResponse{BadRequestJSONResponse: badRequestField(ferr.field, ferr.message)}, nil
	}
	var paths []string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		a, err := s.appliance(ctx)
		if err != nil {
			return err
		}
		before := a.Spec.DeepCopy()
		in.applyTo(&a.Spec)
		if paths = nodePaths(before, &a.Spec); len(paths) == 0 {
			return nil
		}
		return s.deps.Client.Update(ctx, a, client.FieldOwner(FieldManager))
	})
	if err != nil {
		return nil, err
	}
	recordPaths(ctx, paths)
	s.deps.Log.Info("node settings saved", "paths", paths)
	return PutNode202Response{}, nil
}

// nodeInput is the validated node settings of a request.
type nodeInput struct {
	gateway  string
	dns, ntp []string
	proxy    *v1alpha1.ProxySpec
}

func (in nodeInput) applyTo(spec *v1alpha1.ApplianceSpec) {
	if first := spec.FirstNode(); first != nil {
		first.Gateway = in.gateway
	}
	spec.DNS.Servers, spec.NTP.Servers, spec.Proxy = in.dns, in.ntp, in.proxy
}

// parseNodeUpdate validates body for node, whose address is empty with DHCP.
func parseNodeUpdate(body NodeUpdate, node v1alpha1.ClusterNode) (nodeInput, *fieldError) {
	var in nodeInput
	if body.Address != nil {
		return in, &fieldError{"address", "The node address is fixed at setup and cannot be changed."}
	}
	if body.HostName != nil && nodeconfig.ASCIILower(strings.TrimSpace(*body.HostName)) != node.HostName {
		return in, &fieldError{"hostName", "The node host name is fixed after setup. " +
			"The public name changes through the ILM host name and the certificate."}
	}
	var bad string
	if in.gateway, bad = parseGateway(node.Address, deref(body.Gateway)); bad != "" {
		return in, &fieldError{"gateway", bad}
	}
	dns, bad := nodeconfig.ParseDNS(body.Dns)
	if bad != "" {
		return in, &fieldError{"dns", bad}
	}
	for _, addr := range dns {
		in.dns = append(in.dns, addr.String())
	}
	if in.ntp, bad = nodeconfig.ParseNTP(body.Ntp); bad != "" {
		return in, &fieldError{"ntp", bad}
	}
	if body.Proxy != nil {
		p, bad := nodeconfig.ParseProxy(deref(body.Proxy.Http), deref(body.Proxy.Https), deref(body.Proxy.NoProxy))
		if bad != "" {
			return in, &fieldError{"proxy", bad}
		}
		if p != nil {
			in.proxy = &v1alpha1.ProxySpec{HTTP: p.HTTP, HTTPS: p.HTTPS, NoProxy: p.NoProxy}
		}
	}
	return in, nil
}

// parseGateway validates the gateway of the static address address: required with one, refused without.
func parseGateway(address, gateway string) (string, string) {
	gateway = strings.TrimSpace(gateway)
	prefix, err := netip.ParsePrefix(address)
	if err != nil {
		if gateway != "" {
			return "", "A gateway needs a static address."
		}
		return "", ""
	}
	gw, bad := nodeconfig.ParseGateway(prefix, gateway)
	if bad != "" {
		return "", bad
	}
	return gw.String(), ""
}

// nodePaths lists the JSON paths of the node settings that changed from before to after.
func nodePaths(before, after *v1alpha1.ApplianceSpec) []string {
	var paths []string
	for _, f := range []struct {
		changed bool
		path    string
	}{
		{gatewayOf(before) != gatewayOf(after), "spec.cluster.nodes[0].gateway"},
		{!slices.Equal(before.DNS.Servers, after.DNS.Servers), "spec.dns.servers"},
		{!slices.Equal(before.NTP.Servers, after.NTP.Servers), "spec.ntp.servers"},
		{!equality.Semantic.DeepEqual(before.Proxy, after.Proxy), "spec.proxy"},
	} {
		if f.changed {
			paths = append(paths, f.path)
		}
	}
	return paths
}

// gatewayOf returns the gateway of the first node of spec, "" without one.
func gatewayOf(spec *v1alpha1.ApplianceSpec) string {
	if first := spec.FirstNode(); first != nil {
		return first.Gateway
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
