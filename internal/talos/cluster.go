package talos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	cosiapi "github.com/cosi-project/runtime/api/v1alpha1"
	"github.com/cosi-project/runtime/pkg/state"
	cosiclient "github.com/cosi-project/runtime/pkg/state/protobuf/client"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Cluster is the Talos API of every node of the cluster, reached through the apid of the local node.
type Cluster interface {
	// Local is the node apid runs on.
	Local() Node
	// Node is the node with the host name name; each call resolves its address.
	Node(name string) Node
	// EtcdMembers lists the etcd members as the local node's etcd sees them, each with the health its own node
	// reports.
	EtcdMembers(ctx context.Context) ([]EtcdMember, error)
	// RemoveEtcdMember removes the etcd member id through the local node.
	RemoveEtcdMember(ctx context.Context, id uint64) error
	// Reset wipes the STATE partition, which holds the machine configuration, and the EPHEMERAL partition of the
	// node with the host name node and reboots it into maintenance mode. The reset is forced: the node does not leave
	// etcd itself.
	Reset(ctx context.Context, node string) error
	// SecretsBundle reads the cluster's secrets from the local node's machine configuration.
	SecretsBundle(ctx context.Context) (*secrets.Bundle, error)
}

// EtcdMember is a member of the cluster's etcd.
type EtcdMember struct {
	ID       uint64
	HostName string
	Learner  bool
	// Healthy reports that the member's node answered with a leader and without errors.
	Healthy bool
}

// Resolver returns the address the Talos API reaches the node with the host name name at.
type Resolver func(ctx context.Context, name string) (string, error)

// memberCheckTimeout bounds the health check of one etcd member, so that a node that is down does not hold up the
// list.
const memberCheckTimeout = 5 * time.Second

type cluster struct {
	local   *node
	resolve Resolver
}

// NewCluster returns the cluster reached through local, a node connected with DialAPI; resolve gives the address of
// a node by its host name.
func NewCluster(local Node, resolve Resolver) (Cluster, error) {
	n, ok := local.(*node)
	if !ok {
		return nil, errors.New("the local node has no Talos API connection")
	}
	return &cluster{local: n, resolve: resolve}, nil
}

func (c *cluster) Local() Node { return c.local }

func (c *cluster) Node(name string) Node {
	return c.local.targeted(func(ctx context.Context) (string, error) {
		addr, err := c.resolve(ctx, name)
		if err != nil {
			return "", fmt.Errorf("address of node %s: %w", name, err)
		}
		return addr, nil
	})
}

// at returns the node at address addr.
func (c *cluster) at(addr string) *node {
	return c.local.targeted(func(context.Context) (string, error) { return addr, nil })
}

func (c *cluster) EtcdMembers(ctx context.Context) ([]EtcdMember, error) {
	resp, err := c.local.c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{QueryLocal: true})
	if err != nil {
		return nil, fmt.Errorf("list etcd members: %w", err)
	}
	var members []EtcdMember
	for _, msg := range resp.GetMessages() {
		for _, m := range msg.GetMembers() {
			members = append(members, EtcdMember{
				ID:       m.GetId(),
				HostName: m.GetHostname(),
				Learner:  m.GetIsLearner(),
				Healthy:  c.memberHealthy(ctx, m),
			})
		}
	}
	return members, nil
}

// memberHealthy asks the node of member m, at the host of its first peer URL, for the status of its etcd.
func (c *cluster) memberHealthy(ctx context.Context, m *machineapi.EtcdMember) bool {
	if len(m.GetPeerUrls()) == 0 {
		return false
	}
	peer, err := url.Parse(m.GetPeerUrls()[0])
	if err != nil || peer.Hostname() == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, memberCheckTimeout)
	defer cancel()
	resp, err := c.at(peer.Hostname()).c.EtcdStatus(ctx)
	if err != nil {
		slog.Debug("etcd member did not answer", "member", m.GetHostname(), "error", err)
		return false
	}
	for _, s := range resp.GetMessages() {
		st := s.GetMemberStatus()
		if st.GetLeader() == 0 || len(st.GetErrors()) > 0 {
			return false
		}
	}
	return len(resp.GetMessages()) > 0
}

func (c *cluster) RemoveEtcdMember(ctx context.Context, id uint64) error {
	if err := c.local.c.EtcdRemoveMemberByID(ctx, &machineapi.EtcdRemoveMemberByIDRequest{MemberId: id}); err != nil {
		return fmt.Errorf("remove etcd member %x: %w", id, err)
	}
	slog.Info("removed etcd member", "member", fmt.Sprintf("%x", id))
	return nil
}

func (c *cluster) Reset(ctx context.Context, name string) error {
	n := c.Node(name).(*node)
	err := n.c.ResetGeneric(ctx, &machineapi.ResetRequest{
		Graceful: false,
		Reboot:   true,
		Mode:     machineapi.ResetRequest_ALL,
		SystemPartitionsToWipe: []*machineapi.ResetPartitionSpec{
			{Label: constants.StatePartitionLabel, Wipe: true},
			{Label: constants.EphemeralPartitionLabel, Wipe: true},
		},
	})
	if err != nil {
		return fmt.Errorf("reset node %s: %w", name, err)
	}
	slog.Info("reset node", "node", name)
	return nil
}

func (c *cluster) SecretsBundle(ctx context.Context) (*secrets.Bundle, error) {
	cfg, err := c.local.machineConfig(ctx)
	if err != nil {
		return nil, err
	}
	bundle, err := secrets.NewBundleFromConfig(secrets.NewFixedClock(time.Now()), cfg)
	if err != nil {
		return nil, fmt.Errorf("read the secrets bundle: %w", err)
	}
	return bundle, nil
}

// targeted returns a node whose calls go over the connection of n to the node at the address target gives, which
// apid forwards them to.
func (n *node) targeted(target func(context.Context) (string, error)) *node {
	cc := &targetConn{cc: n.c.Conn(), target: target}
	c := &client.Client{
		MachineClient:   machineapi.NewMachineServiceClient(cc),
		ImageClient:     machineapi.NewImageServiceClient(cc),
		LifecycleClient: machineapi.NewLifecycleServiceClient(cc),
		COSI:            state.WrapCore(cosiclient.NewAdapter(cosiapi.NewStateClient(cc))),
	}
	return &node{c: c, st: c.COSI}
}

// targetConn addresses every call to the node at the address target gives.
type targetConn struct {
	cc     grpc.ClientConnInterface
	target func(context.Context) (string, error)
}

func (t *targetConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	ctx, err := t.withTarget(ctx)
	if err != nil {
		return err
	}
	return t.cc.Invoke(ctx, method, args, reply, opts...)
}

func (t *targetConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, err := t.withTarget(ctx)
	if err != nil {
		return nil, err
	}
	return t.cc.NewStream(ctx, desc, method, opts...)
}

// withTarget adds the target node and the runtime metadata the Talos client sends with every call.
func (t *targetConn) withTarget(ctx context.Context) (context.Context, error) {
	addr, err := t.target(ctx)
	if err != nil {
		return nil, err
	}
	return client.WithNode(metadata.AppendToOutgoingContext(ctx, "runtime", "Talos"), addr), nil
}
