package manager

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/addons"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// certManagerStep is the add-on step after which cert-manager issues certificates.
const certManagerStep = "cert-manager"

// reconcileCertificate requests the built-in certificate once cert-manager runs and reports it in CertificateReady.
// It returns how soon to look again, zero when the add-on pass or a watched object will. A failure does not hold up
// the platform: the edge serves its internal certificate until the built-in one is issued.
func (r *ApplianceReconciler) reconcileCertificate(ctx context.Context, a *v1alpha1.Appliance, progress addons.Progress) time.Duration {
	if a.Spec.TLS.Source == v1alpha1.TLSSourceSecret {
		meta.RemoveStatusCondition(&a.Status.Conditions, v1alpha1.ConditionCertificateReady)
		return 0
	}
	notReady := func(reason, message string) {
		setCondition(a, v1alpha1.ConditionCertificateReady, metav1.ConditionFalse, reason, message)
	}
	if !stepPassed(r.Steps, progress, certManagerStep) {
		notReady(reasonWaitingForCertManager, "waiting for cert-manager to run")
		return 0
	}
	ip, found, err := r.nodeAddress(ctx, a)
	if err != nil {
		log.FromContext(ctx).Error(err, "find the node address for the built-in certificate")
		notReady(reasonFailed, messageCertificateFailed)
		return addonsRequeue
	}
	if !found {
		notReady(reasonWaitingForNodeAddress, "waiting for the node address")
		// Nodes are not watched, so nothing else would look again soon.
		return addonsRequeue
	}
	ready, err := r.ensureCertificate(ctx, a, ip)
	switch {
	case err != nil:
		log.FromContext(ctx).Error(err, "request the built-in certificate")
		notReady(reasonFailed, messageCertificateFailed)
		return addonsRequeue
	case !ready:
		notReady(reasonIssuing, "cert-manager is issuing the built-in certificate")
		return addonsRequeue
	}
	setCondition(a, v1alpha1.ConditionCertificateReady, metav1.ConditionTrue, reasonReady, "the built-in certificate is issued")
	return 0
}

func (r *ApplianceReconciler) ensureCertificate(ctx context.Context, a *v1alpha1.Appliance, ip netip.Addr) (bool, error) {
	if _, err := certs.EnsureBuiltin(ctx, r.Client, certificateHosts(a), []netip.Addr{ip}); err != nil {
		return false, err
	}
	return certs.Ready(ctx, r.Client)
}

// stepPassed reports whether the pass that reported progress got beyond the named step.
func stepPassed(steps []kube.Step, progress addons.Progress, name string) bool {
	if progress.Done {
		return true
	}
	at := func(step string) int {
		return slices.IndexFunc(steps, func(s kube.Step) bool { return s.Name == step })
	}
	target := at(name)
	return target >= 0 && at(progress.Step) > target
}

// certificateHosts returns the node host name and the ILM host name, which defaults to it.
func certificateHosts(a *v1alpha1.Appliance) []string {
	hosts := []string{a.Spec.Node.HostName}
	if p := a.Spec.Platform; p != nil && p.HostName != "" && p.HostName != a.Spec.Node.HostName {
		hosts = append(hosts, p.HostName)
	}
	return hosts
}

// nodeAddress returns the node address from the status, the spec, or with DHCP the node's internal address.
func (r *ApplianceReconciler) nodeAddress(ctx context.Context, a *v1alpha1.Appliance) (netip.Addr, bool, error) {
	for _, s := range []string{a.Status.NodeAddress, a.Spec.Node.Address} {
		if addr, ok := parseAddress(s); ok {
			return addr, true, nil
		}
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return netip.Addr{}, false, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		for _, address := range n.Status.Addresses {
			if address.Type != corev1.NodeInternalIP {
				continue
			}
			if addr, ok := parseAddress(address.Address); ok {
				return addr, true, nil
			}
		}
	}
	return netip.Addr{}, false, nil
}

// parseAddress accepts an address with or without a prefix length, and only a global unicast IPv4 address.
func parseAddress(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		prefix, perr := netip.ParsePrefix(s)
		if perr != nil {
			return netip.Addr{}, false
		}
		addr = prefix.Addr()
	}
	return addr, addr.Is4() && addr.IsGlobalUnicast()
}
