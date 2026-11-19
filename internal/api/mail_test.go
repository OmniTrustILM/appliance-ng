package api

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/mail"
)

const (
	pathMail     = "/api/v1/mail"
	pathMailTest = pathMail + "/test"
	mailTestTo   = "admin@example.com"
)

// sentMail is what the server handed the mail sender.
type sentMail struct {
	spec           v1alpha1.MailSpec
	user, password string
	to             string
	roots          *x509.CertPool
}

// withSender makes the server send test mail through a fake that records the call and fails with err.
func (h *harness) withSender(err error) *[]sentMail {
	sent := &[]sentMail{}
	h.srv.sendMail = func(_ context.Context, m v1alpha1.MailSpec, user, password, to string, roots *x509.CertPool) error {
		*sent = append(*sent, sentMail{m, user, password, to, roots})
		return err
	}
	return sent
}

func getMail(t *testing.T, h *harness, s *session) MailState {
	t.Helper()
	rec := h.request(http.MethodGet, pathMail, nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got MailState
	decode(t, rec, &got)
	return got
}

func TestMailPasswordWriteOnly(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	if got := getMail(t, h, s); got.Mail != nil {
		t.Fatalf("mail %+v before it was set", got.Mail)
	}

	m := validMail()
	wantStatus(t, h.request(http.MethodPut, pathMail, m, s), http.StatusAccepted)
	rec := h.request(http.MethodGet, pathMail, nil, s)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), *m.Password) {
		t.Fatal("GET /mail returns the password")
	}
	var got MailState
	decode(t, rec, &got)
	want := MailSettings{Host: "smtp.example.com", Port: 587, Security: Starttls, From: "ilm@example.com", Username: ptr("mailer"), PasswordSet: true}
	if got.Mail == nil || *got.Mail.Username != *want.Username || got.Mail.Host != want.Host || got.Mail.Port != want.Port ||
		got.Mail.Security != want.Security || got.Mail.From != want.From || !got.Mail.PasswordSet {
		t.Errorf("mail %+v, want %+v", got.Mail, want)
	}
	if spec := getAppliance(t).Spec.Mail; spec == nil || spec.CredentialsRef != MailSecretName {
		t.Errorf("spec.mail %+v, want the credentials Secret referenced", spec)
	}
	if string(mailSecret(t).Data[MailPasswordKey]) != *m.Password {
		t.Error("the password is not in the Secret")
	}

	// Without a password the stored one is kept for the same server and user.
	m.Password, m.From = nil, "pki@example.com"
	wantStatus(t, h.request(http.MethodPut, pathMail, m, s), http.StatusAccepted)
	if string(mailSecret(t).Data[MailPasswordKey]) != "smtp-secret-value" {
		t.Error("the stored password was not kept")
	}
	// But never for another server.
	m.Host = "smtp.elsewhere.example.com"
	rec = h.request(http.MethodPut, pathMail, m, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "password", "again")

	entries := h.entries()
	if last := entries[len(entries)-2]; last.Action != string(AuditEntryActionMailSettings) || !slices.Equal(last.Paths, []string{"spec.mail.from"}) {
		t.Errorf("audit entry %+v", last)
	}
}

func TestMailValidates(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	m := validMail()
	m.Port = 0
	rec := h.request(http.MethodPut, pathMail, m, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "port", "port")
	m = validMail()
	m.Security = None
	rec = h.request(http.MethodPut, pathMail, m, s)
	wantStatus(t, rec, http.StatusBadRequest)
	wantField(t, rec.Body.Bytes(), "username", "starttls or tls")
	// Without credentials an unencrypted server is accepted.
	m.Username, m.Password = nil, nil
	wantStatus(t, h.request(http.MethodPut, pathMail, m, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodDelete, pathMail, nil, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathMail, "", s), http.StatusBadRequest)
	if getAppliance(t).Spec.Mail != nil {
		t.Error("mail written by a refused request")
	}
}

func TestDeleteMail(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{testMailComponent}}, s), http.StatusAccepted)

	rec := h.request(http.MethodDelete, pathMail, nil, s)
	wantStatus(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "Email Notification Provider") {
		t.Errorf("answer %s does not name the component", rec.Body)
	}

	wantStatus(t, h.request(http.MethodPut, pathPlatform, PlatformUpdate{HostName: "ilm.example.com", Components: []string{}}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodDelete, pathMail, nil, s), http.StatusAccepted)
	if getAppliance(t).Spec.Mail != nil {
		t.Error("spec.mail kept")
	}
	if registrySecretExists(t, MailSecretName) {
		t.Error("the mail credentials Secret kept")
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Action != string(AuditEntryActionMailRemove) || !slices.Equal(last.Paths, []string{"spec.mail"}) {
		t.Errorf("audit entry %+v", last)
	}
	// Removing what is not there changes nothing.
	wantStatus(t, h.request(http.MethodDelete, pathMail, nil, s), http.StatusAccepted)
}

func TestMailTestSendsWithTheStoredSettings(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	ca := newTestCA(t)
	wantStatus(t, h.request(http.MethodPost, pathTrust, TrustUpload{Pem: string(ca.pem)}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	sent := h.withSender(nil)

	wantStatus(t, h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s), http.StatusNoContent)

	if len(*sent) != 1 {
		t.Fatalf("sent %d mails", len(*sent))
	}
	got := (*sent)[0]
	if got.spec.Host != "smtp.example.com" || got.user != "mailer" || got.password != "smtp-secret-value" || got.to != mailTestTo {
		t.Errorf("sent with %+v", got)
	}
	// The additional trusted CAs are among the roots.
	certPEM, _ := ca.leaf(t)
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.roots == nil {
		t.Fatal("no roots")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: got.roots}); err != nil {
		t.Errorf("roots do not hold the trusted CA: %v", err)
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Action != string(AuditEntryActionMailTest) || last.Result != string(Success) || len(last.Paths) != 0 {
		t.Errorf("audit entry %+v", last)
	}
}

func TestMailTestFailureNamesTheStage(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	h.withSender(&mail.Error{Stage: mail.StageAuth, Err: errors.New("535 refused AUTH PLAIN smtp-secret-value")})

	rec := h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s)
	wantStatus(t, rec, http.StatusBadGateway)
	var got MailTestFailure
	decode(t, rec, &got)
	if got.Stage != StageAuth || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "535") {
		t.Errorf("answer %s", rec.Body)
	}
	if strings.Contains(h.logs.String(), "smtp-secret-value") {
		t.Error("the log holds the server's reply")
	}
	entries := h.entries()
	if last := entries[len(entries)-1]; last.Action != string(AuditEntryActionMailTest) || last.Result != string(Failure) {
		t.Errorf("audit entry %+v", last)
	}
}

func TestMailTestRefusals(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	installedAppliance(t)
	sent := h.withSender(nil)
	wantStatus(t, h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusAccepted)
	for _, to := range []string{"", "not an address", "Admin <admin@example.com>", "admin@example.com\r\nBcc: x@example.com"} {
		rec := h.request(http.MethodPost, pathMailTest, MailTest{To: to}, s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), "to", "")
	}
	wantStatus(t, h.request(http.MethodPost, pathMailTest, "", s), http.StatusBadRequest)
	if len(*sent) != 0 {
		t.Errorf("sent %d mails for refused requests", len(*sent))
	}
}

func TestMailBeforeTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodGet, pathMail, nil, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPut, pathMail, validMail(), s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodDelete, pathMail, nil, s), http.StatusConflict)
	wantStatus(t, h.request(http.MethodPost, pathMailTest, MailTest{To: mailTestTo}, s), http.StatusConflict)
}
