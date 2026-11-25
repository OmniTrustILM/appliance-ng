package manager

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const (
	// acmeNetworkPolicyName admits cert-manager to Core's HTTP port, where ILM serves the ACME directory; the
	// operator's policies deny ingress from other namespaces.
	acmeNetworkPolicyName = "appliance-acme-to-core"
	corePort              = 8080
	// eabVaultKey and eabVaultProfileKey of the preset's binding Secret name the vault profile that holds its key.
	eabVaultKey        = "vaultUuid"
	eabVaultProfileKey = "vaultProfileUuid"
)

// Conditions of the ILM profile that ILM, not the appliance, holds up.
const (
	messageWaitingForILMACME = "waiting for ILM to provide the ACME directory"
	messageNoVaultProfile    = "ILM needs a vault profile to hold the ACME account key"
	messageEABRefused        = "ILM refused to store the ACME account key; check the vault profile"
	messageProfileRefused    = "ILM refused the ACME profile; check the RA profile"
)

// acmeWait is why the ILM profile cannot request the certificate yet.
type acmeWait struct{ reason, message string }

var waitingForILM = &acmeWait{reasonWaitingForILM, messageWaitingForILMACME}

// ilmPreset reports whether a requests its certificate from the ACME profile of its ILM.
func ilmPreset(a *v1alpha1.Appliance) bool {
	return a.Spec.TLS.Source == v1alpha1.TLSSourceACME && a.Spec.TLS.ACME != nil && a.Spec.TLS.ACME.ILMProfile != nil
}

// ilmACMEDirectory is the directory of the ACME profile p at Core's in-cluster address: the ILM host name does not
// resolve in the cluster, and Core builds the URLs of the ACME flow from the request.
func ilmACMEDirectory(p *v1alpha1.ILMACMEProfile) string {
	return platform.CoreAPIURL + "/v1/protocols/acme/" + profileName(p) + "/directory"
}

func profileName(p *v1alpha1.ILMACMEProfile) string {
	if p.ProfileName == "" {
		return certs.ILMACMEProfileName
	}
	return p.ProfileName
}

// prepareILMACME gets ILM ready to issue the certificate once ILM answers: an EAB key stored in the chosen vault
// profile and in the binding's Secret, and the ACME profile, enabled, that issues only for the appliance's host names
// and node address ip. It returns the binding's key ID, or why it cannot yet. A slow or unreachable ILM is waited
// for, and never holds up the platform.
func (r *ApplianceReconciler) prepareILMACME(ctx context.Context, a *v1alpha1.Appliance, ip netip.Addr) (string, *acmeWait) {
	if !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionPlatformReady) ||
		!meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionILMSettingsApplied) {
		return "", waitingForILM
	}
	if err := applyACMENetworkPolicy(ctx, r.Client); err != nil {
		log.FromContext(ctx).Error(err, "apply the network policy of the ILM ACME profile")
		return "", &acmeWait{reasonFailed, messageACMEFailed}
	}
	ctx, cancel := context.WithTimeout(ctx, ilmPassTimeout)
	defer cancel()
	c, err := ilmClientOf(ctx, r.Client, r.APIReader, r.ILM, a)
	if err != nil {
		logV1Failure(ctx, err, "could not reach ILM for the ACME profile")
		return "", waitingForILM
	}
	p := a.Spec.TLS.ACME.ILMProfile
	keyID, wait := r.ensureEABKey(ctx, c, p)
	if wait != nil {
		return "", wait
	}
	profile := ilmclient.ACMEProfile{Name: profileName(p), RAProfileUUID: p.RAProfileUUID, EABSecretUUIDs: []string{keyID},
		Identifiers: []ilmclient.PreauthorizedIdentifier{{Type: "ip", Value: ip.String(), MatchType: "exact"}}}
	for _, host := range certs.Hosts(a) {
		profile.Identifiers = append(profile.Identifiers, ilmclient.PreauthorizedIdentifier{Type: "dns", Value: host, MatchType: "exact"})
	}
	if _, err := c.EnsureACMEProfile(ctx, profile); err != nil {
		return "", ilmFailure(ctx, err, "could not set up the ACME profile in ILM", messageProfileRefused)
	}
	return keyID, nil
}

// ensureEABKey returns the key ID of the binding the preset's Secret holds while ILM still holds its key in the vault
// profile of p. Otherwise it has ILM generate a key, stores it in the vault profile of p and writes both into the
// Secret. The key is never logged.
func (r *ApplianceReconciler) ensureEABKey(ctx context.Context, c *ilmclient.Client, p *v1alpha1.ILMACMEProfile) (string, *acmeWait) {
	s := &corev1.Secret{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: certs.Namespace, Name: certs.ACMEILMEABSecretName}, s)
	if client.IgnoreNotFound(err) != nil {
		log.FromContext(ctx).Error(err, "read the Secret of the ACME external account binding")
		return "", &acmeWait{reasonFailed, messageACMEFailed}
	}
	if id := string(s.Data[certs.ACMEEABKeyIDKey]); id != "" && len(s.Data[certs.ACMEEABHMACKey]) > 0 && storedIn(s, p) {
		exists, err := c.SecretExists(ctx, id)
		if err != nil {
			return "", ilmFailure(ctx, err, "could not find the ACME account key in ILM", messageEABRefused)
		}
		if exists {
			return id, nil
		}
		log.FromContext(ctx).Info("the ACME account key is gone from ILM; generating a new one")
	}
	vaults, err := c.ListVaultProfiles(ctx)
	if err != nil {
		return "", ilmFailure(ctx, err, "could not list the vault profiles in ILM", messageNoVaultProfile)
	}
	if !slices.ContainsFunc(vaults, func(v ilmclient.VaultProfile) bool {
		return v.UUID == p.VaultProfileUUID && v.VaultUUID == p.VaultUUID
	}) {
		return "", &acmeWait{reasonWaitingForILM, messageNoVaultProfile}
	}
	key, err := c.GenerateEABKey(ctx)
	if err != nil {
		return "", ilmFailure(ctx, err, "could not generate the ACME account key in ILM", messageEABRefused)
	}
	// ILM's secret names are unique, and a key of an earlier selection of the preset may still be there.
	name := certs.ACMEILMEABSecretName + "-" + strings.ToLower(rand.Text()[:8])
	id, err := c.CreateSecretKey(ctx, p.VaultUUID, p.VaultProfileUUID, name, key)
	if err != nil {
		return "", ilmFailure(ctx, err, "could not store the ACME account key in ILM", messageEABRefused)
	}
	if err := writeEABSecret(ctx, r.Client, s, key, id, p); err != nil {
		log.FromContext(ctx).Error(err, "store the ACME account key")
		return "", &acmeWait{reasonFailed, messageACMEFailed}
	}
	log.FromContext(ctx).Info("stored a new ACME account key in ILM", "secret", name)
	return id, nil
}

// storedIn tells whether the key of the Secret s is stored in the vault profile of p.
func storedIn(s *corev1.Secret, p *v1alpha1.ILMACMEProfile) bool {
	return string(s.Data[eabVaultKey]) == p.VaultUUID && string(s.Data[eabVaultProfileKey]) == p.VaultProfileUUID
}

// writeEABSecret writes the binding's HMAC key and key ID, and the vault profile of p that holds the key, into s, the
// Secret as read, creating it when it was not found.
func writeEABSecret(ctx context.Context, c client.Client, s *corev1.Secret, key, id string, p *v1alpha1.ILMACMEProfile) error {
	s.Data = map[string][]byte{certs.ACMEEABHMACKey: []byte(key), certs.ACMEEABKeyIDKey: []byte(id),
		eabVaultKey: []byte(p.VaultUUID), eabVaultProfileKey: []byte(p.VaultProfileUUID)}
	if s.ResourceVersion != "" {
		return c.Update(ctx, s, client.FieldOwner(kube.ManagerFieldManager))
	}
	s.ObjectMeta = metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.ACMEILMEABSecretName}
	s.Type = corev1.SecretTypeOpaque
	return c.Create(ctx, s, client.FieldOwner(kube.ManagerFieldManager))
}

// ilmFailure logs a failed ILM call by its fixed reason and tells what holds the certificate up: ILM's refusal,
// reported as refused, or else ILM, waited for.
func ilmFailure(ctx context.Context, err error, message, refused string) *acmeWait {
	logV1Failure(ctx, err, message)
	if ilmclient.IsRefusal(err) {
		return &acmeWait{reasonFailed, refused}
	}
	return waitingForILM
}

// acmeNetworkPolicy admits cert-manager's controller pods to Core's pods, as the operator labels them, on Core's HTTP
// port only.
func acmeNetworkPolicy() *networkingv1.NetworkPolicy {
	tcp, port := corev1.ProtocolTCP, intstr.FromInt32(corePort)
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: acmeNetworkPolicyName},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": "core", "app.kubernetes.io/instance": platform.Name,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						corev1.LabelMetadataName: certs.ClusterResourceNamespace,
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app.kubernetes.io/name": "cert-manager", "app.kubernetes.io/component": "controller",
					}},
				}},
			}},
		},
	}
}

func applyACMENetworkPolicy(ctx context.Context, c client.Client) error {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(acmeNetworkPolicy())
	if err != nil {
		return fmt.Errorf("encode the network policy: %w", err)
	}
	u := &unstructured.Unstructured{Object: content}
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(kube.ManagerFieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply network policy %s/%s: %w", platform.Namespace, acmeNetworkPolicyName, err)
	}
	return nil
}

func removeACMENetworkPolicy(ctx context.Context, c client.Client) error {
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: acmeNetworkPolicyName}}
	if err := c.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete network policy %s/%s: %w", platform.Namespace, acmeNetworkPolicyName, err)
	}
	return nil
}
