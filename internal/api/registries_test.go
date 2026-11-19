package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

const (
	testRegistryHost     = "registry.example.com:5000"
	pathRegistries       = "/api/v1/registries"
	pathTestRegistry     = pathRegistries + "/" + testRegistryHost
	registryPassword     = "registry-Pa55word-that-must-stay-inside"
	registryPasswordNext = "registry-Pa55word-the-second"
)

func registrySecretExists(t *testing.T, name string) bool {
	t.Helper()
	err := testClient.Get(context.Background(), client.ObjectKey{Namespace: SystemNamespace, Name: name}, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestRegistrySecretName(t *testing.T) {
	// The first 12 hex digits of the SHA-256 of the host.
	if got, want := registrySecretName("registry.example.com:5000"), "appliance-registry-"; !strings.HasPrefix(got, want) || len(got) != len(want)+12 {
		t.Errorf("registrySecretName = %q", got)
	}
	if registrySecretName("a.example.com") == registrySecretName("b.example.com") {
		t.Error("two hosts share a Secret")
	}
}

func TestRegistryPasswordWriteOnly(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	name := registrySecretName(testRegistryHost)

	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Registries; !slices.Equal(got, []v1alpha1.RegistrySpec{{Host: testRegistryHost, CredentialsRef: name}}) {
		t.Fatalf("registries %+v, want %s with %s", got, testRegistryHost, name)
	}
	stored := systemSecret(t, name)
	if string(stored.Data[MailUsernameKey]) != "robot" || string(stored.Data[MailPasswordKey]) != registryPassword {
		t.Fatal("the credentials Secret does not hold the credentials")
	}

	var answers strings.Builder
	rec := h.request(http.MethodGet, pathTestRegistry, nil, s)
	wantStatus(t, rec, http.StatusOK)
	answers.Write(rec.Body.Bytes())
	var one RegistryCredentials
	decode(t, rec, &one)
	if one != (RegistryCredentials{Host: testRegistryHost, Username: "robot", PasswordSet: true}) {
		t.Errorf("registry %+v, want the user name and the password as set", one)
	}
	rec = h.request(http.MethodGet, pathRegistries, nil, s)
	wantStatus(t, rec, http.StatusOK)
	answers.Write(rec.Body.Bytes())
	var list RegistryList
	decode(t, rec, &list)
	if !slices.Equal(list.Items, []RegistryCredentials{one}) {
		t.Errorf("registries %+v, want %+v", list.Items, one)
	}
	rec = h.request(http.MethodGet, "/api/v1/appliance", nil, s)
	wantStatus(t, rec, http.StatusOK)
	answers.Write(rec.Body.Bytes())
	var settings ApplianceSettings
	decode(t, rec, &settings)
	if !slices.Equal(settings.Registries, []Registry{{Host: testRegistryHost, CredentialsSet: true}}) {
		t.Errorf("appliance registries %+v", settings.Registries)
	}

	// Without a password the stored one is kept, but only for the same user name.
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot"}, s), http.StatusAccepted)
	if got := string(systemSecret(t, name).Data[MailPasswordKey]); got != registryPassword {
		t.Error("the stored password was not kept")
	}
	rec = h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "other"}, s)
	wantStatus(t, rec, http.StatusBadRequest)
	answers.Write(rec.Body.Bytes())
	if !strings.Contains(rec.Body.String(), `"field":"password"`) {
		t.Errorf("answer %s, want field password", rec.Body)
	}
	if got := string(systemSecret(t, name).Data[MailUsernameKey]); got != "robot" {
		t.Errorf("user name %q after a refused change", got)
	}
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "other", Password: ptr(registryPasswordNext)}, s), http.StatusAccepted)
	if got := systemSecret(t, name); string(got.Data[MailUsernameKey]) != "other" || string(got.Data[MailPasswordKey]) != registryPasswordNext {
		t.Error("the new credentials were not stored")
	}

	for _, secret := range []string{registryPassword, registryPasswordNext} {
		if strings.Contains(answers.String(), secret) || strings.Contains(readDir(t, h.auditFS), secret) || strings.Contains(h.logs.String(), secret) {
			t.Error("a registry password left the manager")
		}
	}
	var actions []string
	for _, e := range h.entries() {
		if strings.HasPrefix(e.Action, "registry-") {
			actions = append(actions, e.Action+" "+e.Result+" "+strings.Join(e.Paths, ","))
		}
	}
	want := []string{"registry-set success spec.registries", "registry-set success ", "registry-set failure ", "registry-set success spec.registries"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit %q, want %q", actions, want)
	}
}

func TestRegistryRemove(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	name := registrySecretName(testRegistryHost)
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathRegistries+"/hub.example.com", RegistryCredentialsUpdate{Username: "puller", Password: ptr(registryPassword)}, s), http.StatusAccepted)

	// The host is matched without regard to case.
	wantStatus(t, h.request(http.MethodDelete, pathRegistries+"/REGISTRY.example.com:5000", nil, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Registries; len(got) != 1 || got[0].Host != "hub.example.com" {
		t.Errorf("registries %+v, want only hub.example.com", got)
	}
	if registrySecretExists(t, name) {
		t.Error("the credentials Secret of the removed registry is kept")
	}
	wantStatus(t, h.request(http.MethodGet, pathTestRegistry, nil, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodDelete, pathTestRegistry, nil, s), http.StatusNotFound)
	if last := h.entries()[len(h.entries())-1]; last.Action != string(AuditEntryActionRegistryRemove) || last.Result != string(Failure) {
		t.Errorf("audit entry %+v, want a failed registry-remove", last)
	}
}

func TestRegistryWithoutCredentialsIsListed(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Spec.Registries = []v1alpha1.RegistrySpec{{Host: "mirror.example.com"}}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	rec := h.request(http.MethodGet, pathRegistries+"/mirror.example.com", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var one RegistryCredentials
	decode(t, rec, &one)
	if one != (RegistryCredentials{Host: "mirror.example.com"}) {
		t.Errorf("registry %+v, want no credentials", one)
	}
	// A password cannot be kept for a registry that had none.
	wantStatus(t, h.request(http.MethodPut, pathRegistries+"/mirror.example.com", RegistryCredentialsUpdate{Username: "robot"}, s), http.StatusBadRequest)
}

func TestRegistryRejects(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	valid := RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}
	for name, tc := range map[string]struct {
		host  string
		body  any
		field string
	}{
		"a scheme":          {"https:%2F%2Fregistry.example.com", valid, "host"},
		"a path":            {"registry.example.com%2Fv2", valid, "host"},
		"a user":            {"robot@registry.example.com", valid, "host"},
		"port out of range": {"registry.example.com:70000", valid, "host"},
		"no port number":    {"registry.example.com:", valid, "host"},
		"bad name":          {"bad_host!", valid, "host"},
		"no user name":      {testRegistryHost, RegistryCredentialsUpdate{Password: ptr(registryPassword)}, "username"},
		"control in name":   {testRegistryHost, RegistryCredentialsUpdate{Username: "ro\nbot", Password: ptr(registryPassword)}, "username"},
		"no password":       {testRegistryHost, RegistryCredentialsUpdate{Username: "robot"}, "password"},
		"empty password":    {testRegistryHost, RegistryCredentialsUpdate{Username: "robot", Password: ptr("")}, "password"},
		"long password":     {testRegistryHost, RegistryCredentialsUpdate{Username: "robot", Password: ptr(strings.Repeat("p", maxCredentialLength+1))}, "password"},
	} {
		rec := h.request(http.MethodPut, pathRegistries+"/"+tc.host, tc.body, s)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s, want 400 for field %s", name, rec.Code, rec.Body, tc.field)
		}
	}
	if got := getAppliance(t).Spec.Registries; len(got) != 0 {
		t.Errorf("registries %+v after refused requests", got)
	}
	for _, host := range []string{"[2001:db8::1]:5000", "192.0.2.80", "Registry.Example.com"} {
		wantStatus(t, h.request(http.MethodPut, pathRegistries+"/"+host, valid, s), http.StatusAccepted)
	}
	got := getAppliance(t).Spec.Registries
	if hosts := []string{got[0].Host, got[1].Host, got[2].Host}; !slices.Equal(hosts, []string{"[2001:db8::1]:5000", "192.0.2.80", "registry.example.com"}) {
		t.Errorf("hosts %v, want them in canonical form", hosts)
	}
}

func TestRegistriesNeedTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, pathRegistries, nil},
		{http.MethodGet, pathTestRegistry, nil},
		{http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}},
		{http.MethodDelete, pathTestRegistry, nil},
	} {
		if rec := h.request(tc.method, tc.path, tc.body, s); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), codeNotSetUp) {
			t.Errorf("%s %s without the Appliance: %d %s, want 409 %s", tc.method, tc.path, rec.Code, rec.Body, codeNotSetUp)
		}
	}
}

func TestRegistryInUseByAComponent(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{privateComponent}}, s), http.StatusAccepted)

	rec := h.request(http.MethodDelete, pathTestRegistry, nil, s)
	wantStatus(t, rec, http.StatusConflict)
	if body := rec.Body.String(); !strings.Contains(body, `"code":"registry_in_use"`) || !strings.Contains(body, "External Authority Provider") {
		t.Errorf("answer %s, want registry_in_use naming the component", body)
	}
	if got := getAppliance(t).Spec.Registries; len(got) != 1 || !registrySecretExists(t, registrySecretName(testRegistryHost)) {
		t.Errorf("registries %+v: a refused removal changed them", got)
	}

	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodDelete, pathTestRegistry, nil, s), http.StatusAccepted)
}

func TestRegistryWrittenOutsideTheAPI(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	a := createAppliance(t, v1alpha1.SetupComplete)
	imported := registrySecretPrefix + "imported"
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: imported},
		Data:       map[string][]byte{"username": []byte("robot"), "password": []byte(registryPassword)},
	}); err != nil {
		t.Fatal(err)
	}
	a.Spec.Registries = []v1alpha1.RegistrySpec{{Host: "Registry.Example.com:5000", CredentialsRef: imported}}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	// A host not in canonical form is found by its canonical form.
	wantStatus(t, h.request(http.MethodGet, pathTestRegistry, nil, s), http.StatusOK)

	// New credentials replace the entry and the Secret it referenced.
	wantStatus(t, h.request(http.MethodPut, pathTestRegistry, RegistryCredentialsUpdate{Username: "robot", Password: ptr(registryPasswordNext)}, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Registries; len(got) != 1 || got[0].Host != testRegistryHost || got[0].CredentialsRef != registrySecretName(testRegistryHost) {
		t.Errorf("registries %+v, want the one entry in canonical form", got)
	}
	if registrySecretExists(t, imported) {
		t.Error("the Secret the entry referenced before is kept")
	}

	// Removal deletes the Secret the entry references.
	a = getAppliance(t)
	a.Spec.Registries[0].CredentialsRef = imported
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: imported},
		Data:       map[string][]byte{"username": []byte("robot"), "password": []byte(registryPassword)},
	}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.request(http.MethodDelete, pathTestRegistry, nil, s), http.StatusAccepted)
	if registrySecretExists(t, imported) {
		t.Error("the Secret the removed entry referenced is kept")
	}
}
