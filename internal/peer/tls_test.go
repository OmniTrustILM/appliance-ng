package peer

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
)

func TestPeerRejectsNonVotingCertificate(t *testing.T) {
	ca := newCA(t)
	seen := make(chan recorded, 8)
	srv, port := leaderServer(t, ca, recordingAPI(seen), always)

	get := func(id *Identity, path string) (int, error) {
		c := &http.Client{Transport: forwarder(id, port).transport}
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	voting := identity(t, followerNode, ca.secret(t, followerNode, CommonName, "127.0.0.1"))
	if code, err := get(voting, "/appliance/api/v1/status"); err != nil || code != http.StatusCreated {
		t.Fatalf("a voting node's peer: %d, %v", code, err)
	}
	<-seen
	if code, err := get(voting, "/appliance/"); err != nil || code != http.StatusNotFound {
		t.Errorf("the peer listener serves the console's files: %d, %v", code, err)
	}

	for name, secret := range map[string]*corev1.Secret{
		"worker":                       ca.secret(t, followerNode, CommonName, workerIP),
		"voting address, other source": ca.secret(t, followerNode, CommonName, followerIP),
		"other CA":                     newCA(t).secret(t, followerNode, CommonName, "127.0.0.1"),
		"not a peer":                   ca.secret(t, followerNode, "ilm.example.com", "127.0.0.1"),
		"no IP SAN":                    ca.secret(t, followerNode, CommonName),
		"other CA roots":               withRoots(ca.secret(t, followerNode, CommonName, "127.0.0.1"), newCA(t).pem),
	} {
		t.Run(name, func(t *testing.T) {
			if code, err := get(identity(t, followerNode, secret), "/appliance/api/v1/status"); err == nil {
				t.Errorf("answered %d", code)
			}
		})
	}
	t.Run("source not a voting node", func(t *testing.T) {
		srv, port := serveLeader(t, ca, recordingAPI(seen), always, "10.0.1.11", followerIP)
		c := &http.Client{Transport: forwarder(voting, port).transport}
		if resp, err := c.Get(srv.URL + "/appliance/api/v1/status"); err == nil {
			_ = resp.Body.Close()
			t.Errorf("answered %d", resp.StatusCode)
		}
	})
	t.Run("no client certificate", func(t *testing.T) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test checks the server's refusal
		if resp, err := c.Get(srv.URL + "/appliance/api/v1/status"); err == nil {
			_ = resp.Body.Close()
			t.Errorf("answered %d", resp.StatusCode)
		}
	})
	t.Run("not issued", func(t *testing.T) {
		id := NewIdentity(quiet, fakeReader(t), followerNode)
		if err := id.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := get(id, "/appliance/api/v1/status"); !errors.Is(err, errNotIssued) {
			t.Errorf("error %v, want %v", err, errNotIssued)
		}
		unissued := NewIdentity(quiet, fakeReader(t), leaderNode).ServerConfig(VotingAddresses(fakeReader(t)))
		if _, err := unissued.GetConfigForClient(&tls.ClientHelloInfo{}); !errors.Is(err, errNotIssued) {
			t.Errorf("server without a certificate: %v", err)
		}
	})
	select {
	case r := <-seen:
		t.Errorf("a refused peer reached the API: %s %s", r.method, r.path)
	default:
	}
}

func withRoots(s *corev1.Secret, roots []byte) *corev1.Secret {
	s.Data[caKey] = roots
	return s
}

func TestIdentityRefresh(t *testing.T) {
	ca := newCA(t)
	bad := ca.secret(t, followerHost, CommonName, followerIP)
	bad.Data[corev1.TLSPrivateKeyKey] = []byte("garbage")
	noRoots := withRoots(ca.secret(t, followerHost, CommonName, followerIP), []byte("garbage"))
	for name, secret := range map[string]*corev1.Secret{"bad key": bad, "bad roots": noRoots} {
		t.Run(name, func(t *testing.T) {
			id := NewIdentity(quiet, fakeReader(t, secret), followerHost)
			err := id.Refresh(context.Background())
			if err == nil || strings.Contains(err.Error(), "garbage") {
				t.Errorf("error %v", err)
			}
		})
	}
	t.Run("run until cancelled", func(t *testing.T) {
		id := NewIdentity(quiet, fakeReader(t, bad), followerHost)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := id.Run(ctx, 1); err != nil || id.cert.Load() != nil {
			t.Errorf("run: %v, loaded %v", err, id.cert.Load() != nil)
		}
	})
}

func TestSecretName(t *testing.T) {
	a, b := SecretName("ilm.example.com"), SecretName("ilm.example.org")
	if a == b || !strings.HasPrefix(a, "appliance-peer-ilm-") {
		t.Errorf("names %q, %q", a, b)
	}
	if long := SecretName(strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + ".example.com"); len(long) > 253 {
		t.Errorf("name of %d characters", len(long))
	}
}

func TestVotingNodes(t *testing.T) {
	a := clusterAppliance()
	a.Spec.Cluster.Nodes = append(a.Spec.Cluster.Nodes,
		v1alpha1.ClusterNode{HostName: "dhcp.example.com", Role: v1alpha1.NodeRoleVoting},
		v1alpha1.ClusterNode{HostName: "joining.example.com", Address: "10.0.1.40/24", Role: v1alpha1.NodeRoleVoting})
	objs := append(kubeNodes(), a, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "dhcp"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.1.30"}}},
	})
	reader := fakeReader(t, objs...)
	nodes, err := VotingNodes(context.Background(), reader, a)
	if err != nil {
		t.Fatal(err)
	}
	want := []Node{
		{HostName: leaderHost, NodeName: leaderNode, Address: mustAddr("10.0.1.11")},
		{HostName: followerHost, NodeName: followerNode, Address: mustAddr(followerIP)},
		{HostName: "dhcp.example.com", NodeName: "dhcp", Address: mustAddr("10.0.1.30")},
	}
	if !slices.Equal(nodes, want) {
		t.Errorf("voting nodes %v, want %v", nodes, want)
	}
	addrs, err := VotingAddresses(reader)(context.Background())
	if err != nil || !slices.Equal(addrs, []netip.Addr{mustAddr("10.0.1.11"), mustAddr(followerIP), mustAddr("10.0.1.30")}) {
		t.Errorf("voting addresses %v, %v", addrs, err)
	}
	if _, err := VotingAddresses(fakeReader(t))(context.Background()); err == nil {
		t.Error("voting addresses without an Appliance")
	}

	// A node registered under a name unlike its host name is found by its address.
	renamed := fakeReader(t, a, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "talos-x1"},
		Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: followerIP}}},
	})
	if nodes, err := VotingNodes(context.Background(), renamed, a); err != nil || len(nodes) != 1 || nodes[0].NodeName != "talos-x1" {
		t.Errorf("voting nodes %v, %v", nodes, err)
	}
}

func TestLeader(t *testing.T) {
	ctx := context.Background()
	reader := fakeReader(t, append(kubeNodes(), clusterAppliance(), lease(HolderIdentity(followerNode)))...)
	name, err := Leader(ctx, reader)
	if err != nil || name != followerNode {
		t.Errorf("leader %q, %v", name, err)
	}
	if addr, err := LeaderAddress(reader, leaderNode)(ctx); err != nil || addr != followerIP {
		t.Errorf("leader address %q, %v", addr, err)
	}
	if _, err := LeaderAddress(reader, followerNode)(ctx); !errors.Is(err, ErrNoLeader) {
		t.Errorf("leader address on the holder's own node: %v", err)
	}
	for name, objs := range map[string][]client.Object{
		"no lease":       {clusterAppliance()},
		"released":       {clusterAppliance(), lease("")},
		"foreign holder": {clusterAppliance(), lease("someone")},
	} {
		t.Run(name, func(t *testing.T) {
			r := fakeReader(t, objs...)
			if _, err := LeaderAddress(r, leaderNode)(ctx); !errors.Is(err, ErrNoLeader) {
				t.Errorf("error %v, want %v", err, ErrNoLeader)
			}
		})
	}
	for name, holder := range map[string]string{"not registered": "gone", "not voting": "worker"} {
		t.Run(name, func(t *testing.T) {
			r := fakeReader(t, append(kubeNodes(), clusterAppliance(), lease(HolderIdentity(holder)))...)
			if _, err := LeaderAddress(r, leaderNode)(ctx); err == nil || errors.Is(err, ErrNoLeader) {
				t.Errorf("error %v", err)
			}
		})
	}
	t.Run("no Appliance", func(t *testing.T) {
		r := fakeReader(t, lease(HolderIdentity(followerNode)))
		if _, err := LeaderAddress(r, leaderNode)(ctx); err == nil {
			t.Error("no error")
		}
	})
}
