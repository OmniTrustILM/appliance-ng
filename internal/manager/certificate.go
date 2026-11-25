package manager

import (
	"context"
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

// reconcileCertificate requests the built-in certificate once cert-manager runs, for source acme also the ACME one,
// and reports the selected one in CertificateReady. It returns how soon to look again, zero when the add-on pass or a
// watched object will. A failure does not hold up the platform: the edge serves its internal certificate until the
// built-in one is issued.
func (r *ApplianceReconciler) reconcileCertificate(ctx context.Context, a *v1alpha1.Appliance, progress addons.Progress) time.Duration {
	if !ilmPreset(a) {
		if err := removeACMENetworkPolicy(ctx, r.Client); err != nil {
			log.FromContext(ctx).Error(err, "remove the network policy of the ILM ACME profile")
		}
	}
	if a.Spec.TLS.Source != v1alpha1.TLSSourceACME {
		r.removeACME(ctx)
	}
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
	ip, found, err := certs.NodeAddress(ctx, r.Client, a)
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
	if a.Spec.TLS.Source == v1alpha1.TLSSourceACME {
		if err != nil {
			log.FromContext(ctx).Error(err, "request the built-in certificate, the fallback of the ACME one")
		}
		return r.reconcileACME(ctx, a, ip)
	}
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

// reconcileACME requests the ACME certificate and reports it in CertificateReady. Until it is issued, the built-in
// certificate is served, and a failure holds up nothing else. For the ILM profile, ILM's directory is reached in the
// cluster, and the certificate also covers the node address ip.
func (r *ApplianceReconciler) reconcileACME(ctx context.Context, a *v1alpha1.Appliance, ip netip.Addr) time.Duration {
	notReady := func(reason, message string) time.Duration {
		setCondition(a, v1alpha1.ConditionCertificateReady, metav1.ConditionFalse, reason, message)
		return addonsRequeue
	}
	if a.Spec.TLS.ACME == nil {
		return notReady(reasonFailed, "the ACME settings are missing")
	}
	spec, directory, ips := *a.Spec.TLS.ACME, a.Spec.TLS.ACME.DirectoryURL, []netip.Addr(nil)
	if p := spec.ILMProfile; p != nil {
		keyID, wait := r.prepareILMACME(ctx, a, ip)
		if wait != nil {
			return notReady(wait.reason, wait.message)
		}
		spec.EABKeyID, spec.EABSecretRef = keyID, &corev1.LocalObjectReference{Name: certs.ACMEILMEABSecretName}
		directory, ips = ilmACMEDirectory(p), []netip.Addr{ip}
	}
	ready, problem, err := certs.EnsureACME(ctx, r.Client, spec, directory, certs.Hosts(a), ips)
	switch {
	case err != nil:
		log.FromContext(ctx).Error(err, "request the ACME certificate")
		return notReady(reasonFailed, messageACMEFailed)
	case problem != "":
		log.FromContext(ctx).Info("the ACME certificate is not issued", "problem", problem)
		return notReady(reasonFailed, problem)
	case !ready:
		return notReady(reasonIssuing, "cert-manager is requesting the ACME certificate")
	}
	if served, err := certs.ServedSecretName(ctx, r.APIReader, a); err != nil || served != certs.ACMESecretName {
		if err != nil {
			log.FromContext(ctx).Error(err, "check the ACME certificate before serving it")
		}
		return notReady(reasonIssuing, messageACMENotServed)
	}
	setCondition(a, v1alpha1.ConditionCertificateReady, metav1.ConditionTrue, reasonReady, "the ACME certificate is issued")
	return 0
}

func (r *ApplianceReconciler) ensureCertificate(ctx context.Context, a *v1alpha1.Appliance, ip netip.Addr) (bool, error) {
	if _, err := certs.EnsureBuiltin(ctx, r.Client, certs.Hosts(a), []netip.Addr{ip}); err != nil {
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

// removeACME deletes the ACME objects and Secrets, except while a restore runs: it writes the backup's Secrets, the
// ACME account key among them, before the backup's Appliance selects source acme.
func (r *ApplianceReconciler) removeACME(ctx context.Context) {
	restoring, err := restoreRunning(ctx, r.APIReader)
	if err != nil {
		log.FromContext(ctx).Error(err, "read the restore status before removing the ACME certificate")
		return
	}
	if restoring {
		return
	}
	if err := certs.RemoveACME(ctx, r.Client); err != nil {
		log.FromContext(ctx).Error(err, "remove the ACME certificate")
	}
}
