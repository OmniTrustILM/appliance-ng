package api

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

// createStaticAppliance creates the Appliance with a static address and the settings first boot imported.
func createStaticAppliance(t *testing.T) *v1alpha1.Appliance {
	t.Helper()
	a := createAppliance(t, v1alpha1.SetupComplete)
	a.Spec.Cluster = v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{HostName: "appliance.example.com", Address: "192.0.2.10/24", Gateway: "192.0.2.1"}}}
	a.Spec.DNS.Servers = []string{"192.0.2.53"}
	a.Spec.Proxy = &v1alpha1.ProxySpec{HTTP: "http://proxy.example.com:3128"}
	if err := testClient.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func validNodeUpdate() NodeUpdate {
	return NodeUpdate{
		HostName: ptr(" Appliance.Example.com "),
		Gateway:  ptr("192.0.2.254"),
		Dns:      []string{"198.51.100.53", "2001:db8::53"},
		Ntp:      []string{"NTP.example.com"},
		Proxy:    &Proxy{Https: ptr("http://proxy.example.com:3128"), NoProxy: ptr(" .example.com ")},
	}
}

func TestPutNodeChangesTheNodeSettings(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createStaticAppliance(t)

	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", validNodeUpdate(), s), http.StatusAccepted)
	spec := getAppliance(t).Spec
	want := v1alpha1.ApplianceSpec{
		TLS:     v1alpha1.TLSSpec{Source: v1alpha1.TLSSourceBuiltin},
		Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{HostName: "appliance.example.com", Address: "192.0.2.10/24", Gateway: "192.0.2.254"}}},
		DNS:     v1alpha1.DNSSpec{Servers: []string{"198.51.100.53", "2001:db8::53"}},
		NTP:     v1alpha1.NTPSpec{Servers: []string{"ntp.example.com"}},
		Proxy:   &v1alpha1.ProxySpec{HTTPS: "http://proxy.example.com:3128", NoProxy: ".example.com"},
	}
	if !equality.Semantic.DeepEqual(spec, want) {
		t.Errorf("spec %+v, want %+v", spec, want)
	}
	entries := h.entries()
	last := entries[len(entries)-1]
	wantPaths := []string{"spec.cluster.nodes[0].gateway", "spec.dns.servers", "spec.ntp.servers", "spec.proxy"}
	if last.Action != string(AuditEntryActionNodeSettings) || last.Result != string(Success) || !slices.Equal(last.Paths, wantPaths) {
		t.Errorf("audit entry %+v, want node-settings success with paths %v", last, wantPaths)
	}

	// Without a proxy and an unchanged rest, only the proxy is removed; the host name may be left out.
	body := validNodeUpdate()
	body.Proxy, body.HostName = nil, nil
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", body, s), http.StatusAccepted)
	if got := getAppliance(t).Spec.Proxy; got != nil {
		t.Errorf("proxy %+v, want it removed", got)
	}
	entries = h.entries()
	if got := entries[len(entries)-1].Paths; !slices.Equal(got, []string{"spec.proxy"}) {
		t.Errorf("paths %v, want only the proxy", got)
	}

	// The same settings again change nothing.
	generation := getAppliance(t).Generation
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", body, s), http.StatusAccepted)
	if got := getAppliance(t).Generation; got != generation {
		t.Errorf("generation %d, want %d after an unchanged request", got, generation)
	}
}

func TestNodeAddressReadOnly(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createStaticAppliance(t)
	for _, address := range []string{"192.0.2.11/24", "192.0.2.10/24", ""} {
		body := validNodeUpdate()
		body.Address = ptr(address)
		rec := h.request(http.MethodPut, "/appliance/api/v1/node", body, s)
		wantStatus(t, rec, http.StatusBadRequest)
		var got Error
		decode(t, rec, &got)
		if got.Field == nil || *got.Field != "address" {
			t.Errorf("address %q: field %v, want address", address, got.Field)
		}
	}
	if got := getAppliance(t).Spec.FirstNode(); got.Address != "192.0.2.10/24" || got.HostName != "appliance.example.com" {
		t.Errorf("node %+v changed by a refused request", got)
	}
}

func TestPutNodeValidates(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createStaticAppliance(t)
	edit := func(change func(*NodeUpdate)) NodeUpdate {
		b := validNodeUpdate()
		change(&b)
		return b
	}
	for _, tc := range []struct {
		name  string
		body  NodeUpdate
		field string
	}{
		{"no gateway", edit(func(b *NodeUpdate) { b.Gateway = nil }), "gateway"},
		{"gateway outside", edit(func(b *NodeUpdate) { b.Gateway = ptr("198.51.100.1") }), "gateway"},
		{"no DNS", edit(func(b *NodeUpdate) { b.Dns = nil }), "dns"},
		{"DNS name", edit(func(b *NodeUpdate) { b.Dns = []string{"dns.example.com"} }), "dns"},
		{"NTP", edit(func(b *NodeUpdate) { b.Ntp = []string{"ntp_example"} }), "ntp"},
		{"proxy credentials", edit(func(b *NodeUpdate) { b.Proxy.Http = ptr("http://u:p@proxy.example.com") }), "proxy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.request(http.MethodPut, "/appliance/api/v1/node", tc.body, s)
			wantStatus(t, rec, http.StatusBadRequest)
			var got Error
			decode(t, rec, &got)
			if got.Field == nil || *got.Field != tc.field {
				t.Errorf("field = %v, want %s", got.Field, tc.field)
			}
		})
	}
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", "null", s), http.StatusBadRequest)
	if got := getAppliance(t).Spec.FirstNode().HostName; got != "appliance.example.com" {
		t.Errorf("host name %q changed by a refused request", got)
	}
}

func TestPutNodeWithDHCP(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createAppliance(t, v1alpha1.SetupComplete)
	body := validNodeUpdate()
	rec := h.request(http.MethodPut, "/appliance/api/v1/node", body, s)
	wantStatus(t, rec, http.StatusBadRequest)
	var got Error
	decode(t, rec, &got)
	if got.Field == nil || *got.Field != "gateway" {
		t.Errorf("field = %v, want gateway: a gateway needs a static address", got.Field)
	}
	body.Gateway = ptr("")
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", body, s), http.StatusAccepted)
	if a := getAppliance(t); a.Spec.FirstNode().Gateway != "" || a.Spec.FirstNode().HostName != "appliance.example.com" {
		t.Errorf("node %+v", a.Spec.FirstNode())
	}
}

func TestPutNodeBeforeTheAppliance(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	wantStatus(t, h.request(http.MethodPut, "/appliance/api/v1/node", validNodeUpdate(), s), http.StatusConflict)
}

func TestNodeHostNameReadOnly(t *testing.T) {
	h := newHarness(t)
	s := h.signIn()
	createStaticAppliance(t)
	for _, name := range []string{"pki.example.com", "ilm", ""} {
		body := validNodeUpdate()
		body.HostName = ptr(name)
		rec := h.request(http.MethodPut, "/appliance/api/v1/node", body, s)
		wantStatus(t, rec, http.StatusBadRequest)
		wantField(t, rec.Body.Bytes(), "hostName", "fixed after setup")
	}
	if got := getAppliance(t).Spec.FirstNode(); got.HostName != "appliance.example.com" || got.Gateway != "192.0.2.1" {
		t.Errorf("node %+v changed by a refused request", got)
	}
}
