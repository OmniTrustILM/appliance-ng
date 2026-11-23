package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testClientSecret = "oidc-client-secret-61b7"

// ilmServer models the ILM groups API and the Keycloak token endpoint as clientSecretAccepted expects them.
type ilmServer struct {
	groups       []string
	acceptsAll   bool
	refusesRight bool
}

func (s *ilmServer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/groups", func(w http.ResponseWriter, _ *http.Request) {
		list := []map[string]string{}
		for _, g := range s.groups {
			list = append(list, map[string]string{"uuid": "u-" + g, "name": g})
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("POST /api/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.groups = append(s.groups, body.Name)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST "+tokenPath, func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != ilmClientID || r.FormValue("grant_type") != "client_credentials" {
			t.Errorf("token request for client %q grant %q", r.FormValue("client_id"), r.FormValue("grant_type"))
		}
		right := r.FormValue("client_secret") == testClientSecret
		answer := map[string]string{"error": "unauthorized_client", "error_description": "Invalid client or Invalid client credentials"}
		status := http.StatusUnauthorized
		switch {
		case right && s.refusesRight:
			answer["error"] = "invalid_client"
		case right || s.acceptsAll:
			answer["error_description"], status = "Client not enabled to retrieve service account", http.StatusBadRequest
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateMarker(t *testing.T) {
	s := &ilmServer{}
	srv := s.serve(t)
	out := captureStdout(t, func() {
		for range 2 {
			if err := createMarker(context.Background(), srv.Client(), srv.URL); err != nil {
				t.Fatalf("createMarker = %v", err)
			}
		}
	})
	if strings.Join(s.groups, ",") != markerGroup {
		t.Errorf("groups = %v, want one %s", s.groups, markerGroup)
	}
	if !strings.Contains(out, markerGroup) {
		t.Errorf("drive printed no %q:\n%s", markerGroup, out)
	}
}

func TestDriveILMMarkerCheckNeverPrintsSecrets(t *testing.T) {
	s := &ilmServer{groups: []string{"other", markerGroup}}
	srv := s.serve(t)
	var err error
	out := captureStdout(t, func() { err = checkRestored(context.Background(), srv.Client(), srv.URL, testClientSecret) })
	if err != nil {
		t.Fatalf("checkRestored = %v\n%s", err, out)
	}
	if strings.Contains(out, testClientSecret) {
		t.Errorf("drive printed the client secret:\n%s", out)
	}
	for _, want := range []string{markerGroup, "accepts", "refuses"} {
		if !strings.Contains(out, want) {
			t.Errorf("drive printed no %q:\n%s", want, out)
		}
	}
}

func TestCheckRestoredFails(t *testing.T) {
	for name, s := range map[string]*ilmServer{
		"no marker":                      {groups: []string{"other"}},
		"Keycloak refuses the secret":    {groups: []string{markerGroup}, refusesRight: true},
		"Keycloak tells no secret apart": {groups: []string{markerGroup}, acceptsAll: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := s.serve(t)
			var err error
			captureStdout(t, func() { err = checkRestored(context.Background(), srv.Client(), srv.URL, testClientSecret) })
			if err == nil {
				t.Fatal("checkRestored passed")
			}
			if strings.Contains(err.Error(), testClientSecret) {
				t.Errorf("the error shows the client secret: %v", err)
			}
		})
	}
}

func TestValidateRequiresTheMarkerFlags(t *testing.T) {
	o := options{hostName: "h", credential: "c", credentialPasswordFile: "p", uiURL: "https://ilm.test", uiAddr: "a"}
	if err := o.validate("ilm-marker"); err == nil {
		t.Error("ilm-marker without --create or --check passed validation")
	}
	o.create, o.check = true, true
	if err := o.validate("ilm-marker"); err == nil {
		t.Error("ilm-marker with --create and --check passed validation")
	}
	o.check = false
	if err := o.validate("ilm-marker"); err != nil {
		t.Errorf("ilm-marker --create = %v", err)
	}
	o.create, o.check = false, true
	if err := o.validate("ilm-marker"); err == nil {
		t.Error("ilm-marker --check without the kit passed validation")
	}
	o.kitFile, o.passphraseFile = "k", "p"
	if err := o.validate("ilm-marker"); err != nil {
		t.Errorf("ilm-marker --check = %v", err)
	}
}
