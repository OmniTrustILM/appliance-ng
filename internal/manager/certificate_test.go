package manager

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

func certManagerObject(kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: kind})
	return u
}

// cleanCertificates removes the built-in chain when the test ends; envtest runs no garbage collector.
func cleanCertificates(t *testing.T) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, ns := range []string{certs.Namespace, certs.ClusterResourceNamespace} {
			_ = testClient.DeleteAllOf(ctx, certManagerObject("Certificate"), client.InNamespace(ns))
		}
		_ = testClient.DeleteAllOf(ctx, certManagerObject("ClusterIssuer"))
	})
}

func leafCertificate(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := certManagerObject("Certificate")
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, u); err != nil {
		t.Fatalf("leaf certificate: %v", err)
	}
	return u
}

// issue does what cert-manager does once it issued the leaf: it writes the Secret and reports the Certificate Ready.
func issue(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: certs.Namespace, Name: certs.SecretName},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: []byte("leaf"), corev1.TLSPrivateKeyKey: []byte("key"), "ca.crt": []byte("root"),
		},
	}
	if err := testClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	leaf := leafCertificate(t)
	conditions := []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Ready", "message": "issued",
		"observedGeneration": leaf.GetGeneration(), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := unstructured.SetNestedSlice(leaf.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Status().Update(ctx, leaf); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileIssuesTheBuiltinCertificate(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		Platform: &v1alpha1.PlatformSpec{HostName: "pki.example.com"},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})

	if res := runReconcile(t, r); res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v while the certificate is issued", res.RequeueAfter, addonsRequeue)
	}
	leaf := leafCertificate(t)
	if dns, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "dnsNames"); !reflect.DeepEqual(dns, []string{"appliance.example.com", "pki.example.com"}) {
		t.Errorf("dnsNames %v, want the node and the ILM host name", dns)
	}
	if ips, _, _ := unstructured.NestedStringSlice(leaf.Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"192.0.2.10"}) {
		t.Errorf("ipAddresses %v, want the node address", ips)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonIssuing {
		t.Errorf("CertificateReady = %+v, want False while issuing", c)
	}
	if tls := getPlatform(t).Spec.Edge.TLS; tls.Source != "internal" {
		t.Errorf("edge TLS = %+v, want internal until the certificate is issued", tls)
	}

	issue(t)
	runReconcile(t, r)
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionTrue || c.Reason != reasonReady {
		t.Errorf("CertificateReady = %+v, want True", c)
	}
	copied := secret(t, TLSSecretName)
	if copied.Type != corev1.SecretTypeTLS || len(copied.Data) != 2 || string(copied.Data[corev1.TLSCertKey]) != "leaf" {
		t.Errorf("TLS copy = %s with keys %v, want the issued pair only", copied.Type, slices.Collect(maps.Keys(copied.Data)))
	}
	if tls := getPlatform(t).Spec.Edge.TLS; tls.Source != "secret" || tls.SecretRef == nil || *tls.SecretRef != TLSSecretName {
		t.Errorf("edge TLS = %+v, want the copied built-in Secret", tls)
	}
}

func TestReconcileCertificateWaitsForCertManager(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"}})
	r, _ := newReconciler(testClient, &fakeCluster{unready: "cert-manager"})
	runReconcile(t, r)

	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForCertManager {
		t.Errorf("CertificateReady = %+v, want False waiting for cert-manager", c)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, certManagerObject("Certificate")); err == nil {
		t.Error("the certificate was requested before cert-manager is ready")
	}
}

func TestReconcileCertificateWaitsForTheNodeAddress(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "appliance.example.com"}})
	r, _ := newReconciler(testClient, &fakeCluster{})
	if res := runReconcile(t, r); res.RequeueAfter != addonsRequeue {
		t.Errorf("requeue after %v, want %v: Nodes are not watched, so the address is looked for again", res.RequeueAfter, addonsRequeue)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionCertificateReady); c.Status != metav1.ConditionFalse || c.Reason != reasonWaitingForNodeAddress {
		t.Errorf("CertificateReady = %+v, want False waiting for the node address", c)
	}

	// With DHCP the address comes from the node.
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "talos-node"}}
	if err := testClient.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, node) })
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: "talos-node"},
		{Type: corev1.NodeInternalIP, Address: "169.254.1.1"},
		{Type: corev1.NodeInternalIP, Address: "198.51.100.7"},
	}
	if err := testClient.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if ips, _, _ := unstructured.NestedStringSlice(leafCertificate(t).Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"198.51.100.7"}) {
		t.Errorf("ipAddresses %v, want the node's usable internal address", ips)
	}

	// The status address, once reported, wins.
	a := getAppliance(t)
	a.Status.NodeAddress = "203.0.113.5"
	if err := testClient.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, r)
	if ips, _, _ := unstructured.NestedStringSlice(leafCertificate(t).Object, "spec", "ipAddresses"); !reflect.DeepEqual(ips, []string{"203.0.113.5"}) {
		t.Errorf("ipAddresses %v, want the status address", ips)
	}
}

func TestReconcileSecretSourceHasNoBuiltinCertificate(t *testing.T) {
	cleanCertificates(t)
	createAppliance(t, v1alpha1.ApplianceSpec{
		Node: v1alpha1.NodeSpec{HostName: "appliance.example.com", Address: "192.0.2.10/24"},
		TLS:  v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: "ilm-tls"},
	})
	r, _ := newReconciler(testClient, &fakeCluster{})
	runReconcile(t, r)
	if c := meta.FindStatusCondition(getAppliance(t).Status.Conditions, v1alpha1.ConditionCertificateReady); c != nil {
		t.Errorf("CertificateReady = %+v, want none for an uploaded certificate", c)
	}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: certs.Namespace, Name: certs.CertificateName}, certManagerObject("Certificate")); err == nil {
		t.Error("the built-in certificate was requested for an uploaded one")
	}
}

const marker = "marker-3e7c1"

func TestConditionsCarryNoErrorText(t *testing.T) {
	createAppliance(t, v1alpha1.ApplianceSpec{
		TLS:      v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: marker},
		Platform: platformSpec(),
	})
	cluster := &fakeCluster{applyErr: errors.New("apply failed: " + marker)}
	r, _ := newReconciler(testClient, cluster)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: v1alpha1.ApplianceName}}

	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("err = %v, want the add-on error for the log", err)
	}
	if c := condition(t, getAppliance(t), v1alpha1.ConditionAddonsReady); c.Reason != reasonFailed || strings.Contains(c.Message, marker) {
		t.Errorf("AddonsReady = %+v, want a fixed reason and message", c)
	}

	cluster.applyErr = nil
	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), marker) {
		t.Fatalf("err = %v, want the platform error for the log", err)
	}
	c := condition(t, getAppliance(t), v1alpha1.ConditionPlatformReady)
	if c.Reason != reasonFailed || strings.Contains(c.Message, marker) {
		t.Errorf("PlatformReady = %+v, want a fixed reason and message", c)
	}
	for _, c := range getAppliance(t).Status.Conditions {
		if strings.Contains(c.Message, marker) {
			t.Errorf("condition %s carries the error text: %q", c.Type, c.Message)
		}
	}
}

func TestCertificateHosts(t *testing.T) {
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Node: v1alpha1.NodeSpec{HostName: "ilm.example.com"}}}
	if got := certificateHosts(a); !reflect.DeepEqual(got, []string{"ilm.example.com"}) {
		t.Errorf("hosts %v without a platform", got)
	}
	a.Spec.Platform = &v1alpha1.PlatformSpec{HostName: "ilm.example.com"}
	if got := certificateHosts(a); !reflect.DeepEqual(got, []string{"ilm.example.com"}) {
		t.Errorf("hosts %v, want the shared name once", got)
	}
}

func TestParseAddress(t *testing.T) {
	for in, want := range map[string]string{"192.0.2.1/24": "192.0.2.1", "192.0.2.2": "192.0.2.2", "bogus": "", "": "", "fe80::1": "", "169.254.0.9/16": ""} {
		got, ok := parseAddress(in)
		if (want == "") == ok || (ok && got.String() != want) {
			t.Errorf("parseAddress(%q) = %v, %v; want %q", in, got, ok, want)
		}
	}
}

func TestCertificateRunsWithTheManagerRBAC(t *testing.T) {
	cleanCertificates(t)
	ctx := context.Background()
	c := managerClient(t)
	if _, err := certs.EnsureBuiltin(ctx, c, []string{"appliance.example.com"}, nil); err != nil {
		t.Fatalf("request the certificate as the manager: %v", err)
	}
	if _, err := certs.Ready(ctx, c); err != nil {
		t.Errorf("read the certificate as the manager: %v", err)
	}
	if _, err := certs.RootCA(ctx, c); !errors.Is(err, certs.ErrNotIssued) {
		t.Errorf("read the root CA as the manager: %v", err)
	}
}
