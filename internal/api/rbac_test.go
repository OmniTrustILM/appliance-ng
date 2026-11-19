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

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

// TestAPIRunsWithTheManagerRBAC serves every operation as the manager's ServiceAccount with the RBAC of its
// manifest.
func TestAPIRunsWithTheManagerRBAC(t *testing.T) {
	h := newHarnessAs(t, managerClient(t))
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Mail.Password = ptr("another-secret-value")
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Mail.Username, body.Mail.Password = nil, nil
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	for _, path := range []string{"/api/v1/status", "/api/v1/appliance", "/api/v1/catalogue", "/api/v1/audit"} {
		wantStatus(t, h.request(http.MethodGet, path, nil, s), http.StatusOK)
	}
	wantStatus(t, h.request(http.MethodGet, "/api/v1/ca.pem", nil, nil), http.StatusNotFound)
	createAdminSecret(t, newAdminCredential(t, pkcs1))
	wantStatus(t, h.request(http.MethodPost, "/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusOK)
	node := NodeUpdate{Dns: []string{"192.0.2.53"}, Ntp: []string{}}
	wantStatus(t, h.request(http.MethodPut, "/api/v1/node", node, s), http.StatusAccepted)
	h.withTalos(&fakeTalos{}, nil)
	wantStatus(t, h.request(http.MethodPost, "/api/v1/system/reboot", Confirmation{Confirm: true}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPost, "/api/v1/system/shutdown", Confirmation{Confirm: true}, s), http.StatusAccepted)

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
}

// managerClient returns a client that acts as the manager's ServiceAccount with the RBAC of addons/manager.yaml.
func managerClient(t *testing.T) client.Client {
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
		if obj.GroupVersionKind().Group != rbacv1.GroupName || !slices.Contains([]string{"", SystemNamespace, platform.Namespace}, obj.GetNamespace()) {
			continue
		}
		if err := testClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), obj) })
	}
	cfg := rest.CopyConfig(testConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + SystemNamespace + ":appliance-manager"}
	c, err := client.New(cfg, client.Options{Scheme: testClient.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
