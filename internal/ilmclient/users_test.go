package ilmclient

import (
	"context"
	"encoding/pem"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// adminSetup is a fake ILM with the manager's user and the administrator, and a client of the manager.
func adminSetup(t *testing.T) (*fakeILM, *Client, *testCA) {
	t.Helper()
	ca := newTestCA(t, "admin-ca")
	f := newFakeILM(t, ca)
	manager := ca.issue(t, "appliance-manager")
	email := "admin@example.com"
	f.users["uuid-manager"] = &fakeUser{uuid: "uuid-manager", username: "appliance-manager-1", fingerprint: Fingerprint(manager.Certificate[0])}
	f.users["uuid-admin"] = &fakeUser{
		uuid: "uuid-admin", username: "admin", fingerprint: Fingerprint(ca.issue(t, "admin").Certificate[0]), email: &email,
		customAttributes: []map[string]any{{
			"uuid": "attr-1", "name": "department", "label": "Department", "type": "custom", "contentType": "string",
			"version": "v2", "content": []any{map[string]any{"data": "PKI"}},
		}},
	}
	return f, New(f.edgeAddr(), testHost, manager, f.roots), ca
}

func TestSetUserCertificateKeepsTheUserDetails(t *testing.T) {
	f, c, ca := adminSetup(t)
	renewed := ca.issue(t, "admin").Certificate[0]

	changed, err := c.SetUserCertificate(context.Background(), "admin", renewed)
	if err != nil || !changed {
		t.Fatalf("SetUserCertificate = %v, %v; want the certificate changed", changed, err)
	}
	admin := f.users["uuid-admin"]
	if admin.fingerprint != Fingerprint(renewed) {
		t.Error("ILM does not hold the renewed certificate")
	}
	if admin.email == nil || *admin.email != "admin@example.com" {
		t.Errorf("email %v; want the user's email kept", admin.email)
	}
	want := []map[string]any{{"uuid": "attr-1", "name": "department", "contentType": "string", "version": "v2", "content": []any{map[string]any{"data": "PKI"}}}}
	if !reflect.DeepEqual(admin.customAttributes, want) {
		t.Errorf("custom attributes %v; want the user's kept, in the request's form", admin.customAttributes)
	}

	before := len(f.edgeRequests())
	changed, err = c.SetUserCertificate(context.Background(), "admin", renewed)
	if err != nil || changed {
		t.Fatalf("again: SetUserCertificate = %v, %v; want nothing to change", changed, err)
	}
	if slices.Contains(f.edgeRequests()[before:], "PUT /api/v1/users/uuid-admin") {
		t.Error("the user was updated with the certificate it holds")
	}
}

func TestSetUserCertificateFailures(t *testing.T) {
	f, c, ca := adminSetup(t)
	der := ca.issue(t, "admin").Certificate[0]
	if _, err := c.SetUserCertificate(context.Background(), "nobody", der); Reason(err) != ReasonNotFound {
		t.Errorf("unknown user: %v, want %s", err, ReasonNotFound)
	}
	f.failStatus, f.failBody = 401, "Invalid certificate header"
	_, err := c.SetUserCertificate(context.Background(), "admin", der)
	if Reason(err) != ReasonAuthFailed {
		t.Errorf("refused: %v, want %s", err, ReasonAuthFailed)
	}
}

func TestCertificateFingerprint(t *testing.T) {
	ca := newTestCA(t, "admin-ca")
	cert := ca.issue(t, "admin").Certificate[0]
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), ca.certPEM...)
	if got := CertificateFingerprint(chain); got != Fingerprint(cert) {
		t.Errorf("CertificateFingerprint = %q, want the leaf's", got)
	}
	if got := CertificateFingerprint([]byte("no PEM")); got != "" {
		t.Errorf("CertificateFingerprint of no certificate = %q, want empty", got)
	}
}

func TestSetUserCertificateKeepsASecretAttribute(t *testing.T) {
	for _, contentType := range []string{"secret", "credential"} {
		t.Run(contentType, func(t *testing.T) {
			f, c, ca := adminSetup(t)
			admin := f.users["uuid-admin"]
			admin.customAttributes = append(admin.customAttributes, map[string]any{
				"uuid": "attr-2", "name": "token", "contentType": contentType, "version": "v2", "content": []any{map[string]any{}},
			})
			before := admin.fingerprint

			_, err := c.SetUserCertificate(context.Background(), "admin", ca.issue(t, "admin").Certificate[0])
			if Reason(err) != ReasonUnsupportedAttribute || !strings.Contains(err.Error(), "token") {
				t.Fatalf("error %v, want %s naming the attribute", err, ReasonUnsupportedAttribute)
			}
			if admin.fingerprint != before || slices.Contains(f.edgeRequests(), "PUT /api/v1/users/uuid-admin") {
				t.Error("the user was updated, which would blank the attribute")
			}
		})
	}
}
