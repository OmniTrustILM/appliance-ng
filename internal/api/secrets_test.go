package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/mail"
)

// TestNoSecretLeavesTheManager sets credentials through the API and looks for them in every answer, the
// Appliance, the Events, the audit log and the manager's log.
func TestNoSecretLeavesTheManager(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupPart2)
	const mailPassword = "smtp-Pa55word-that-must-stay-inside"

	// A failed sign-in with the password in the user field and a successful one.
	wantStatus(t, h.request(http.MethodPost, "/api/v1/session", signInBody(testPassword, testPassword, "123456"), nil), http.StatusBadRequest)
	code := h.code()
	s := h.signIn()

	mail := validMail()
	mail.Password = ptr(mailPassword)
	var answers bytes.Buffer
	put := h.request(http.MethodPut, "/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}, Mail: mail}, s)
	wantStatus(t, put, http.StatusAccepted)
	answers.Write(put.Body.Bytes())

	cred := newAdminCredential(t, pkcs1)
	createAdminSecret(t, cred)
	wantStatus(t, h.request(http.MethodPost, "/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, s), http.StatusOK)

	uploadedKey, csrKey := certificateAnswers(t, h, s, &answers)
	registryAnswers(t, h, s, &answers)
	mailAnswers(t, h, s, &answers)

	for _, path := range []string{
		"/api/v1/appliance", "/api/v1/status", "/api/v1/audit", "/api/v1/session", "/api/v1/catalogue", pathCertificate,
		pathTrust, pathRegistries, pathTestRegistry, pathMail, pathPlatform,
	} {
		rec := h.request(http.MethodGet, path, nil, s)
		wantStatus(t, rec, http.StatusOK)
		answers.Write(rec.Body.Bytes())
	}

	appliance, err := json.Marshal(getAppliance(t))
	if err != nil {
		t.Fatal(err)
	}
	events := eventsJSON(t)
	auditFiles := readDir(t, h.auditFS)

	secrets := map[string]string{
		"mail password":       mailPassword,
		"console password":    testPassword,
		"TOTP seed":           h.seed,
		"TOTP code":           code,
		"credential password": credentialPassword,
		"admin key":           keyBody(cred.key),
		"uploaded key":        keyBody(uploadedKey),
		"CSR key":             keyBody(csrKey),
		"registry password":   registryPassword,
		"next registry pw":    registryPasswordNext,
		"console mail pw":     consoleMailPassword,
	}
	for where, data := range map[string]string{
		"API answers":   answers.String(),
		"the Appliance": string(appliance),
		"Events":        events,
		"the audit log": auditFiles,
		"the log":       h.logs.String(),
	} {
		for what, secret := range secrets {
			if strings.Contains(data, secret) || strings.Contains(data, base64.StdEncoding.EncodeToString([]byte(secret))) {
				t.Errorf("%s holds the %s", where, what)
			}
		}
	}
	if !strings.Contains(h.logs.String(), "sign") {
		t.Error("the log is empty; the test would not notice a leak")
	}
}

// certificateAnswers uploads a certificate, refuses one, creates a signing request and installs its certificate,
// adding the answers to answers. It returns the uploaded and the signing request's private keys.
func certificateAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) (uploadedKey, csrKey []byte) {
	t.Helper()
	a := getAppliance(t)
	a.Status.NodeAddress = certNodeAddress
	if err := testClient.Status().Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	ca := newTestCA(t)
	certPEM, uploadedKey := ca.leaf(t)
	for _, rec := range []*httptest.ResponseRecorder{
		h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": uploadedKey, "chain": ca.pem}, s),
		h.upload(http.MethodPut, pathCertificate, map[string][]byte{"certificate": certPEM, "key": uploadedKey}, s),
	} {
		answers.Write(rec.Body.Bytes())
	}
	csr := h.request(http.MethodPost, pathCSR, nil, s)
	wantStatus(t, csr, http.StatusOK)
	answers.Write(csr.Body.Bytes())
	csrKey = systemSecret(t, certs.CSRSecretName).Data[corev1.TLSPrivateKeyKey]
	signed := ca.sign(t, csr.Body.Bytes())
	for _, rec := range []*httptest.ResponseRecorder{
		h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": certPEM}, s),
		h.upload(http.MethodPut, pathSigned, map[string][]byte{"certificate": signed, "chain": ca.pem}, s),
	} {
		answers.Write(rec.Body.Bytes())
	}
	return uploadedKey, csrKey
}

// registryAnswers sets registry credentials, keeps the password, refuses a change and replaces the password, adding
// the answers to answers.
func registryAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) {
	t.Helper()
	for _, body := range []RegistryCredentialsUpdate{
		{Username: "robot", Password: ptr(registryPassword)},
		{Username: "robot"},
		{Username: "other"},
		{Username: "other", Password: ptr(registryPasswordNext)},
	} {
		answers.Write(h.request(http.MethodPut, pathTestRegistry, body, s).Body.Bytes())
	}
	wantStatus(t, h.request(http.MethodPut, pathRegistries+"/hub.example.com", RegistryCredentialsUpdate{Username: "puller", Password: ptr(registryPassword)}, s), http.StatusAccepted)
	answers.Write(h.request(http.MethodDelete, pathRegistries+"/hub.example.com", nil, s).Body.Bytes())
}

// consoleMailPassword is the SMTP password set through PUT /mail.
const consoleMailPassword = "smtp-Pa55word-set-in-the-console"

// mailAnswers sets the mail server with a password, keeps it, refuses a change without it and sends a test mail
// that the server refuses with a reply repeating the password, adding the answers to answers.
func mailAnswers(t *testing.T, h *harness, s *session, answers *bytes.Buffer) {
	t.Helper()
	m := validMail()
	m.Password = ptr(consoleMailPassword)
	answers.Write(h.request(http.MethodPut, pathMail, m, s).Body.Bytes())
	m.Password = nil
	answers.Write(h.request(http.MethodPut, pathMail, m, s).Body.Bytes())
	m.Username = ptr("another-user")
	answers.Write(h.request(http.MethodPut, pathMail, m, s).Body.Bytes())
	h.withSender(&mail.Error{Stage: mail.StageAuth, Err: errors.New("535 refused " + consoleMailPassword)})
	test := h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s)
	wantStatus(t, test, http.StatusBadGateway)
	answers.Write(test.Body.Bytes())
}

// keyBody is the base64 body of the first PEM block, without line breaks, a distinctive part of the key.
func keyBody(p []byte) string {
	lines := strings.Split(strings.TrimSpace(string(p)), "\n")
	return lines[1]
}

func eventsJSON(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var out bytes.Buffer
	core := &corev1.EventList{}
	if err := testClient.List(ctx, core); err != nil {
		t.Fatal(err)
	}
	modern := &eventsv1.EventList{}
	if err := testClient.List(ctx, modern); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{core, modern} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(data)
	}
	return out.String()
}

func readDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(data)
	}
	if out.Len() == 0 {
		t.Fatal("the audit log is empty")
	}
	return out.String()
}
