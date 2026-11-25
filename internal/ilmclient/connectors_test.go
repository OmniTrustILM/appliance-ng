package ilmclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// connectorCall is a request the connector tests' server received.
type connectorCall struct{ method, path, body string }

// connectorClient returns a client of a server that records its requests and answers each with status and body.
func connectorClient(t *testing.T, status int, body string) (*Client, *[]connectorCall) {
	t.Helper()
	var calls []connectorCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		calls = append(calls, connectorCall{r.Method, r.URL.EscapedPath(), string(data)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return newLocal(srv.URL), &calls
}

func TestListConnectors(t *testing.T) {
	c, calls := connectorClient(t, http.StatusOK,
		`[{"uuid":"u1","name":"x509","url":"http://x509:8080","status":"connected","authType":"none"}]`)
	got, err := c.ListConnectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Connector{{UUID: "u1", Name: "x509", URL: "http://x509:8080", Status: "connected"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connectors %+v, want %+v", got, want)
	}
	if want := []connectorCall{{http.MethodGet, "/api/v1/connectors", ""}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v, want %+v", *calls, want)
	}
}

func TestCreateConnector(t *testing.T) {
	c, calls := connectorClient(t, http.StatusOK, `{"uuid":"new-uuid"}`)
	uuid, err := c.CreateConnector(context.Background(), "x509", "http://x509:8080")
	if err != nil || uuid != "new-uuid" {
		t.Fatalf("uuid %q, err %v", uuid, err)
	}
	want := []connectorCall{{http.MethodPost, "/api/v1/connectors", `{"authType":"none","name":"x509","url":"http://x509:8080"}`}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v, want %+v", *calls, want)
	}
}

func TestReconnectAndDeleteConnector(t *testing.T) {
	c, calls := connectorClient(t, http.StatusNoContent, "")
	ctx := context.Background()
	if err := c.ReconnectConnector(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteConnector(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	want := []connectorCall{
		{http.MethodPut, "/api/v1/connectors/u1/reconnect", ""},
		{http.MethodDelete, "/api/v1/connectors/a%2Fb", ""},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v, want %+v", *calls, want)
	}
}

func TestConnectorErrorsUseFixedReasons(t *testing.T) {
	const quoted = "secret-quoted-by-ilm"
	for status, reason := range map[int]string{
		http.StatusForbidden: ReasonAuthFailed, http.StatusNotFound: ReasonNotFound,
		http.StatusUnprocessableEntity: ReasonInvalid, http.StatusInternalServerError: ReasonFailed,
	} {
		c, _ := connectorClient(t, status, quoted)
		ctx := context.Background()
		_, listErr := c.ListConnectors(ctx)
		_, createErr := c.CreateConnector(ctx, "n", "http://n")
		for _, err := range []error{listErr, createErr, c.ReconnectConnector(ctx, "u"), c.DeleteConnector(ctx, "u")} {
			if Reason(err) != reason || strings.Contains(err.Error(), quoted) {
				t.Errorf("status %d: reason %q, error %v", status, Reason(err), err)
			}
		}
	}
}

func TestConnectorBadAnswerIsNotQuoted(t *testing.T) {
	c, _ := connectorClient(t, http.StatusOK, "not-json-secret")
	_, err := c.ListConnectors(context.Background())
	if Reason(err) != ReasonFailed || strings.Contains(err.Error(), "not-json-secret") {
		t.Fatalf("reason %q, error %v", Reason(err), err)
	}
}
