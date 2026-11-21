package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
)

const (
	pathPlatform      = "/appliance/api/v1/platform"
	testMailComponent = "email-notification-provider"
	privateComponent  = "external-authority-provider"
)

// installedAppliance creates an Appliance after setup with the platform at ILM version 2.19.0, an older version
// than the release ships, and the components.
func installedAppliance(t *testing.T, components ...string) {
	t.Helper()
	createAppliance(t, v1alpha1.SetupComplete)
	a := getAppliance(t)
	a.Spec.Platform = &v1alpha1.PlatformSpec{ILMVersion: "2.19.0", HostName: "ilm.example.com", Components: components}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

// useUploadedCertificate stores a certificate for hosts as the uploaded one and selects it.
func useUploadedCertificate(t *testing.T, hosts ...string) {
	t.Helper()
	createTLSSecret(t, certs.CustomSecretName, newTestCA(t).issue(t, newECKey(t).Public(), hosts, nil))
	a := getAppliance(t)
	a.Spec.TLS = v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceSecret, SecretRef: certs.CustomSecretName}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
}

func wantField(t *testing.T, body []byte, field, mention string) {
	t.Helper()
	got := string(body)
	if !strings.Contains(got, `"field":"`+field+`"`) || !strings.Contains(got, mention) {
		t.Errorf("answer %s, want field %s naming %s", got, field, mention)
	}
}

func TestGetPlatform(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t, "ejbca-ng-connector")

	rec := h.request(http.MethodGet, pathPlatform, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got PlatformState
	decode(t, rec, &got)
	want := PlatformState{IlmVersion: "2.19.0", HostName: "ilm.example.com", Components: []PlatformComponent{
		{Name: "common-credential-provider", Title: "Common Credential Provider"},
		{Name: "ejbca-ng-connector", Title: "EJBCA NG Connector", Chosen: true},
		{Name: privateComponent, Title: "External Authority Provider", Private: true, NeedsCredentials: true},
		{Name: testMailComponent, Title: "Email Notification Provider", NeedsMail: true},
	}}
	if got.IlmVersion != want.IlmVersion || got.HostName != want.HostName || !slices.Equal(got.Components, want.Components) {
		t.Errorf("platform\n got %+v\nwant %+v", got, want)
	}

	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	decode(t, h.request(http.MethodGet, pathPlatform, nil, s), &got)
	for _, c := range got.Components {
		if c.NeedsCredentials || c.NeedsMail {
			t.Errorf("%s still needs something: %+v", c.Name, c)
		}
	}
}

func TestPlatformPrivateNeedsCredentials(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	body := PlatformUpdate{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector", privateComponent}}

	rec := h.request(http.MethodPut, pathPlatform, body, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "components", privateComponent)
	if got := getAppliance(t).Spec.Platform.Components; len(got) != 0 {
		t.Errorf("components %v written by a refused request", got)
	}

	// Credentials for another registry do not help.
	wantStatus(t, h.request(http.MethodPut, pathRegistries+"/hub.example.com", RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, body, s), http.StatusBadRequest)

	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, body, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Platform.Components; !slices.Equal(got, body.Components) {
		t.Errorf("components %v, want %v", got, body.Components)
	}
	entries := h.entries()
	last := entries[len(entries)-1]
	if last.Action != string(AuditEntryActionPlatformSettings) || last.Result != string(Success) || !slices.Equal(last.Paths, []string{"spec.platform.components"}) {
		t.Errorf("audit entry %+v", last)
	}
}

func TestPlatformMailComponentNeedsMail(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	body := PlatformUpdate{HostName: "ilm.example.com", Components: []string{testMailComponent}}

	rec := h.request(http.MethodPut, pathPlatform, body, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "components", testMailComponent)

	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, body, s), http.StatusAccepted)
}

func TestPlatformKeepsTheILMVersion(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t, "ejbca-ng-connector")

	rec := h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}, IlmVersion: ptr("2.20.0")}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "ilmVersion", "cannot be changed")

	body := PlatformUpdate{HostName: "PKI.example.com", Components: []string{}, IlmVersion: ptr("2.19.0")}
	wantStatus(t, h.request(http.MethodPut, pathPlatform, body, s), http.StatusAccepted)
	got := getAppliance(t).Spec.Platform
	if got.ILMVersion != "2.19.0" || got.HostName != "pki.example.com" || len(got.Components) != 0 {
		t.Errorf("platform %+v", got)
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; !slices.Equal(last.Paths, []string{"spec.platform.components", "spec.platform.hostName"}) {
		t.Errorf("audit paths %v", last.Paths)
	}
}

func TestPlatformValidates(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	for _, tc := range []struct {
		name  string
		body  any
		field string
	}{
		{"IP host name", PlatformUpdate{HostName: "192.0.2.1", Components: []string{}}, "hostName"},
		{"no components", `{"hostName":"ilm.example.com"}`, "components"},
		{"unknown component", PlatformUpdate{HostName: "ilm.example.com", Components: []string{"nope"}}, "components"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodPut, pathPlatform, tc.body, s)
			wantStatus(t, rec, http.StatusBadRequest)
			wantField(t, rec.Body.Bytes(), tc.field, "")
		})
	}
	wantStatus(t, h.request(http.MethodPut, pathPlatform, "", s), http.StatusBadRequest)
}

func TestPlatformBeforeTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodGet, pathPlatform, nil, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}}, s), http.StatusConflict)
}

func TestGetPlatformBeforePart2(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	var got PlatformState
	rec := h.request(http.MethodGet, pathPlatform, nil, s)
	wantStatus(t, rec, http.StatusOK)
	decode(t, rec, &got)
	if got.IlmVersion != testRelease.Platform.ILMVersion || got.HostName != "" || len(got.Components) != len(testRelease.Connectors) {
		t.Errorf("platform before part 2 %+v", got)
	}
}

func TestWizardAcceptsComponentsWithWhatTheyNeed(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{privateComponent, testMailComponent}, Mail: validMail()}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Platform.Components; !slices.Equal(got, body.Components) {
		t.Errorf("components %v", got)
	}
}

func TestPlatformKeepsTheUploadedCertificateValid(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	useUploadedCertificate(t, "appliance.example.com", "ilm.example.com")

	rec := h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "pki.example.com", Components: []string{}}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "hostName", "The uploaded certificate does not cover pki.example.com")
	if got := getAppliance(t).Spec.Platform.HostName; got != "ilm.example.com" {
		t.Errorf("host name %q changed by a refused request", got)
	}

	// The ILM host name may fall back to the node host name, which the certificate covers.
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "appliance.example.com", Components: []string{}}, s), http.StatusAccepted)
}
