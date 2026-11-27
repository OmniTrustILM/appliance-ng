// Package peer connects the managers of the voting nodes. The manager holding the leader election Lease does all the
// work; the others forward the console's API requests to it over mutual TLS, each with its node's certificate from
// the built-in CA.
package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/OmniTrustILM/appliance-ng/api/v1alpha1"
	"github.com/OmniTrustILM/appliance-ng/internal/certs"
	"github.com/OmniTrustILM/appliance-ng/internal/kube"
)

const (
	// Port is the peer listener's port on every voting node's address.
	Port = 8445
	// LeaseName is the leader election Lease in the system namespace.
	LeaseName = "appliance-manager"
	// CommonName is the common name of every peer certificate; certificates the built-in CA issues for anything else
	// do not have it.
	CommonName = "appliance-peer"
	// caKey is where cert-manager puts the issuing CA's certificate in the peer Secret.
	caKey = "ca.crt"
	// verifyTimeout bounds the read of the voting nodes while a peer connects.
	verifyTimeout = 5 * time.Second
)

var (
	// ErrNoLeader means no manager holds the Lease, or the holder is this node's own manager that no longer leads.
	ErrNoLeader  = errors.New("no manager leads")
	errNotIssued = errors.New("the peer certificate is not issued yet")
)

// SecretName returns the name of the TLS Secret, in the system namespace, of the peer certificate of the Kubernetes
// node node: its first label and a digest of the whole name, so that it stays short and distinct.
func SecretName(node string) string {
	label, _, _ := strings.Cut(node, ".")
	sum := sha256.Sum256([]byte(node))
	return "appliance-peer-" + label + "-" + hex.EncodeToString(sum[:4])
}

// HolderIdentity returns a new leader election identity of the manager on the node node: the node name and a random
// suffix, so that a restarted manager is told apart from its predecessor.
func HolderIdentity(node string) string {
	return node + "_" + string(uuid.NewUUID())
}

// Leader returns the host name of the node whose manager holds the Lease, read with reader, or ErrNoLeader.
func Leader(ctx context.Context, reader client.Reader) (string, error) {
	lease := &coordinationv1.Lease{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: LeaseName}, lease)
	switch {
	case apierrors.IsNotFound(err):
		return "", ErrNoLeader
	case err != nil:
		return "", fmt.Errorf("read the leader election Lease: %w", err)
	}
	if lease.Spec.HolderIdentity == nil {
		return "", ErrNoLeader
	}
	node, _, found := strings.Cut(*lease.Spec.HolderIdentity, "_")
	if !found || node == "" {
		return "", ErrNoLeader
	}
	return node, nil
}

// LeaderAddress returns a function that returns the address of the leader's voting node, read with reader. The
// manager on the node self asks only while it does not lead, so the Lease still naming self means no manager leads
// yet.
func LeaderAddress(reader client.Reader, self string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name, err := Leader(ctx, reader)
		if err != nil {
			return "", err
		}
		if name == self {
			return "", ErrNoLeader
		}
		nodes, err := readVotingNodes(ctx, reader)
		if err != nil {
			return "", err
		}
		for _, n := range nodes {
			if n.NodeName == name {
				return n.Address.String(), nil
			}
		}
		return "", fmt.Errorf("the leader's node %s is not a voting node with an address", name)
	}
}

// Node is a voting node: its host name in the spec, the name of its Kubernetes node, which keys its peer Secret, and
// the address its peer certificate covers.
type Node struct {
	HostName string
	NodeName string
	Address  netip.Addr
}

// VotingNodes returns the voting nodes of a that have a Kubernetes node, read with reader, and an address: the
// address in the spec, with DHCP the internal address of the Kubernetes node.
func VotingNodes(ctx context.Context, reader client.Reader, a *v1alpha1.Appliance) ([]Node, error) {
	list := &corev1.NodeList{}
	if err := reader.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list the nodes: %w", err)
	}
	var nodes []Node
	for _, n := range a.Spec.Cluster.Nodes {
		if n.Role != v1alpha1.NodeRoleVoting {
			continue
		}
		kn := kubeNode(list.Items, n)
		if kn == nil {
			continue
		}
		addr, ok := certs.ParseAddress(n.Address)
		if !ok {
			addr, ok = certs.InternalAddress(kn)
		}
		if ok {
			nodes = append(nodes, Node{HostName: n.HostName, NodeName: kn.Name, Address: addr})
		}
	}
	return nodes, nil
}

// NodeEntry returns the entry of a's cluster nodes that kn is, matched as VotingNodes matches them, or nil when there
// is none.
func NodeEntry(a *v1alpha1.Appliance, kn *corev1.Node) *v1alpha1.ClusterNode {
	for i := range a.Spec.Cluster.Nodes {
		if kubeNode([]corev1.Node{*kn}, a.Spec.Cluster.Nodes[i]) != nil {
			return &a.Spec.Cluster.Nodes[i]
		}
	}
	return nil
}

// kubeNode returns the Kubernetes node of the cluster node n: the one with n's address, or else the one named n's host
// name or its first label, as Talos registers the short host name.
func kubeNode(nodes []corev1.Node, n v1alpha1.ClusterNode) *corev1.Node {
	if addr, ok := certs.ParseAddress(n.Address); ok {
		for i := range nodes {
			if internal, ok := certs.InternalAddress(&nodes[i]); ok && internal == addr {
				return &nodes[i]
			}
		}
	}
	label, _, _ := strings.Cut(n.HostName, ".")
	for _, name := range []string{n.HostName, label} {
		for i := range nodes {
			if nodes[i].Name == name {
				return &nodes[i]
			}
		}
	}
	return nil
}

// readVotingNodes returns the voting nodes of the Appliance, read with reader.
func readVotingNodes(ctx context.Context, reader client.Reader) ([]Node, error) {
	a := &v1alpha1.Appliance{}
	if err := reader.Get(ctx, client.ObjectKey{Name: v1alpha1.ApplianceName}, a); err != nil {
		return nil, fmt.Errorf("read the Appliance: %w", err)
	}
	return VotingNodes(ctx, reader, a)
}

// VotingAddresses returns a function that returns the addresses of the voting nodes, read with reader.
func VotingAddresses(reader client.Reader) func(context.Context) ([]netip.Addr, error) {
	return func(ctx context.Context) ([]netip.Addr, error) {
		nodes, err := readVotingNodes(ctx, reader)
		if err != nil {
			return nil, err
		}
		addrs := make([]netip.Addr, 0, len(nodes))
		for _, n := range nodes {
			addrs = append(addrs, n.Address)
		}
		return addrs, nil
	}
}

// Identity is this node's peer certificate and the CA it trusts, loaded from the node's peer Secret, which the
// leader requests from cert-manager. Run picks up a renewed certificate. The private key stays in memory.
type Identity struct {
	log    *slog.Logger
	reader client.Reader
	secret string

	// dialTimeout bounds the connection to a peer and its TLS handshake: a paused manager still accepts connections.
	dialTimeout time.Duration

	cert   atomic.Pointer[tls.Certificate]
	roots  atomic.Pointer[x509.CertPool]
	loaded []byte
}

// NewIdentity returns the peer identity of the Kubernetes node node, read with reader; it has no certificate until
// Refresh finds the node's peer Secret.
func NewIdentity(log *slog.Logger, reader client.Reader, node string) *Identity {
	return &Identity{log: log, reader: reader, secret: SecretName(node), dialTimeout: verifyTimeout}
}

// Refresh loads the node's peer Secret. Until it exists, the identity stays as it is.
func (i *Identity) Refresh(ctx context.Context) error {
	s := &corev1.Secret{}
	err := i.reader.Get(ctx, client.ObjectKey{Namespace: kube.SystemNamespace, Name: i.secret}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("read the peer Secret %s: %w", i.secret, err)
	}
	if bytes.Equal(s.Data[corev1.TLSCertKey], i.loaded) {
		return nil
	}
	cert, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return fmt.Errorf("the peer Secret %s holds no valid certificate and key", i.secret)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(s.Data[caKey]) {
		return fmt.Errorf("the peer Secret %s holds no CA certificate", i.secret)
	}
	i.cert.Store(&cert)
	i.roots.Store(roots)
	i.loaded = s.Data[corev1.TLSCertKey]
	i.log.Info("peer certificate loaded", "secret", i.secret)
	return nil
}

// Run refreshes the identity now and then every interval every until ctx ends; a failure is logged.
func (i *Identity) Run(ctx context.Context, every time.Duration) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := i.Refresh(ctx); err != nil {
			i.log.Warn("could not load the peer certificate", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ServerConfig returns the TLS configuration of the peer listener: it serves the node's peer certificate and accepts
// only a peer certificate of the same CA for the address the peer connects from, which voters must return.
func (i *Identity) ServerConfig(voters func(context.Context) ([]netip.Addr, error)) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			cert, roots := i.cert.Load(), i.roots.Load()
			if cert == nil {
				return nil, errNotIssued
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    roots,
				VerifyConnection: func(cs tls.ConnectionState) error {
					ctx, cancel := context.WithTimeout(hello.Context(), verifyTimeout)
					defer cancel()
					return verifyVoter(ctx, cs.PeerCertificates[0], remoteAddr(hello.Conn), voters)
				},
			}, nil
		},
	}
}

// remoteAddr returns the address conn comes from; the zero address when it is not an IP connection.
func remoteAddr(conn net.Conn) netip.Addr {
	if conn == nil {
		return netip.Addr{}
	}
	addrPort, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return addrPort.Addr().Unmap()
}

// verifyVoter accepts a verified peer certificate leaf for remote, the address the peer connects from, when remote is
// a voting node's address.
func verifyVoter(ctx context.Context, leaf *x509.Certificate, remote netip.Addr, voters func(context.Context) ([]netip.Addr, error)) error {
	if err := verifyPeer(leaf); err != nil {
		return err
	}
	if !slices.ContainsFunc(leaf.IPAddresses, func(ip net.IP) bool {
		addr, ok := netip.AddrFromSlice(ip)
		return ok && addr.Unmap() == remote
	}) {
		return fmt.Errorf("the peer certificate for %v does not cover %s, where the peer connects from", leaf.IPAddresses, remote)
	}
	addrs, err := voters(ctx)
	if err != nil {
		return fmt.Errorf("read the voting nodes: %w", err)
	}
	if !slices.Contains(addrs, remote) {
		return fmt.Errorf("the peer at %s is not a voting node", remote)
	}
	return nil
}

// verifyPeer accepts a verified certificate leaf only when it is a peer certificate.
func verifyPeer(leaf *x509.Certificate) error {
	if leaf.Subject.CommonName != CommonName {
		return fmt.Errorf("the certificate of %q is not a peer certificate", leaf.Subject.CommonName)
	}
	return nil
}

// dialTLS connects to the peer listener at addr, presenting the node's peer certificate and accepting only a peer
// certificate of the same CA for the address dialled.
func (i *Identity) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	cert, roots := i.cert.Load(), i.roots.Load()
	if cert == nil {
		return nil, errNotIssued
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, i.dialTimeout)
	defer cancel()
	d := &tls.Dialer{
		Config: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: host,
			RootCAs:    roots,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return cert, nil
			},
			VerifyConnection: func(cs tls.ConnectionState) error { return verifyPeer(cs.PeerCertificates[0]) },
		},
	}
	return d.DialContext(ctx, network, addr)
}
