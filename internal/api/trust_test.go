package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	pathTrust        = "/appliance/api/v1/trust"
	pathTrustPreview = "/appliance/api/v1/trust/preview"
)

func (ca *testCA) fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

func trustList(t *testing.T, h *harness, s *session) []TrustedCertificate {
	t.Helper()
	rec := h.request(http.MethodGet, pathTrust, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var list TrustedCertificateList
	decode(t, rec, &list)
	return list.Items
}

func listedFingerprints(items []TrustedCertificate) []string {
	out := make([]string, len(items))
	for i, c := range items {
		out[i] = c.Sha256
	}
	return out
}

func trustSecretExists(t *testing.T) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: certs.TrustSecretName}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestTrustAddPreviewAndRemove(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	caA, caB := newTestCA(t), newTestCA(t)

	if items := trustList(t, h, s); items == nil || len(items) != 0 {
		t.Fatalf("trust %v, want an empty list", items)
	}

	rec := h.request(http.MethodPost, pathTrustPreview, TrustUpload{Pem: string(caA.pem)}, s)
	wantStatus(t, rec, http.StatusOK)
	var preview TrustedCertificateList
	decode(t, rec, &preview)
	if len(preview.Items) != 1 || preview.Items[0].New == nil || !*preview.Items[0].New || preview.Items[0].Subject != "CN=Test CA" {
		t.Fatalf("preview %+v, want the CA as new", preview.Items)
	}
	if getAppliance(t).Spec.Trust != nil || trustSecretExists(t) {
		t.Fatal("a preview changed the trust")
	}

	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(caA.pem)}, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Trust; got == nil || *got != (v1alpha1.SecretKeyRef{Name: certs.TrustSecretName, Key: certs.TrustKey}) {
		t.Fatalf("spec.trust %+v, want %s/%s", got, certs.TrustSecretName, certs.TrustKey)
	}
	if got := string(systemSecret(t, certs.TrustSecretName).Data[certs.TrustKey]); got != string(caA.pem) {
		t.Errorf("bundle %q, want the CA", got)
	}

	rec = h.request(http.MethodPost, pathTrustPreview, TrustUpload{Pem: string(caA.pem) + string(caB.pem)}, s)
	wantStatus(t, rec, http.StatusOK)
	decode(t, rec, &preview)
	if got := listedFingerprints(preview.Items); !slices.Equal(got, []string{caA.fingerprint(), caB.fingerprint()}) || *preview.Items[0].New || !*preview.Items[1].New {
		t.Fatalf("preview %+v, want A kept and B new", preview.Items)
	}
	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(caA.pem) + string(caB.pem)}, s), http.StatusAccepted)
	if got := listedFingerprints(trustList(t, h, s)); !slices.Equal(got, []string{caA.fingerprint(), caB.fingerprint()}) {
		t.Fatalf("trust %v, want A and B", got)
	}

	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+caA.fingerprint(), nil, s), http.StatusAccepted)
	if got := listedFingerprints(trustList(t, h, s)); !slices.Equal(got, []string{caB.fingerprint()}) {
		t.Fatalf("trust %v, want only B", got)
	}
	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+caA.fingerprint(), nil, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+caB.fingerprint(), nil, s), http.StatusAccepted)
	if getAppliance(t).Spec.Trust != nil || trustSecretExists(t) {
		t.Error("removing the last certificate kept the trust")
	}
	if items := trustList(t, h, s); len(items) != 0 {
		t.Errorf("trust %v, want none", items)
	}
	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+caB.fingerprint(), nil, s), http.StatusNotFound)

	var actions []string
	for _, e := range h.entries() {
		if strings.HasPrefix(e.Action, "trust-") {
			actions = append(actions, e.Action+" "+e.Result+" "+strings.Join(e.Paths, ","))
		}
	}
	want := []string{
		"trust-add success spec.trust", "trust-add success spec.trust",
		"trust-remove success spec.trust", "trust-remove failure ", "trust-remove success spec.trust", "trust-remove failure ",
	}
	if !slices.Equal(actions, want) {
		t.Errorf("audit %q, want %q", actions, want)
	}
}

func TestTrustRejects(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	ca := newTestCA(t)
	leaf, key := ca.leaf(t)

	for name, tc := range map[string]struct {
		body any
		want string
	}{
		"a leaf":     {TrustUpload{Pem: string(ca.pem) + string(leaf)}, "CN=appliance.example.com"},
		"a key":      {TrustUpload{Pem: string(key)}, "PRIVATE KEY"},
		"nothing":    {TrustUpload{Pem: ""}, "no certificate"},
		"no body":    {nil, "not valid"},
		"not JSON":   {"pem", "not valid"},
		"over 1 MiB": {TrustUpload{Pem: strings.Repeat(" ", 1<<20) + string(ca.pem)}, "not valid"},
	} {
		for _, path := range []string{pathTrustPreview, pathTrust} {
			rec := h.request(http.MethodPost, path, tc.body, s)
			wantStatus(t, rec, http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("%s to %s: %s, want a message with %q", name, path, rec.Body, tc.want)
			}
		}
	}
	if getAppliance(t).Spec.Trust != nil || trustSecretExists(t) {
		t.Error("a refused upload changed the trust")
	}
	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+ca.fingerprint(), nil, s), http.StatusNotFound)
}

func TestTrustAcceptsAnUploadLargerThanTheDefaultLimit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	ca := newTestCA(t)
	big := TrustUpload{Pem: "# exported CA bundle\n" + strings.Repeat("\n", 2*maxBody) + string(ca.pem)}
	wantStatus(t, h.request(http.MethodPost, pathTrust, big, s), http.StatusAccepted)
	if got := listedFingerprints(trustList(t, h, s)); !slices.Equal(got, []string{ca.fingerprint()}) {
		t.Errorf("trust %v, want the CA", got)
	}
}

func TestTrustBuildsOnTheReferencedBundle(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	imported, added := newTestCA(t), newTestCA(t)
	src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: "imported-cas"}, Data: map[string][]byte{"bundle.pem": imported.pem}}
	if err := testClient.Create(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	a.Spec.Trust = &v1alpha1.SecretKeyRef{Name: "imported-cas", Key: "bundle.pem"}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(added.pem)}, s), http.StatusAccepted)
	if got := listedFingerprints(trustList(t, h, s)); !slices.Equal(got, []string{imported.fingerprint(), added.fingerprint()}) {
		t.Errorf("trust %v, want the imported and the added CA", got)
	}
	if got := getAppliance(t).Spec.Trust; got.Name != certs.TrustSecretName {
		t.Errorf("spec.trust %+v, want the appliance's own Secret", got)
	}
}

func TestTrustNeedsTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	ca := newTestCA(t)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, pathTrust, nil},
		{http.MethodPost, pathTrust, TrustUpload{Pem: string(ca.pem)}},
		{http.MethodPost, pathTrustPreview, TrustUpload{Pem: string(ca.pem)}},
		{http.MethodDelete, pathTrust + "/" + ca.fingerprint(), nil},
	} {
		if rec := h.request(tc.method, tc.path, tc.body, s); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), codeNotSetUp) {
			t.Errorf("%s %s without the Appliance: %d %s, want 409 %s", tc.method, tc.path, rec.Code, rec.Body, codeNotSetUp)
		}
	}
}
