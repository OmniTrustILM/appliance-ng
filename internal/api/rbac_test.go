package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/access"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// TestAPIRunsWithTheManagerRBAC serves every operation as the manager's ServiceAccount with the RBAC of its
// manifest.
func TestAPIRunsWithTheManagerRBAC(t *testing.T) {
	cfg := managerConfig(t)
	h := newHarnessWith(t, clientFor(t, cfg), cfg)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Mail.Password = ptr("another-secret-value")
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Mail.Username, body.Mail.Password = nil, nil
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	for _, path := range []string{"/appliance/api/v1/status", "/appliance/api/v1/appliance", "/appliance/api/v1/catalogue", "/appliance/api/v1/audit"} {
		wantStatus(t, h.request(http.MethodGet, path, nil, s), http.StatusOK)
	}
	wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/ca.pem", nil, nil), http.StatusNotFound)
	createAdminSecret(t, newAdminCredential(t, pkcs1))
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusOK)
	createAdminPasswordSecret(t)
	wantStatus(t, h.request(http.MethodPost, pathAdminPassword, nil, s), http.StatusOK)
	setPlatformReady(t)
	createKeycloakAdminSecret(t)
	h.srv.deps.KeycloakURL = newFakeKeycloak(t).url
	wantStatus(t, h.request(http.MethodPut, pathAdminPassword, nil, s), http.StatusOK)
	node := NodeUpdate{Dns: []string{"192.0.2.53"}, Ntp: []string{}}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", node, s), http.StatusAccepted)
	h.withTalos(&fakeTalos{}, nil)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/system/reboot", Confirmation{Confirm: true}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/system/shutdown", Confirmation{Confirm: true}, s), http.StatusAccepted)

	ca := newTestCA(t)
	key := newECKey(t)
	certPEM := ca.issue(t, key.Public(), []string{"appliance.example.com", "ilm.example.com"}, nil)
	upload := map[string][]byte{"certificate": certPEM, "key": pkcs8PEM(t, key), "chain": ca.pem}
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, upload, s), http.StatusAccepted)
	wantStatus(t, h.upload(http.MethodPut, pathCertificate, upload, s), http.StatusAccepted)
	csr := h.request(http.MethodPost, pathCSR, nil, s)
	wantStatus(t, csr, http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathCSR, nil, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodGet, pathCertificate, nil, s), http.StatusOK)
	signed := map[string][]byte{"certificate": ca.sign(t, h.request(http.MethodPost, pathCSR, nil, s).Body.Bytes()), "chain": ca.pem}
	wantStatus(t, h.upload(http.MethodPut, pathSigned, signed, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, pathCSR, nil, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodPut, pathBuiltin, nil, s), http.StatusAccepted)

	trust := TrustUpload{Pem: string(ca.pem)}
	wantStatus(t, h.request(http.MethodPost, pathTrustPreview, trust, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathTrust, trust, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(newTestCA(t).pem)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodGet, pathTrust, nil, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+ca.fingerprint(), nil, s), http.StatusAccepted)
	for _, c := range trustList(t, h, s) {
		wantStatus(t, h.request(http.MethodDelete, pathTrust+"/"+c.Sha256, nil, s), http.StatusAccepted)
	}

	credentials := RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, credentials, s), http.StatusAccepted)
	credentials.Password = ptr(registryPasswordNext)
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, credentials, s), http.StatusAccepted)
	for _, path := range []string{pathRegistries, pathTestRegistry} {
		wantStatus(t, h.request(http.MethodGet, path, nil, s), http.StatusOK)
	}

	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodGet, pathMail, nil, s), http.StatusOK)
	h.withSender(nil)
	wantStatus(t, h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s), http.StatusNoContent)
	platform := PlatformUpdate{HostName: "ilm.example.com", Components: []string{privateComponent, testMailComponent}}
	wantStatus(t, h.request(http.MethodPut, pathPlatform, platform, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodGet, pathPlatform, nil, s), http.StatusOK)
	platform.Components = []string{}
	wantStatus(t, h.request(http.MethodPut, pathPlatform, platform, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodDelete, pathMail, nil, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodDelete, pathTestRegistry, nil, s), http.StatusAccepted)

	accessAnswers(t, h, s)
}

// accessAnswers issues and revokes kubeconfigs, issues a talosconfig and issues, re-issues, replaces and confirms
// the recovery kit, as the manager's ServiceAccount.
func accessAnswers(t *testing.T, h *harness, s *session) {
	t.Helper()
	resetAccess(t)
	setNodeAddress(t)
	fillAccessTokens(t)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: View, Hours: ptr(1)}, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathKubeconfig, KubeconfigRequest{Role: ClusterAdmin, Name: ptr("ci")}, s), http.StatusOK)
	h.withTalos(&talosNode{}, nil)
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsAdmin, Hours: 1}, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathTalosconfig, TalosconfigRequest{Role: OsReader, Hours: 1}, s), http.StatusOK)
	items := accessList(t, h, s)
	if len(items) != 4 {
		t.Fatalf("listed %d credentials, want 4", len(items))
	}
	for _, c := range items {
		if c.Name != nil {
			wantStatus(t, h.request(http.MethodDelete, pathKubeconfig+"/"+*c.Name, nil, s), http.StatusNoContent)
		}
	}

	first := issueKit(t, h, s)
	wantStatus(t, h.request(http.MethodGet, pathRecoveryKit, nil, s), http.StatusOK)
	wantStatus(t, reissue(h, s, first, kitPassphrase, nextPassphrase), http.StatusOK)
	wantStatus(t, h.request(http.MethodPost, pathReplace, RecoveryKitReplace{Passphrase: ptr(kitPassphrase), Confirm: true}, s), http.StatusOK)
	wantStatus(t, confirmStored(t, h, s), http.StatusNoContent)
}

// TestManagerRBACCoversTheBackupContent checks, as the manager's ServiceAccount, what a backup needs: exec into the
// database primary, its Cluster, the Secrets it collects, a short-lived pod for an external database and the volume,
// claim and pod of an NFS target; and that exec stays within those two namespaces.
func TestManagerRBACCoversTheBackupContent(t *testing.T) {
	c := clientFor(t, managerConfig(t))
	allowed := func(verb, group, resource, ns, name string) bool {
		t.Helper()
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Verb: verb, Group: group, Resource: resource, Namespace: ns, Name: name},
		}}
		if err := c.Create(context.Background(), review); err != nil {
			t.Fatal(err)
		}
		return review.Status.Allowed
	}
	for _, tc := range []struct {
		verb, group, resource, ns, name string
		want                            bool
	}{
		{"create", "", "pods/exec", platform.Namespace, "", true},
		{"list", "", "pods", platform.Namespace, "", true},
		{"get", "postgresql.cnpg.io", "clusters", platform.Namespace, "ilm-db", true},
		{"list", "otilm.com", "connectors", platform.Namespace, "", true},
		{"get", "", "secrets", platform.Namespace, "ca-keypair", true},
		{"get", "", "secrets", certs.ClusterResourceNamespace, certs.RootCAName, true},
		{"create", "", "pods", SystemNamespace, "", true},
		{"get", "", "pods", SystemNamespace, "", true},
		{"delete", "", "pods", SystemNamespace, "", true},
		{"create", "", "pods/exec", SystemNamespace, "", true},
		{"create", "", "persistentvolumes", "", "", true},
		{"get", "", "persistentvolumes", "", "", true},
		{"delete", "", "persistentvolumes", "", "", true},
		{"list", "", "persistentvolumes", "", "", true},
		{"list", "", "persistentvolumeclaims", SystemNamespace, "", true},
		{"list", "", "persistentvolumeclaims", platform.Namespace, "", false},
		{"create", "", "persistentvolumeclaims", SystemNamespace, "", true},
		{"get", "", "persistentvolumeclaims", SystemNamespace, "", true},
		{"delete", "", "persistentvolumeclaims", SystemNamespace, "", true},
		// The CBOM store's claim is created once and never deleted.
		{"create", "", "persistentvolumeclaims", platform.Namespace, "", true},
		{"delete", "", "persistentvolumeclaims", platform.Namespace, "cbom-s3-data", false},
		{"delete", "apps", "deployments", platform.Namespace, "cbom-s3", true},
		{"delete", "apps", "deployments", platform.Namespace, "core", false},
		{"delete", "", "services", platform.Namespace, "cbom-repository-service", true},
		{"delete", "", "services", platform.Namespace, "core", false},
		{"delete", "", "secrets", platform.Namespace, "cbom-s3-credentials", false},
		{"update", "", "persistentvolumes", "", "", false},
		{"create", "", "pods", platform.Namespace, "", false},
		{"delete", "", "pods", platform.Namespace, "", false},
		{"create", "", "pods/exec", "kube-system", "", false},
		{"get", "", "pods", platform.Namespace, "", false},
		{"get", "postgresql.cnpg.io", "clusters", platform.Namespace, "other", false},
		{"get", "", "secrets", certs.ClusterResourceNamespace, "another", false},
		{"list", "", "secrets", certs.ClusterResourceNamespace, "", false},
	} {
		if got := allowed(tc.verb, tc.group, tc.resource, tc.ns, tc.name); got != tc.want {
			t.Errorf("%s %s/%s %s in %s: allowed = %v, want %v", tc.verb, tc.group, tc.resource, tc.name, tc.ns, got, tc.want)
		}
	}
}

func clientFor(t *testing.T, cfg *rest.Config) client.Client {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// managerConfig creates the RBAC of addons/manager.yaml and returns a configuration that acts as the manager's
// ServiceAccount.
func managerConfig(t *testing.T) *rest.Config {
	t.Helper()
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("..", "..", "addons", "manager.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		if obj.GroupVersionKind().Group != rbacv1.GroupName || !slices.Contains([]string{"", SystemNamespace, platform.Namespace, access.Namespace, certs.ClusterResourceNamespace}, obj.GetNamespace()) {
			continue
		}
		if err := testClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })
	}
	cfg := rest.CopyConfig(testConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + SystemNamespace + ":appliance-manager"}
	return cfg
}
