// Package talos is a client for the local machined socket of a Talos node.
package talos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Stage is the coarse lifecycle stage of the node.
type Stage string

// Node stages.
const (
	StageMaintenance Stage = "maintenance"
	StageBooting     Stage = "booting"
	StageRunning     Stage = "running"
	StageInstalling  Stage = "installing"
	// StageOther covers every other stage: rebooting, upgrading, resetting, shutting down and unknown.
	StageOther Stage = "other"
)

// ErrAlreadyBootstrapped is returned by Bootstrap when etcd is already bootstrapped.
var ErrAlreadyBootstrapped = errors.New("etcd already bootstrapped")

// Node is the subset of the Talos API that the first-boot service needs.
type Node interface {
	Stage(ctx context.Context) (Stage, error)
	ApplyConfig(ctx context.Context, cfg []byte) error
	Bootstrap(ctx context.Context) error
	IsBootstrapped(ctx context.Context) (bool, error)
	Kubeconfig(ctx context.Context) ([]byte, error)
	Addresses(ctx context.Context) ([]netip.Prefix, error)
	// Configured reports whether the node holds a machine configuration (the active MachineConfig resource).
	Configured(ctx context.Context) (bool, error)
}

const (
	roleKey   = "talos-role"
	roleAdmin = "os:admin"
)

type node struct {
	c  *client.Client
	st state.CoreState
}

// Dial connects to the machined Unix socket. Every call carries the admin role metadata that apid
// would otherwise add; machined rejects the call without it.
func Dial(ctx context.Context, socket string) (Node, error) {
	c, err := client.New(ctx,
		client.WithUnixSocket(socket),
		client.WithGRPCDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithChainUnaryInterceptor(withRoleUnary),
			grpc.WithChainStreamInterceptor(withRoleStream),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to machined socket %s: %w", socket, err)
	}
	slog.Debug("connected to machined", "socket", socket)

	return &node{c: c, st: c.COSI}, nil
}

func withRoleUnary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	return invoker(withRole(ctx), method, req, reply, cc, opts...)
}

func withRoleStream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	return streamer(withRole(ctx), desc, cc, method, opts...)
}

func withRole(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, roleKey, roleAdmin)
}

// Close releases the connection.
func (n *node) Close() error { return n.c.Close() }

func (n *node) Stage(ctx context.Context) (Stage, error) {
	ms, err := safe.StateGetByID[*runtime.MachineStatus](ctx, n.st, runtime.MachineStatusID)
	if err != nil {
		return "", fmt.Errorf("read machine status: %w", err)
	}

	switch ms.TypedSpec().Stage {
	case runtime.MachineStageMaintenance:
		return StageMaintenance, nil
	case runtime.MachineStageBooting:
		return StageBooting, nil
	case runtime.MachineStageRunning:
		return StageRunning, nil
	case runtime.MachineStageInstalling:
		return StageInstalling, nil
	default:
		return StageOther, nil
	}
}

func (n *node) Configured(ctx context.Context) (bool, error) {
	_, err := n.st.Get(ctx, resource.NewMetadata(config.NamespaceName, config.MachineConfigType, config.ActiveID, resource.VersionUndefined))
	if state.IsNotFoundError(err) || status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read machine configuration status: %w", err)
	}

	return true, nil
}

func (n *node) Addresses(ctx context.Context) ([]netip.Prefix, error) {
	na, err := safe.StateGetByID[*network.NodeAddress](ctx, n.st, network.NodeAddressCurrentID)
	if err != nil {
		return nil, fmt.Errorf("read node addresses: %w", err)
	}

	return na.TypedSpec().Addresses, nil
}

func (n *node) ApplyConfig(ctx context.Context, cfg []byte) error {
	resp, err := n.c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: cfg,
		Mode: machineapi.ApplyConfigurationRequest_AUTO,
	})
	if err != nil {
		return fmt.Errorf("apply configuration: %w", err)
	}
	for _, m := range resp.GetMessages() {
		slog.Info("configuration applied", "mode", m.GetMode().String(), "details", m.GetModeDetails())
	}

	return nil
}

// Bootstrap bootstraps etcd. It returns ErrAlreadyBootstrapped for a node that is already bootstrapped.
func (n *node) Bootstrap(ctx context.Context) error {
	err := n.c.Bootstrap(ctx, &machineapi.BootstrapRequest{})
	if err != nil && (status.Code(err) == codes.AlreadyExists || strings.Contains(err.Error(), "already bootstrapped")) {
		return ErrAlreadyBootstrapped
	}
	if err != nil {
		return fmt.Errorf("bootstrap etcd: %w", err)
	}

	return nil
}

// IsBootstrapped reports whether etcd runs with at least one member. An etcd that is not running
// or not yet bootstrapped yields false without an error.
func (n *node) IsBootstrapped(ctx context.Context) (bool, error) {
	resp, err := n.c.EtcdMemberList(ctx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		if etcdNotRunning(err) {
			return false, nil
		}
		return false, fmt.Errorf("list etcd members: %w", err)
	}
	for _, m := range resp.GetMessages() {
		if len(m.GetMembers()) > 0 || len(m.GetLegacyMembers()) > 0 {
			return true, nil
		}
	}

	return false, nil
}

func etcdNotRunning(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.FailedPrecondition:
		return true
	}
	msg := err.Error()

	return strings.Contains(msg, "not running") || strings.Contains(msg, "not bootstrapped")
}

func (n *node) Kubeconfig(ctx context.Context) ([]byte, error) {
	kc, err := n.c.Kubeconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch kubeconfig: %w", err)
	}

	return kc, nil
}
