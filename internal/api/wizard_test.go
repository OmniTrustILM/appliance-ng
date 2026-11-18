package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/console"
)

func ptr[T any](v T) *T { return &v }

func validMail() *Mail {
	return &Mail{Host: "smtp.example.com", Port: 587, Security: Starttls, From: "ilm@example.com", Username: ptr("mailer"), Password: ptr("smtp-secret-value")}
}

func TestWizardWritesThePlatform(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)

	rec := h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{
		HostName:   "ILM.Example.com",
		Components: []string{"ejbca-ng-connector", "common-credential-provider", "ejbca-ng-connector"},
	}, s)
	wantStatus(t, rec, http.StatusAccepted)

	a := getAppliance(t)
	want := &v1alpha1.PlatformSpec{ILMVersion: "2.20.0", HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector", "common-credential-provider"}}
	if p := a.Spec.Platform; p == nil || p.ILMVersion != want.ILMVersion || p.HostName != want.HostName || !slices.Equal(p.Components, want.Components) {
		t.Errorf("platform = %+v, want %+v", a.Spec.Platform, want)
	}
	if a.Spec.Mail != nil {
		t.Errorf("mail = %+v, want none", a.Spec.Mail)
	}
	if got := a.ManagedFields; !slices.ContainsFunc(got, func(f metav1.ManagedFieldsEntry) bool { return f.Manager == FieldManager }) {
		t.Errorf("managed fields %v lack manager %s", got, FieldManager)
	}
}

func TestWizardStoresMailCredentialsInASecret(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)

	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: validMail()}
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	a := getAppliance(t)
	want := v1alpha1.MailSpec{Host: "smtp.example.com", Port: 587, Security: "starttls", From: "ilm@example.com", CredentialsRef: MailSecretName}
	if a.Spec.Mail == nil || *a.Spec.Mail != want {
		t.Fatalf("mail = %+v, want %+v", a.Spec.Mail, want)
	}
	if got := mailSecret(t); string(got.Data[MailUsernameKey]) != "mailer" || string(got.Data[MailPasswordKey]) != "smtp-secret-value" {
		t.Errorf("mail Secret holds %v", got.Data)
	}

	// Re-entering part 2 without the password keeps the stored one for the same server and user name.
	body.Mail.Password = nil
	body.Mail.From = "alerts@example.com"
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if got := mailSecret(t); string(got.Data[MailUsernameKey]) != "mailer" || string(got.Data[MailPasswordKey]) != "smtp-secret-value" {
		t.Errorf("mail Secret holds %v after re-entry", got.Data)
	}

	// The stored password is never handed to another server or user name.
	for name, edit := range map[string]func(*Mail){
		"user name": func(m *Mail) { m.Username = ptr("mailer2") },
		"host":      func(m *Mail) { m.Host = "smtp.elsewhere.example" },
		"port":      func(m *Mail) { m.Port = 465 },
		"security":  func(m *Mail) { m.Security = None },
	} {
		changed := *body.Mail
		edit(&changed)
		rec := h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{HostName: body.HostName, Components: body.Components, Mail: &changed}, s)
		wantStatus(t, rec, http.StatusBadRequest)
		var got BadRequest
		if decode(t, rec, &got); got.Field == nil || *got.Field != "mail.password" {
			t.Errorf("%s changed: error %+v, want one for mail.password", name, got)
		}
	}
	if got := getAppliance(t).Spec.Mail; got == nil || got.Host != "smtp.example.com" {
		t.Errorf("mail = %+v after refused changes, want the first server", got)
	}

	// With the password, the user name may change.
	body.Mail.Username, body.Mail.Password = ptr("mailer2"), ptr("other-secret-value")
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if got := mailSecret(t); string(got.Data[MailUsernameKey]) != "mailer2" || string(got.Data[MailPasswordKey]) != "other-secret-value" {
		t.Errorf("mail Secret holds %v after the new user name", got.Data)
	}

	// Without a user name the credentials go.
	body.Mail.Username, body.Mail.Password = nil, nil
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Mail; got == nil || got.CredentialsRef != "" {
		t.Errorf("mail = %+v, want no credentials", got)
	}
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: MailSecretName}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("mail Secret still there: %v", err)
	}

	// Without mail the mail server goes.
	body.Mail = nil
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", body, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Mail; got != nil {
		t.Errorf("mail = %+v, want none", got)
	}
}

func mailSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: MailSecretName}, secret); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestWizardValidates(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupPart2)
	mail := func(change func(*Mail)) *Mail {
		m := validMail()
		change(m)
		return m
	}
	for _, tc := range []struct {
		name  string
		body  PlatformSettings
		field string
	}{
		{"empty host name", PlatformSettings{Components: []string{}}, "hostName"},
		{"single label", PlatformSettings{HostName: "ilm", Components: []string{}}, "hostName"},
		{"IP address", PlatformSettings{HostName: "192.0.2.1", Components: []string{}}, "hostName"},
		{"bad character", PlatformSettings{HostName: "ilm_1.example.com", Components: []string{}}, "hostName"},
		{"unknown component", PlatformSettings{HostName: "ilm.example.com", Components: []string{"nope"}}, "components"},
		{"private component", PlatformSettings{HostName: "ilm.example.com", Components: []string{"external-authority-provider"}}, "components"},
		{"mail host", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.Host = "bad host" })}, "mail.host"},
		{"mail port low", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.Port = 0 })}, "mail.port"},
		{"mail port high", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.Port = 65536 })}, "mail.port"},
		{"mail security", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.Security = "ssl" })}, "mail.security"},
		{"mail from", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.From = "ILM <ilm@example.com>" })}, "mail.from"},
		{"password without user", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail(func(m *Mail) { m.Username = nil })}, "mail.username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodPut, "/api/v1/wizard/platform", tc.body, s)
			wantStatus(t, rec, http.StatusBadRequest)
			var got Error
			decode(t, rec, &got)
			if got.Field == nil || *got.Field != tc.field {
				t.Errorf("field = %v, want %s", got.Field, tc.field)
			}
		})
	}
	wantStatus(t, h.request(http.MethodPut, "/api/v1/wizard/platform", `{"hostName":"ilm.example.com"}`, s), http.StatusBadRequest)
	if got := getAppliance(t).Spec.Platform; got != nil {
		t.Errorf("platform written by a refused request: %+v", got)
	}
}

func TestWizardClosedAfterComplete(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)

	rec := h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}}, s)
	wantStatus(t, rec, http.StatusConflict)
	if got := getAppliance(t).Spec.Platform; got != nil {
		t.Errorf("platform written after setup completed: %+v", got)
	}

	routes := console.Routes(h.handler)
	for _, path := range []string{"/setup", "/setup/", "/setup/node"} {
		res := httptest.NewRecorder()
		routes.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusFound || res.Header().Get("Location") != "/signin" {
			t.Errorf("GET %s: %d to %q, want 302 to /signin", path, res.Code, res.Header().Get("Location"))
		}
	}
}

func TestWizardBeforeTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	rec := h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}}, s)
	wantStatus(t, rec, http.StatusConflict)
}

func TestCatalogue(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	rec := h.request(http.MethodGet, "/api/v1/catalogue", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got Catalogue
	decode(t, rec, &got)
	want := []CatalogueItem{
		{Name: "common-credential-provider", Title: "Common Credential Provider"},
		{Name: "ejbca-ng-connector", Title: "EJBCA NG Connector"},
		{Name: "external-authority-provider", Title: "External Authority Provider", Private: true},
	}
	if !slices.Equal(got.Items, want) {
		t.Errorf("catalogue = %+v, want %+v", got.Items, want)
	}
}
