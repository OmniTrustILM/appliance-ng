// Package certs manages the appliance's certificate: the built-in one, a root CA generated on the device by
// cert-manager and a leaf certificate it issues for the host names and the address of the appliance, or one the
// administrator uploads, possibly for a signing request the appliance created.
package certs

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

// Names of the objects of the built-in chain.
const (
	// Namespace holds the leaf Certificate and its Secret.
	Namespace = kube.SystemNamespace
	// ClusterResourceNamespace is the cert-manager namespace, where the cert-manager add-on keeps the Secrets of
	// ClusterIssuers; the root CA's Secret has to be there.
	ClusterResourceNamespace = "cert-manager"
	SelfSignedIssuerName     = "appliance-selfsigned"
	RootCAName               = "appliance-root-ca"
	CAIssuerName             = "appliance-ca"
	CertificateName          = "appliance-tls"
	// SecretName is the TLS Secret, in Namespace, that holds the leaf certificate, its key and the root CA.
	SecretName = "appliance-tls"
	// FieldManager is the field manager of the manager's server-side applies.
	FieldManager = kube.ManagerFieldManager
)

const (
	rootDuration    = "87600h"
	leafDuration    = "8760h"
	leafRenewBefore = "720h"
	rootCommonName  = "ILM Appliance Root CA"
	// caKey is where cert-manager's CA issuer puts the issuing CA's certificate.
	caKey = "ca.crt"
)

// ErrNotIssued means the built-in certificate has not been issued yet.
var ErrNotIssued = errors.New("the built-in certificate has not been issued yet")

func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: kind}
}

func object(kind, namespace, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk(kind))
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func ecdsaP256() map[string]any {
	return map[string]any{"algorithm": "ECDSA", "size": int64(256)}
}

func clusterIssuerRef(name string) map[string]any {
	return map[string]any{"name": name, "kind": "ClusterIssuer", "group": "cert-manager.io"}
}

// EnsureBuiltin server-side-applies the chain: a self-signed ClusterIssuer, the root CA Certificate it issues
// (ECDSA P-256, 10 years), a CA ClusterIssuer of that root and the leaf Certificate (1 year, renewed 30 days
// before it expires) for hosts and ips. Applying unchanged objects changes nothing. It returns the name of the leaf
// Secret in Namespace, which cert-manager writes once it issued the certificate.
func EnsureBuiltin(ctx context.Context, c client.Client, hosts []string, ips []netip.Addr) (string, error) {
	if len(hosts) == 0 {
		return "", errors.New("the built-in certificate needs at least one host name")
	}
	dnsNames := make([]any, 0, len(hosts))
	for _, h := range hosts {
		dnsNames = append(dnsNames, h)
	}
	addresses := make([]any, 0, len(ips))
	for _, ip := range ips {
		addresses = append(addresses, ip.String())
	}
	leaf := map[string]any{
		"secretName":  SecretName,
		"duration":    leafDuration,
		"renewBefore": leafRenewBefore,
		"dnsNames":    dnsNames,
		"privateKey":  ecdsaP256(),
		"issuerRef":   clusterIssuerRef(CAIssuerName),
	}
	if len(addresses) > 0 {
		leaf["ipAddresses"] = addresses
	}
	rootKey := ecdsaP256()
	// Keeping the root key on renewal keeps the leaves it issued valid.
	rootKey["rotationPolicy"] = "Never"

	chain := []*unstructured.Unstructured{
		object("ClusterIssuer", "", SelfSignedIssuerName, map[string]any{"selfSigned": map[string]any{}}),
		object("Certificate", ClusterResourceNamespace, RootCAName, map[string]any{
			"isCA":       true,
			"commonName": rootCommonName,
			"secretName": RootCAName,
			"duration":   rootDuration,
			"privateKey": rootKey,
			"issuerRef":  clusterIssuerRef(SelfSignedIssuerName),
		}),
		object("ClusterIssuer", "", CAIssuerName, map[string]any{"ca": map[string]any{"secretName": RootCAName}}),
		object("Certificate", Namespace, CertificateName, leaf),
	}
	for _, u := range chain {
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return "", fmt.Errorf("apply %s %s: %w", u.GetKind(), client.ObjectKeyFromObject(u), err)
		}
	}
	return SecretName, nil
}

// Ready reports whether cert-manager reports the leaf Certificate Ready for its current generation.
func Ready(ctx context.Context, c client.Reader) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk("Certificate"))
	err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: CertificateName}, u)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read certificate %s/%s: %w", Namespace, CertificateName, err)
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		generation, _, _ := unstructured.NestedInt64(cond, "observedGeneration")
		return cond["status"] == "True" && generation == u.GetGeneration(), nil
	}
	return false, nil
}

// RootCA returns the PEM certificate of the built-in root CA, read from the leaf Secret. Only CERTIFICATE blocks are
// returned. It returns ErrNotIssued until cert-manager issued the leaf.
func RootCA(ctx context.Context, c client.Reader) ([]byte, error) {
	s := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: SecretName}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, ErrNotIssued
	case err != nil:
		return nil, fmt.Errorf("read secret %s/%s: %w", Namespace, SecretName, err)
	}
	var out bytes.Buffer
	for rest := s.Data[caKey]; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			_ = pem.Encode(&out, &pem.Block{Type: block.Type, Bytes: block.Bytes})
		}
	}
	if out.Len() == 0 {
		return nil, ErrNotIssued
	}
	return out.Bytes(), nil
}
