package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OmniTrustILM/appliance-ng/internal/auth"
)

func TestTOTPMatchesAuth(t *testing.T) {
	seed, _, err := auth.NewTOTP("admin", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{time.Unix(0, 0), time.Unix(1_800_000_029, 0), time.Now()} {
		code, err := totpCode(seed, at)
		if err != nil {
			t.Fatalf("totpCode at %v: %v", at, err)
		}
		if !auth.CheckTOTP(seed, code, at) {
			t.Errorf("auth.CheckTOTP refuses the code drive generated at %v", at)
		}
	}
}

func TestNextStep(t *testing.T) {
	used := time.Unix(1_800_000_010, 0) // inside the step that starts at 1_800_000_000
	for _, tc := range []struct {
		now  time.Time
		want time.Duration
	}{
		{time.Unix(1_800_000_010, 0), 20 * time.Second},
		{time.Unix(1_800_000_029, 0), time.Second},
		{time.Unix(1_800_000_030, 0), 0},
		{time.Unix(1_800_000_100, 0), 0},
	} {
		if got := untilNextStep(used, tc.now); got != tc.want {
			t.Errorf("untilNextStep(%v) = %v, want %v", tc.now.Unix(), got, tc.want)
		}
	}
}

func TestServesBuiltinCertificateWithoutTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n"))
	}))
	defer srv.Close()
	err := newClient(srv.URL).servesBuiltinCertificate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("servesBuiltinCertificate over plain HTTP = %v, want an error naming the missing certificate", err)
	}
}

func TestConnectorRegistered(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"connected":            {`[{"name":"X","status":"connected"}]`, true},
		"waiting for approval": {`[{"name":"X","status":"waitingForApproval"}]`, true},
		"offline":              {`[{"name":"X","status":"offline"}]`, false},
		"other connector":      {`[{"name":"Y","status":"connected"}]`, false},
		"none":                 {`[]`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/connectors" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		err := connectorRegistered(context.Background(), srv.Client(), srv.URL, "X")
		srv.Close()
		if (err == nil) != tc.want {
			t.Errorf("%s: connectorRegistered = %v, want registered %v", name, err, tc.want)
		}
	}
}
