package manager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	otilm "github.com/OmniTrustILM/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/ilmclient"
	"github.com/OmniTrustILM/appliance-ng/internal/platform"
)

const (
	complianceName = "x509-compliance-provider"
	emailName      = "email-notification-provider"
)

// fakeConnectorsEdge is the ILM edge with the connectors endpoints; down answers every request with 500.
type fakeConnectorsEdge struct {
	mu         sync.Mutex
	srv        *httptest.Server
	connectors []ilmclient.Connector
	requests   []string
	down       bool
	// createStatus and reconnectStatus, when set, are the statuses the edge answers a create and a reconnect with.
	createStatus    int
	reconnectStatus int
	// delay is how long the edge takes to answer.
	delay time.Duration
}

func newFakeConnectorsEdge(t *testing.T, connectors ...ilmclient.Connector) *fakeConnectorsEdge {
	t.Helper()
	e := &fakeConnectorsEdge{connectors: connectors}
	e.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(e.delay)
		e.mu.Lock()
		defer e.mu.Unlock()
		e.requests = append(e.requests, r.Method+" "+r.URL.Path)
		if e.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(e.connectors)
		case http.MethodPost:
			if e.createStatus != 0 {
				w.WriteHeader(e.createStatus)
				_, _ = w.Write([]byte(`["Connector Email Notification Provider already exists"]`))
				return
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			e.requests[len(e.requests)-1] += " " + body["name"] + " " + body["url"]
			_ = json.NewEncoder(w).Encode(map[string]string{"uuid": "new"})
		case http.MethodPut:
			if e.reconnectStatus != 0 {
				w.WriteHeader(e.reconnectStatus)
			}
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeConnectorsEdge) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.requests)
}

// writes are the calls that change ILM.
func (e *fakeConnectorsEdge) writes() []string {
	var out []string
	for _, c := range e.calls() {
		if !strings.HasPrefix(c, http.MethodGet) {
			out = append(out, c)
		}
	}
	return out
}

// connectorsIdentity returns a client of the fake edge.
type connectorsIdentity struct {
	edge  *fakeConnectorsEdge
	calls int
}

func (f *connectorsIdentity) Ensure(context.Context, string, string, *x509.CertPool) (*ilmclient.Client, error) {
	f.calls++
	pool := x509.NewCertPool()
	pool.AddCert(f.edge.srv.Certificate())
	return ilmclient.New(f.edge.srv.Listener.Addr().String(), "example.com", tls.Certificate{}, pool), nil
}

func v1URL(t *testing.T, name string) string {
	t.Helper()
	for _, c := range testRelease.Connectors {
		if c.Name == name {
			return platform.ConnectorURL(c)
		}
	}
	t.Fatalf("no connector %s in the catalogue", name)
	return ""
}

func running(names ...string) []otilm.Connector {
	var out []otilm.Connector
	for _, n := range names {
		out = append(out, otilm.Connector{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	return out
}

// v1Fixture returns a reconciler whose ILM is edge, with the edge certificate's copy in the platform namespace, and
// an Appliance choosing components whose ILM settings are applied.
func v1Fixture(t *testing.T, edge *fakeConnectorsEdge, components ...string) (*ApplianceReconciler, *connectorsIdentity, *v1alpha1.Appliance) {
	t.Helper()
	ctx := context.Background()
	tlsCopy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: platform.Namespace, Name: platform.TLSSecretName},
		Data:       map[string][]byte{corev1.TLSCertKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: edge.srv.Certificate().Raw})},
	}
	if err := testClient.Create(ctx, tlsCopy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tlsCopy) })
	identity := &connectorsIdentity{edge: edge}
	r := &ApplianceReconciler{Client: testClient, APIReader: testClient, Release: testRelease, ILM: identity}
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{
		Node:     v1alpha1.NodeSpec{HostName: "node.example.com", Address: "192.0.2.10/24"},
		Platform: platformSpec(components...),
	}}
	createAppliance(t, a.Spec)
	setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionTrue, reasonApplied, "the ILM settings are applied")
	return r, identity, a
}

func TestV1ConnectorAbsentFromILMIsCreated(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	r, _, a := v1Fixture(t, edge, complianceName)

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true).Awaiting

	want := "POST /api/v1/connectors X.509 Compliance Provider " + v1URL(t, complianceName)
	if got := edge.writes(); !slices.Equal(got, []string{want}) {
		t.Errorf("writes = %q, want %q", got, want)
	}
	if !slices.Equal(awaiting, []string{complianceName}) {
		t.Errorf("awaiting = %q, want the connector until ILM lists it connected", awaiting)
	}
}

func TestV1ConnectorNotConnectedIsReconnected(t *testing.T) {
	for _, status := range []string{"waitingForApproval", "failed", "offline"} {
		t.Run(status, func(t *testing.T) {
			edge := newFakeConnectorsEdge(t, ilmclient.Connector{UUID: "u1", Name: "x", URL: v1URL(t, complianceName), Status: status})
			r, _, a := v1Fixture(t, edge, complianceName)

			awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true).Awaiting

			if got := edge.writes(); !slices.Equal(got, []string{"PUT /api/v1/connectors/u1/reconnect"}) {
				t.Errorf("writes = %q", got)
			}
			if !slices.Equal(awaiting, []string{complianceName}) {
				t.Errorf("awaiting = %q", awaiting)
			}
		})
	}
}

func TestV1ConnectorConnectedIsLeftAlone(t *testing.T) {
	edge := newFakeConnectorsEdge(t, ilmclient.Connector{UUID: "u1", Name: "x", URL: v1URL(t, complianceName), Status: "connected"})
	r, _, a := v1Fixture(t, edge, complianceName)

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true).Awaiting

	if got := edge.writes(); len(got) != 0 {
		t.Errorf("writes = %q, want none", got)
	}
	if len(awaiting) != 0 {
		t.Errorf("awaiting = %q, want none", awaiting)
	}
}

func TestRemovedV1ConnectorIsDeletedFromILMAndNothingElse(t *testing.T) {
	edge := newFakeConnectorsEdge(t,
		ilmclient.Connector{UUID: "kept", Name: "email", URL: v1URL(t, emailName), Status: "connected"},
		ilmclient.Connector{UUID: "gone", Name: "compliance", URL: v1URL(t, complianceName), Status: "connected"},
		ilmclient.Connector{UUID: "admin", Name: "own", URL: "https://connector.example.com", Status: "connected"},
		ilmclient.Connector{UUID: "v2", Name: "ejbca", URL: "http://ejbca-ng-connector." + platform.Namespace + ".svc.cluster.local:8080", Status: "connected"},
	)
	r, _, a := v1Fixture(t, edge, emailName)

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(emailName), nil, true).Awaiting

	if got := edge.writes(); !slices.Equal(got, []string{"DELETE /api/v1/connectors/gone"}) {
		t.Errorf("writes = %q, want only the removed v1 connector deleted", got)
	}
	if len(awaiting) != 0 {
		t.Errorf("awaiting = %q, want none", awaiting)
	}
}

func TestV1ConnectorStillStartingIsNotRegistered(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	r, _, a := v1Fixture(t, edge, complianceName, emailName)

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(emailName), nil, true).Awaiting

	if got := edge.writes(); len(got) != 1 || !strings.Contains(got[0], v1URL(t, emailName)) {
		t.Errorf("writes = %q, want only the running connector created", got)
	}
	if !slices.Equal(awaiting, []string{emailName}) {
		t.Errorf("awaiting = %q", awaiting)
	}
}

func TestV1RegistrationWithILMDownWaitsWithoutFailing(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	edge.down = true
	r, _, a := v1Fixture(t, edge, complianceName, emailName)

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName, emailName), nil, true).Awaiting

	if !slices.Equal(awaiting, []string{complianceName, emailName}) {
		t.Errorf("awaiting = %q", awaiting)
	}
	if len(edge.writes()) != 0 {
		t.Errorf("writes = %q, want none", edge.writes())
	}
}

func TestV1RegistrationWaitsUntilILMSettingsAreApplied(t *testing.T) {
	cases := map[string]func(*v1alpha1.Appliance, *bool){
		"settings not applied": func(a *v1alpha1.Appliance, _ *bool) {
			setCondition(a, v1alpha1.ConditionILMSettingsApplied, metav1.ConditionFalse, reasonWaitingForPlatform, "waiting")
		},
		"settings unknown":   func(a *v1alpha1.Appliance, _ *bool) { a.Status.Conditions = nil },
		"platform not ready": func(_ *v1alpha1.Appliance, ready *bool) { *ready = false },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			edge := newFakeConnectorsEdge(t)
			r, identity, a := v1Fixture(t, edge, complianceName, emailName)
			ready := true
			change(a, &ready)

			awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName, emailName), nil, ready).Awaiting

			if identity.calls != 0 || len(edge.calls()) != 0 {
				t.Errorf("ILM was called: %d identity calls, requests %q", identity.calls, edge.calls())
			}
			if !slices.Equal(awaiting, []string{complianceName, emailName}) {
				t.Errorf("awaiting = %q", awaiting)
			}
		})
	}
}

func TestV1RegistrationWithoutAnEdgeCertificateWaits(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	r, _, a := v1Fixture(t, edge, complianceName)
	r.APIReader = unreadableReader{}

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true).Awaiting

	if !slices.Equal(awaiting, []string{complianceName}) {
		t.Errorf("awaiting = %q", awaiting)
	}
}

func TestV1RegistrationWaitsWhileARestoreRuns(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	r, identity, a := v1Fixture(t, edge, complianceName)
	stored := &v1alpha1.Appliance{}
	if err := testClient.Get(context.Background(), client.ObjectKey{Name: v1alpha1.ApplianceName}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Restore = &v1alpha1.RestoreStatus{Backup: "b.ilmbackup", Step: v1alpha1.RestoreStepDatabase, StartedAt: metav1.Now()}
	if err := testClient.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}

	awaiting := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true).Awaiting

	if !slices.Equal(awaiting, []string{complianceName}) {
		t.Errorf("awaiting = %q", awaiting)
	}
	if got := edge.writes(); len(got) != 0 {
		t.Errorf("writes = %q during a restore", got)
	}
	if identity.calls != 0 {
		t.Errorf("the identity was ensured %d times during a restore", identity.calls)
	}
}

// unreadableReader fails every read.
type unreadableReader struct{ client.Reader }

func (unreadableReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unavailable")
}

func TestConnectorsConditionNamesWhatWaitsForILM(t *testing.T) {
	ready := []v1alpha1.ConnectorState{{Name: "a", State: connectorStateReady}, {Name: "b", State: connectorStateReady}, {Name: "c", State: connectorStateReady}}
	cases := []struct {
		name     string
		states   []v1alpha1.ConnectorState
		awaiting []string
		status   metav1.ConditionStatus
		reason   string
		message  string
	}{
		{"waiting for ILM", ready, []string{"a", "b"}, metav1.ConditionFalse, reasonProgressing, "waiting for ILM to register a, b"},
		{"nothing waits", ready, nil, metav1.ConditionTrue, reasonReady, "all chosen connectors are ready"},
		{
			"a starting connector comes first",
			[]v1alpha1.ConnectorState{{Name: "a", State: connectorStateReady}, {Name: "c", State: connectorStateWaiting}},
			[]string{"a"}, metav1.ConditionFalse, reasonProgressing, "waiting for these connectors: c",
		},
		{
			"a failed connector comes first",
			[]v1alpha1.ConnectorState{{Name: "a", State: connectorStateReady}, {Name: "c", State: connectorStateFailed, Problem: "p"}},
			[]string{"a"}, metav1.ConditionFalse, reasonFailed, "these connectors failed: c (p)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &v1alpha1.Appliance{}
			setConnectorsCondition(a, tc.states, tc.awaiting)
			c := condition(t, a, v1alpha1.ConditionConnectorsReady)
			if c.Status != tc.status || c.Reason != tc.reason || c.Message != tc.message {
				t.Errorf("ConnectorsReady = %s/%s %q, want %s/%s %q", c.Status, c.Reason, c.Message, tc.status, tc.reason, tc.message)
			}
		})
	}
}

func TestV1ConnectorILMRefusesIsFailedNotWaiting(t *testing.T) {
	for status, refused := range map[int]bool{
		http.StatusConflict:            true,
		http.StatusUnprocessableEntity: true,
		http.StatusBadRequest:          true,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			edge := newFakeConnectorsEdge(t)
			edge.createStatus = status
			r, _, a := v1Fixture(t, edge, emailName)

			out := r.reconcileV1Registrations(context.Background(), a, running(emailName), nil, true)

			if got := slices.Equal(out.Refused, []string{emailName}); got != refused {
				t.Errorf("refused = %q, want refused %v", out.Refused, refused)
			}
			if got := slices.Equal(out.Awaiting, []string{emailName}); got == refused {
				t.Errorf("awaiting = %q, want waiting %v", out.Awaiting, !refused)
			}
		})
	}
}

func TestV1ConnectorILMRefusesToReconnectIsFailedNotWaiting(t *testing.T) {
	for status, refused := range map[int]bool{
		http.StatusUnprocessableEntity: true,
		http.StatusBadRequest:          true,
		http.StatusInternalServerError: false,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			edge := newFakeConnectorsEdge(t, ilmclient.Connector{UUID: "u1", Name: "x", URL: v1URL(t, emailName), Status: "failed"})
			edge.reconnectStatus = status
			r, _, a := v1Fixture(t, edge, emailName)

			out := r.reconcileV1Registrations(context.Background(), a, running(emailName), nil, true)

			if got := slices.Equal(out.Refused, []string{emailName}); got != refused {
				t.Errorf("refused = %q, want refused %v", out.Refused, refused)
			}
			if got := slices.Equal(out.Awaiting, []string{emailName}); got == refused {
				t.Errorf("awaiting = %q, want waiting %v", out.Awaiting, !refused)
			}
		})
	}
}

func TestRefusedConnectorShowsTheRegistrationProblem(t *testing.T) {
	states := []v1alpha1.ConnectorState{{Name: "a", State: connectorStateReady}, {Name: "b", State: connectorStateReady}}
	got := withRegistrations(states, registrationOutcome{Refused: []string{"a"}, Awaiting: []string{"b"}})
	want := []v1alpha1.ConnectorState{{Name: "a", State: connectorStateFailed, Problem: problemRegistration}, {Name: "b", State: connectorStateReady}}
	if !slices.Equal(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	a := &v1alpha1.Appliance{}
	setConnectorsCondition(a, got, []string{"b"})
	if c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionConnectorsReady); c.Message != "these connectors failed: a ("+problemRegistration+")" {
		t.Errorf("message = %q", c.Message)
	}
}

func TestV1RegistrationPassIsBounded(t *testing.T) {
	edge := newFakeConnectorsEdge(t)
	edge.delay = 500 * time.Millisecond
	r, _, a := v1Fixture(t, edge, complianceName)
	old := ilmPassTimeout
	ilmPassTimeout = 50 * time.Millisecond
	t.Cleanup(func() { ilmPassTimeout = old })

	start := time.Now()
	out := r.reconcileV1Registrations(context.Background(), a, running(complianceName), nil, true)

	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("the pass took %v, want it cut at the timeout", time.Since(start))
	}
	if !slices.Equal(out.Awaiting, []string{complianceName}) || len(out.Refused) != 0 {
		t.Errorf("outcome = %+v, want the connector waiting", out)
	}
}

func TestOfflineV2ConnectorILMReachesIsReachable(t *testing.T) {
	const v2Name = "ejbca-ng-connector"
	listed := func(status string) []ilmclient.Connector {
		return []ilmclient.Connector{{UUID: "v2", Name: "ejbca", URL: v1URL(t, v2Name), Status: status}}
	}
	cases := map[string]struct {
		listed []ilmclient.Connector
		ready  bool
		reach  bool
	}{
		"connected":          {listed("connected"), true, true},
		"waiting approval":   {listed("waitingForApproval"), true, true},
		"offline":            {listed("offline"), true, false},
		"failed":             {listed("failed"), true, false},
		"absent":             {nil, true, false},
		"platform not ready": {listed("connected"), false, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			edge := newFakeConnectorsEdge(t, tc.listed...)
			r, _, a := v1Fixture(t, edge, v2Name)

			out := r.reconcileV1Registrations(context.Background(), a, nil, []string{v2Name}, tc.ready)

			if got := slices.Equal(out.Reachable, []string{v2Name}); got != tc.reach {
				t.Errorf("reachable = %q, want reachable %v", out.Reachable, tc.reach)
			}
			if len(edge.writes()) != 0 {
				t.Errorf("writes = %q, want none", edge.writes())
			}
		})
	}
}

func TestOfflineConnectorsAreTheOnesFailedAsUnreachable(t *testing.T) {
	states := []v1alpha1.ConnectorState{
		{Name: "a", State: connectorStateFailed, Problem: problemUnreachable},
		{Name: "b", State: connectorStateFailed, Problem: problemCrashLoop},
		{Name: "c", State: connectorStateReady},
	}
	if got := offlineConnectors(states); !slices.Equal(got, []string{"a"}) {
		t.Errorf("offline = %q", got)
	}
	got := withRegistrations(states, registrationOutcome{Reachable: []string{"a"}})
	if got[0] != (v1alpha1.ConnectorState{Name: "a", State: connectorStateReady}) {
		t.Errorf("state = %v, want ready without a problem", got[0])
	}
}
