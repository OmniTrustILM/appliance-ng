package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
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

	for _, path := range []string{"/api/v1/appliance", "/api/v1/status", "/api/v1/audit", "/api/v1/session", "/api/v1/catalogue"} {
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
