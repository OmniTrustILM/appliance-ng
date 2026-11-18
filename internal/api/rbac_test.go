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
