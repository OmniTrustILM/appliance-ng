package certs

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

func TestHosts(t *testing.T) {
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{HostName: "ilm.example.com"}}}}}
	if got := Hosts(a); !reflect.DeepEqual(got, []string{"ilm.example.com"}) {
		t.Errorf("hosts %v without a platform", got)
	}
	a.Spec.Platform = &v1alpha1.PlatformSpec{HostName: "ilm.example.com"}
	if got := Hosts(a); !reflect.DeepEqual(got, []string{"ilm.example.com"}) {
		t.Errorf("hosts %v, want the shared name once", got)
	}
	a.Spec.Platform.HostName = "pki.example.com"
	if got := Hosts(a); !reflect.DeepEqual(got, []string{"ilm.example.com", "pki.example.com"}) {
		t.Errorf("hosts %v, want both names", got)
	}
}

func TestAddresses(t *testing.T) {
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{
		{HostName: "a.example.com", Address: "192.0.2.20/24"},
		{HostName: "b.example.com", Address: "192.0.2.10/24"},
		{HostName: "c.example.com"},
	}}}}
	got := Addresses(a, netip.MustParseAddr("192.0.2.10"))
	want := []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.20")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("addresses %v, want %v", got, want)
	}
}

func TestParseAddress(t *testing.T) {
	for in, want := range map[string]string{"192.0.2.1/24": "192.0.2.1", "192.0.2.2": "192.0.2.2", "bogus": "", "": "", "fe80::1": "", "169.254.0.9/16": ""} {
		got, ok := ParseAddress(in)
		if (want == "") == ok || (ok && got.String() != want) {
			t.Errorf("ParseAddress(%q) = %v, %v; want %q", in, got, ok, want)
		}
	}
}

func TestNodeAddress(t *testing.T) {
	ctx := context.Background()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: "192.0.2.99"},
			{Type: corev1.NodeInternalIP, Address: "fe80::1"},
			{Type: corev1.NodeInternalIP, Address: "192.0.2.30"},
		}},
	}
	c := fake.NewClientBuilder().WithObjects(node).Build()
	a := &v1alpha1.Appliance{Spec: v1alpha1.ApplianceSpec{Cluster: v1alpha1.ClusterSpec{Nodes: []v1alpha1.ClusterNode{{}}}}}
	for _, tc := range []struct{ status, spec, want string }{
		{"192.0.2.10", "192.0.2.20/24", "192.0.2.10"},
		{"", "192.0.2.20/24", "192.0.2.20"},
		{"", "", "192.0.2.30"},
	} {
		a.Status.NodeAddress, a.Spec.FirstNode().Address = tc.status, tc.spec
		got, ok, err := NodeAddress(ctx, c, a)
		if err != nil || !ok || got.String() != tc.want {
			t.Errorf("NodeAddress(status %q, spec %q) = %v, %v, %v; want %s", tc.status, tc.spec, got, ok, err, tc.want)
		}
	}
	a.Status.NodeAddress, a.Spec.FirstNode().Address = "", ""
	if _, ok, err := NodeAddress(ctx, fake.NewClientBuilder().Build(), a); ok || err != nil {
		t.Errorf("found an address without nodes: %v, %v", ok, err)
	}
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
	if _, _, err := NodeAddress(ctx, failing, a); err == nil {
		t.Error("a get error was not reported")
	}
}

func TestNodeAddressReadsTheManagersNode(t *testing.T) {
	withAddress := func(name, address string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}},
		}
	}
	c := fake.NewClientBuilder().WithObjects(withAddress("aaa-stale", "192.0.2.40"), withAddress("ilm", "192.0.2.50")).Build()
	t.Setenv(kube.NodeNameEnv, "ilm")
	got, ok, err := NodeAddress(context.Background(), c, &v1alpha1.Appliance{})
	if err != nil || !ok || got.String() != "192.0.2.50" {
		t.Errorf("NodeAddress = %v, %v, %v; want the address of node ilm", got, ok, err)
	}
}

func TestUncoveredHost(t *testing.T) {
	f := newChain(t)
	for _, tc := range []struct {
		name  string
		cert  []byte
		hosts []string
		want  string
	}{
		{"covered", f.valid.cert, uploadHosts, ""},
		{"one missing", f.valid.cert, []string{uploadHosts[0], "pki.example.com"}, "pki.example.com"},
		{"not a certificate", []byte("garbage"), []string{"pki.example.com"}, ""},
		{"unparsable block", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), []string{"pki.example.com"}, ""},
	} {
		if got := UncoveredHost(tc.cert, tc.hosts); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
