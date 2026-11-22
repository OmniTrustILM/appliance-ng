package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/audit"
)

func TestAuditEntryPerMutation(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupPart2)

	// An unknown user name is not recorded: it may be a password typed into the wrong field.
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody("someone", "wrong password value", "000000"), nil), http.StatusBadRequest)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", "000000"), nil), http.StatusBadRequest)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/status", nil, s), http.StatusOK)
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "bad", Components: []string{}}, s), http.StatusBadRequest)
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}}, &session{cookie: s.cookie, csrf: "x"}), http.StatusForbidden)
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": "a long enough password"}, s), http.StatusNotFound)
	wantStatus(t, h.request(http.MethodDelete, "/appliance/api/v1/session", nil, s), http.StatusNoContent)
	// Neither a change without a session nor a sign-in while locked is audited.
	wantStatus(t, h.request(http.MethodDelete, "/appliance/api/v1/session", nil, s), http.StatusUnauthorized)
	for range 5 {
		h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, "wrong password value", "000000"), nil)
	}
	wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/session", signInBody(testUser, testPassword, h.code()), nil), http.StatusUnauthorized)

	got := h.entries()
	got = got[:len(got)-5] // the five failed sign-ins before the lock
	want := []struct {
		user   string
		action AuditEntryAction
		result AuditEntryResult
		paths  []string
	}{
		{"", AuditEntryActionSignIn, Failure, nil},
		{testUser, AuditEntryActionSignIn, Failure, nil},
		{testUser, AuditEntryActionSignIn, Success, nil},
		{testUser, AuditEntryActionWizardPlatform, Success, []string{
			"spec.mail", "spec.mail.credentialsRef", "spec.platform",
		}},
		{testUser, AuditEntryActionWizardPlatform, Failure, nil},
		{testUser, AuditEntryActionWizardPlatform, Denied, nil},
		{testUser, AuditEntryActionAdminCredential, Failure, nil},
		{testUser, AuditEntryActionSignOut, Success, nil},
	}
	if len(got) != len(want) {
		t.Fatalf("audit entries = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.User != w.user || g.Action != string(w.action) || g.Result != string(w.result) || !slices.Equal(g.Paths, w.paths) || g.Time.IsZero() {
			t.Errorf("entry %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestAnonymousChangesAreNotAudited(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupPart2)
	stale := &session{cookie: "not-a-session", csrf: "x"}
	for range 50 {
		wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", PlatformSettings{HostName: "ilm.example.com", Components: []string{}}, stale), http.StatusUnauthorized)
		wantStatus(t, h.request(http.MethodPost, "/appliance/api/v1/platform/admin-credential", map[string]string{"password": credentialPassword}, nil), http.StatusUnauthorized)
		wantStatus(t, h.request(http.MethodDelete, "/appliance/api/v1/session", nil, nil), http.StatusUnauthorized)
	}
	if got := h.entries(); len(got) != 0 {
		t.Errorf("anonymous requests wrote %d audit entries, want none", len(got))
	}
}

func TestAuditPathsOfAChange(t *testing.T) {
	h := newHarness(t)
	createAppliance(t, v1alpha1.SetupPart2)
	s := h.signIn()
	body := PlatformSettings{HostName: "ilm.example.com", Components: []string{"ejbca-ng-connector"}, Mail: validMail()}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Components = []string{"common-credential-provider"}
	body.Mail.From = "alerts@example.com"
	body.Mail.Password = nil
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)
	body.Mail.Password = ptr("another-secret-value")
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/wizard/platform", body, s), http.StatusAccepted)

	got := h.entries()
	for i, want := range [][]string{
		{"spec.mail.from", "spec.platform.components"},
		nil,
		{"spec.mail.credentialsRef"},
	} {
		if e := got[len(got)-3+i]; !slices.Equal(e.Paths, want) {
			t.Errorf("paths of change %d = %v, want %v", i, e.Paths, want)
		}
	}
}

func TestGetAudit(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	h.signIn()
	h.signIn()

	rec := h.request(http.MethodGet, "/appliance/api/v1/audit?limit=2", nil, s)
	wantStatus(t, rec, http.StatusOK)
	var got AuditLog
	decode(t, rec, &got)
	if len(got.Items) != 2 || !got.Items[0].Time.After(got.Items[1].Time) || got.Items[0].Action != AuditEntryActionSignIn || got.Items[0].Paths == nil {
		t.Errorf("audit = %+v, want the two newest sign-ins, newest first", got.Items)
	}
	decode(t, h.request(http.MethodGet, "/appliance/api/v1/audit", nil, s), &got)
	if len(got.Items) != 3 {
		t.Errorf("audit = %d entries, want 3", len(got.Items))
	}
	for _, q := range []string{"limit=0", "limit=1001", "limit=x"} {
		wantStatus(t, h.request(http.MethodGet, "/appliance/api/v1/audit?"+q, nil, s), http.StatusBadRequest)
	}
}

// failingAudit is an audit log whose writes and reads fail.
type failingAudit struct{}

func (failingAudit) Write(audit.Entry) error         { return errors.New("disk full") }
func (failingAudit) Tail(int) ([]audit.Entry, error) { return nil, errors.New("disk gone") }

func TestAuditFailureStillAnswers(t *testing.T) {
	h := newHarness(t)
	h.srv.deps.Audit = failingAudit{}
	s := h.signIn()
	if !strings.Contains(h.logs.String(), "audit") {
		t.Errorf("the failed audit write is not logged: %s", h.logs)
	}
	rec := h.request(http.MethodGet, "/appliance/api/v1/audit", nil, s)
	wantStatus(t, rec, http.StatusInternalServerError)
	if strings.Contains(rec.Body.String(), "disk gone") {
		t.Errorf("the error leaks to the client: %s", rec.Body)
	}
}
