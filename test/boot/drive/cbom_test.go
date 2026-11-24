package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// statusConsole serves the console's status with conditions, a type mapped to its status and reason.
func statusConsole(t *testing.T, conditions map[string][2]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/appliance/api/v1/status" {
			http.NotFound(w, r)
			return
		}
		list := []map[string]string{}
		for typ, sr := range conditions {
			list = append(list, map[string]string{"type": typ, "status": sr[0], "reason": sr[1]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"setup": "complete", "conditions": list})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConditionsTrue(t *testing.T) {
	srv := statusConsole(t, map[string][2]string{
		conditionILMSettings: {"True", "Applied"}, conditionCBOM: {"False", "Failed"},
	})
	c := newClient(srv.URL, nil, "")
	if err := c.conditionsTrue(context.Background(), conditionILMSettings); err != nil {
		t.Errorf("conditionsTrue(%s) = %v", conditionILMSettings, err)
	}
	err := c.conditionsTrue(context.Background(), conditionILMSettings, conditionCBOM)
	if err == nil || !strings.Contains(err.Error(), conditionCBOM) || !strings.Contains(err.Error(), "Failed") {
		t.Errorf("conditionsTrue with a False condition = %v, want an error naming it and its reason", err)
	}
	err = c.conditionsTrue(context.Background(), "Missing")
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Errorf("conditionsTrue with a missing condition = %v, want an error naming it", err)
	}
}

func TestEnableCBOM(t *testing.T) {
	var put map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /appliance/api/v1/platform", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hostName": testHostName,
			"components": []map[string]any{
				{"name": "chosen-connector", "chosen": true}, {"name": "other-connector", "chosen": false},
			},
			"cbom": map[string]any{"enabled": false, "storage": "appliance"},
		})
	})
	mux.HandleFunc("PUT /appliance/api/v1/platform", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&put)
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	captureStdout(t, func() {
		if err := newClient(srv.URL, nil, "").enableCBOM(context.Background()); err != nil {
			t.Fatalf("enableCBOM = %v", err)
		}
	})
	components, _ := put["components"].([]any)
	cbom, _ := put["cbom"].(map[string]any)
	if put["hostName"] != testHostName || len(components) != 1 || components[0] != "chosen-connector" {
		t.Errorf("PUT platform with host %v and components %v, want the current ones", put["hostName"], components)
	}
	if cbom["enabled"] != true || cbom["storage"] != "appliance" {
		t.Errorf("PUT platform with cbom %v, want it on with the store on the appliance", cbom)
	}
}

// cbomILM models ILM's CBOM API: uploads are kept by serial number and listed; refuse answers every upload with 503.
type cbomILM struct {
	content map[string]map[string]any
	refuse  bool
	altered bool
}

func (s *cbomILM) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+cbomUploadPath, func(w http.ResponseWriter, r *http.Request) {
		if s.refuse {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body struct{ Content map[string]any }
		_ = json.NewDecoder(r.Body).Decode(&body)
		serial, _ := body.Content["serialNumber"].(string)
		s.content[serial] = body.Content
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST "+cbomListPath, func(w http.ResponseWriter, _ *http.Request) {
		items := []map[string]any{}
		for serial := range s.content {
			items = append(items, map[string]any{"uuid": "u-" + serial, "serialNumber": serial, "version": 1})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("GET "+cbomListPath+"/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		serial := strings.TrimPrefix(r.PathValue("uuid"), "u-")
		content, ok := s.content[serial]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s.altered {
			content = map[string]any{"serialNumber": "urn:uuid:other"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "u-" + serial, "serialNumber": serial, "content": content})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUploadedCBOMIsReadable(t *testing.T) {
	s := &cbomILM{content: map[string]map[string]any{}}
	srv := s.serve(t)
	ctx := context.Background()
	out := captureStdout(t, func() {
		if err := cbomReadable(ctx, srv.Client(), srv.URL); err == nil {
			t.Error("cbomReadable passed before the upload")
		}
		if err := uploadCBOM(ctx, srv.Client(), srv.URL); err != nil {
			t.Fatalf("uploadCBOM = %v", err)
		}
		if err := cbomReadable(ctx, srv.Client(), srv.URL); err != nil {
			t.Errorf("cbomReadable after the upload = %v", err)
		}
	})
	if _, ok := s.content[cbomSerial]; !ok || len(s.content) != 1 {
		t.Errorf("uploaded %d CBOM(s), want one with serial number %s", len(s.content), cbomSerial)
	}
	if !strings.Contains(out, cbomSerial) {
		t.Errorf("drive printed no %q:\n%s", cbomSerial, out)
	}
}

// A retried upload whose earlier attempt was stored, with its answer lost, does not upload again.
func TestStoreCBOMSkipsAStoredCBOM(t *testing.T) {
	s := &cbomILM{content: map[string]map[string]any{cbomSerial: cbomDocument}, refuse: true}
	srv := s.serve(t)
	captureStdout(t, func() {
		if err := storeCBOM(context.Background(), srv.Client(), srv.URL); err != nil {
			t.Errorf("storeCBOM = %v, want the stored CBOM taken", err)
		}
	})
}

func TestCBOMChecksFail(t *testing.T) {
	ctx := context.Background()
	refusing := &cbomILM{content: map[string]map[string]any{}, refuse: true}
	srv := refusing.serve(t)
	if err := uploadCBOM(ctx, srv.Client(), srv.URL); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("uploadCBOM refused = %v, want an error naming HTTP 503", err)
	}
	altered := &cbomILM{content: map[string]map[string]any{cbomSerial: {"serialNumber": cbomSerial}}, altered: true}
	srv = altered.serve(t)
	captureStdout(t, func() {
		if err := cbomReadable(ctx, srv.Client(), srv.URL); err == nil {
			t.Error("cbomReadable passed with other content")
		}
	})
}

func TestValidateRequiresTheCBOMFlags(t *testing.T) {
	o := options{user: "u", password: "p", seedFile: "s", hostName: "h", credential: "c", credentialPasswordFile: "f"}
	o.create = true
	if err := o.validate(cmdCBOM); err == nil {
		t.Error("cbom without --ui-url and --ui-addr passed validation")
	}
	o.uiURL, o.uiAddr = "https://ilm.test", "a"
	if err := o.validate(cmdCBOM); err != nil {
		t.Errorf("cbom --create = %v", err)
	}
	o.check = true
	if err := o.validate(cmdCBOM); err == nil {
		t.Error("cbom with --create and --check passed validation")
	}
	o.create, o.credential = false, ""
	if err := o.validate(cmdCBOM); err == nil {
		t.Error("cbom without --credential passed validation")
	}
}
