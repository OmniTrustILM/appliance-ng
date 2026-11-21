package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	talosx509 "github.com/siderolabs/crypto/x509"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
)

const (
	pathAccess      = "/appliance/api/v1/access"
	pathKubeconfig  = pathAccess + "/kubeconfig"
	pathTalosconfig = pathAccess + "/talosconfig"
	// talosKey is the client key in the talosconfig the fake node generates.
	talosKey = "talos-client-key-that-must-stay-inside"
	// longLivedToken is the token the fake token controller writes.
	longLivedToken = "long-lived-token-that-must-stay-inside"
)

// talosNode is a node that generates talosconfigs.
type talosNode struct {
	fakeTalos
	roles []string
	ttl   time.Duration
	err   error
}

func (n *talosNode) ClientConfig(_ context.Context, roles []string, ttl time.Duration) ([]byte, error) {
	n.roles, n.ttl = roles, ttl
	if n.err != nil {
		return nil, n.err
	}
	cfg := clientconfig.NewConfig("appliance", []string{"10.255.0.1"}, []byte("ca"),
		&talosx509.PEMEncodedCertificateAndKey{Crt: clientCertificate(time.Now().Add(ttl)), Key: []byte(talosKey)})
	return cfg.Bytes()
}

// clientCertificate is a self-signed PEM certificate that expires at notAfter, as in a generated talosconfig.
func clientCertificate(notAfter time.Time) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// withAccess resets the access namespace and creates a set-up Appliance with its node address.
func withAccess(t *testing.T) {
	t.Helper()
	resetAccess(t)
	createAppliance(t, v1alpha1.SetupComplete)
	setNodeAddress(t)
}

// resetAccess deletes the credentials earlier tests issued.
func resetAccess(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, obj := range []client.Object{&corev1.ServiceAccount{}, &corev1.Secret{}, &corev1.ConfigMap{}} {
		if err := testClient.DeleteAllOf(ctx, obj, client.InNamespace(access.Namespace)); err != nil {
			t.Fatal(err)
		}
	}
	if err := testClient.DeleteAllOf(ctx, &rbacv1.ClusterRoleBinding{}, client.MatchingLabels{access.LabelKind: access.KindKubeconfig}); err != nil {
		t.Fatal(err)
	}
}

func setNodeAddress(t *testing.T) {
	t.Helper()
	a := getAppliance(t)
	a.Status.NodeAddress = certNodeAddress
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// fillAccessTokens plays the token controller, which envtest does not run, until the test ends.
func fillAccessTokens(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			secrets := &corev1.SecretList{}
			_ = testClient.List(ctx, secrets, client.InNamespace(access.Namespace))
			for i := range secrets.Items {
				if s := &secrets.Items[i]; len(s.Data[corev1.ServiceAccountTokenKey]) == 0 {
					s.Data = map[string][]byte{corev1.ServiceAccountTokenKey: []byte(longLivedToken)}
					_ = testClient.Update(ctx, s)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func accessList(t *testing.T, h *harness, s *session) []AccessCredential {
	t.Helper()
	rec := h.request(http.MethodGet, pathAccess, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var list AccessList
	decode(t, rec, &list)
	return list.Items
}

func lastEntry(h *harness) audit.Entry {
	entries := h.entries()
	return entries[len(entries)-1]
}

func kubeconfigToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	cfg, err := clientcmd.Load(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	current := cfg.Contexts[cfg.CurrentContext]
	if server := cfg.Clusters[current.Cluster].Server; server != "https://"+certNodeAddress+":6443" {
		t.Errorf("server %s", server)
	}
	if string(cfg.Clusters[current.Cluster].CertificateAuthorityData) != string(testConfig.CAData) {
		t.Error("the kubeconfig does not carry the cluster CA")
	}
	return cfg.AuthInfos[current.AuthInfo].Token
}

func TestIssueAndRevokeKubeconfig(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)

	rec := h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(2)}, s)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, `attachment; filename="kubeconfig-kc-`) {
		t.Errorf("Content-Disposition %q", got)
	}
	if kubeconfigToken(t, rec) == "" {
		t.Error("the kubeconfig has no token")
	}
	items := accessList(t, h, s)
	if len(items) != 1 || items[0].Kind != Kubeconfig || items[0].Role != "view" || items[0].IssuedBy != testUser ||
		items[0].Expires == nil || items[0].Name == nil {
		t.Fatalf("listed %+v", items)
	}
	name := *items[0].Name
	e := lastEntry(h)
	if e.Action != string(AuditEntryActionKubeconfigIssue) || e.Result != string(Success) || e.User != testUser ||
		e.Details["role"] != "view" || e.Details["name"] != name || e.Details["expires"] == "" {
		t.Errorf("audit entry %+v", e)
	}

	wantStatus(t, h.request(http.MethodDelete, pathKubeconfig+"/"+name, nil, s), http.StatusNoContent)
	if e := lastEntry(h); e.Action != string(AuditEntryActionKubeconfigRevoke) || e.Details["name"] != name || e.Details["role"] != "view" {
		t.Errorf("audit entry %+v", e)
	}
	if items := accessList(t, h, s); len(items) != 0 {
		t.Errorf("listed after revoke: %+v", items)
	}
	wantStatus(t, h.request(http.MethodDelete, pathKubeconfig+"/"+name, nil, s), http.StatusNotFound)
}

func TestKubeconfigRevokedWhenTheAuditFails(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	h.srv.deps.Audit = failingAudit{}
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(2)}, s), http.StatusInternalServerError)
	if items := accessList(t, h, s); len(items) != 0 {
		t.Errorf("a kubeconfig without its audit entry is still issued: %+v", items)
	}
}

func TestIssueLongLivedKubeconfig(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	fillAccessTokens(t)

	rec := h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: ClusterAdmin, Name: ptr("ci")}, s)
	wantStatus(t, rec, http.StatusOK)
	if kubeconfigToken(t, rec) != longLivedToken {
		t.Error("the kubeconfig does not carry the Secret's token")
	}
	if e := lastEntry(h); e.Details["name"] != "named-ci" || e.Details["expires"] != "never" || e.Details["role"] != "cluster-admin" {
		t.Errorf("audit entry %+v", e)
	}
	rec = h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Name: ptr("ci")}, s)
	wantStatus(t, rec, http.StatusConflict)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View}, s), http.StatusBadRequest)
}

func TestHoursOutOfRange(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	h.withTalos(&talosNode{}, nil)
	for _, tc := range []struct {
		path  string
		body  any
		field string
	}{
		{pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(0)}, "hours"},
		{pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(25)}, "hours"},
		{pathKubeconfig, KubeconfigRequest{Role: "edit", Hours: ptr(1)}, "role"},
		{pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(1), Name: ptr("Bad Name")}, "name"},
		{pathTalosconfig, TalosconfigRequest{Role: OsReader, Hours: 0}, "hours"},
		{pathTalosconfig, TalosconfigRequest{Role: OsReader, Hours: 9}, "hours"},
		{pathTalosconfig, TalosconfigRequest{Role: "os:operator", Hours: 1}, "role"},
	} {
		rec := h.request(http.MethodPost, tc.path, tc.body, s)
		wantStatus(t, rec, http.StatusBadRequest)
		var e Error
		decode(t, rec, &e)
		if e.Field == nil || *e.Field != tc.field {
			t.Errorf("%s %+v: field %v, want %s", tc.path, tc.body, e.Field, tc.field)
		}
	}
	if items := accessList(t, h, s); len(items) != 0 {
		t.Errorf("issued on a refused request: %+v", items)
	}
}

func TestIssueTalosconfig(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	node := &talosNode{}
	h.withTalos(node, nil)

	rec := h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsReader, Hours: 8}, s)
	wantStatus(t, rec, http.StatusOK)
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="talosconfig.yaml"` {
		t.Errorf("Content-Disposition %q", got)
	}
	cfg, err := clientconfig.FromBytes(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if endpoints := cfg.Contexts[cfg.Context].Endpoints; len(endpoints) != 1 || endpoints[0] != certNodeAddress {
		t.Errorf("endpoints %v", endpoints)
	}
	if len(node.roles) != 1 || node.roles[0] != "os:reader" || node.ttl != 8*time.Hour {
		t.Errorf("roles %v, TTL %v", node.roles, node.ttl)
	}
	items := accessList(t, h, s)
	if len(items) != 1 || items[0].Kind != Talosconfig || items[0].Role != "os:reader" || items[0].Name != nil || items[0].Expires == nil {
		t.Fatalf("listed %+v", items)
	}
	if e := lastEntry(h); e.Action != string(AuditEntryActionTalosconfigIssue) || e.Details["role"] != "os:reader" || e.Details["expires"] == "" {
		t.Errorf("audit entry %+v", e)
	}
}

func TestCorruptTalosconfigRecordsLeaveKubeconfigsListed(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(1)}, s), http.StatusOK)
	cm := &corev1.ConfigMap{}
	cm.Namespace, cm.Name = access.Namespace, access.TalosconfigRecords
	cm.Data = map[string]string{"records": "corrupt-record-content"}
	if err := testClient.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	items := accessList(t, h, s)
	if len(items) != 1 || items[0].Kind != Kubeconfig {
		t.Fatalf("listed %+v, want the kubeconfig", items)
	}
	wantStatus(t, h.request(http.MethodDelete, pathKubeconfig+"/"+*items[0].Name, nil, s), http.StatusNoContent)
	if strings.Contains(h.logs.String(), "corrupt-record-content") {
		t.Error("the log quotes the records")
	}
}

func TestHugeHoursAreRefused(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	h.withTalos(&talosNode{}, nil)
	// 2^51+1 hours overflow time.Duration to exactly one hour.
	rec := h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsReader, Hours: 1<<51 + 1}, s)
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestTalosconfigWithoutTheNode(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	withAccess(t)
	h.withTalos(nil, errors.New("no talosconfig yet"))
	rec := h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsAdmin, Hours: 1}, s)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	h.withTalos(&talosNode{err: errors.New("rpc error: code = Unavailable")}, nil)
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsAdmin, Hours: 1}, s), http.StatusServiceUnavailable)
	h.srv.deps.Talos = nil
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsAdmin, Hours: 1}, s), http.StatusServiceUnavailable)
	if items := accessList(t, h, s); len(items) != 0 {
		t.Errorf("recorded without a talosconfig: %+v", items)
	}
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, nil, s), http.StatusBadRequest)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, nil, s), http.StatusBadRequest)
}

func TestAccessNeedsTheNodeAddress(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	h.withTalos(&talosNode{}, nil)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(1)}, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsAdmin, Hours: 1}, s), http.StatusConflict)
	createAppliance(t, v1alpha1.SetupPart2)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(1)}, s), http.StatusConflict)
}
