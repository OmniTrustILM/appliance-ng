// Package talos is a client for the Talos API of the local node, over the machined socket or over apid.
package talos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	configdoc "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
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

// Node is the subset of the Talos API that the first-boot service and the appliance manager need.
type Node interface {
	Stage(ctx context.Context) (Stage, error)
	ApplyConfig(ctx context.Context, cfg []byte) error
	Bootstrap(ctx context.Context) error
	IsBootstrapped(ctx context.Context) (bool, error)
	Kubeconfig(ctx context.Context) ([]byte, error)
	Addresses(ctx context.Context) ([]netip.Prefix, error)
	// Network reports the network setup of the node for addr: its link, gateway, name and time servers, host name.
	Network(ctx context.Context, addr netip.Addr) (Network, error)
	// Configured reports whether the node holds a machine configuration (the active MachineConfig resource).
	Configured(ctx context.Context) (bool, error)
	// BrokenSystemImages returns the images of system services that fail because their binary cannot be executed.
	BrokenSystemImages(ctx context.Context) ([]string, error)
	// ResetRecorded reports whether a reset of the node storage was recorded since the node was last ready; the
	// record survives the reset.
	ResetRecorded(ctx context.Context) (bool, error)
	// RecordReset records a reset of the node storage.
	RecordReset(ctx context.Context) error
	// ClearResetRecord forgets the reset of the node storage.
	ClearResetRecord(ctx context.Context) error
	// WasReady reports whether the node has been ready once; the marker survives reboots and resets.
	WasReady(ctx context.Context) (bool, error)
	// MarkReady marks the node as having been ready.
	MarkReady(ctx context.Context) error
	// ResetEphemeral wipes the EPHEMERAL partition and reboots; the machine configuration and the volumes are kept.
	ResetEphemeral(ctx context.Context) error
	// PatchConfig applies a strategic merge patch to the running machine configuration; the result is persisted and
	// applied in no-reboot mode. A failed apply returns an error that names the changed documents.
	PatchConfig(ctx context.Context, patch []byte) error
	// ExtensionConfigFile returns the content of the file mounted at file by the ExtensionServiceConfig name of the
	// running machine configuration, or nil when there is none. Talos mounts such a file only into a service started
	// after the configuration was loaded.
	ExtensionConfigFile(ctx context.Context, name, file string) ([]byte, error)
	// RemoveExtensionConfig removes the file mounted at file from the ExtensionServiceConfig name, in no-reboot mode.
	RemoveExtensionConfig(ctx context.Context, name, file string) error
	// Documents returns the documents of the running machine configuration whose kind is one of kinds.
	Documents(ctx context.Context, kinds ...string) ([]configdoc.Document, error)
	// ReplaceDocuments replaces whole documents of the running machine configuration: each of docs takes the place of
	// the running document with its ID (see DocumentID) or is added, and each running document whose ID is in remove
	// is dropped. The result is persisted and applied in no-reboot mode; a failed apply returns an error that names
	// the changed documents.
	ReplaceDocuments(ctx context.Context, docs []configdoc.Document, remove []string) error
	// Disks lists the disks of the node, ordered by device path; before the install, the disk installSelector
	// matches first is marked as the system disk.
	Disks(ctx context.Context, installSelector string) ([]Disk, error)
	// ExtensionVersion returns the version of the system extension name the node runs, empty without one.
	ExtensionVersion(ctx context.Context, name string) (string, error)
	// Versions returns the Talos version of the node and the Kubernetes version of its machine configuration.
	Versions(ctx context.Context) (talos, kubernetes string, err error)
	// ClientConfig generates a talosconfig for the node with a client certificate of roles, valid for ttl; the node's
	// CA signs it, so its key never leaves the node.
	ClientConfig(ctx context.Context, roles []string, ttl time.Duration) ([]byte, error)
	// Upgrade pulls the Talos installer image into the system store and installs it as the boot entry the next boot
	// boots. It neither drains nor reboots the node.
	Upgrade(ctx context.Context, image string) error
	// RebootPowercycle reboots the node with a power cycle, which boots the default boot entry.
	RebootPowercycle(ctx context.Context) error
	// SetInstallerImage sets the installer image of the UnattendedInstallConfig of the running machine configuration,
	// applied in no-reboot mode.
	SetInstallerImage(ctx context.Context, image string) error
	// PullSystemImage pulls image into the system store, which Talos runs its own services, such as etcd, from.
	PullSystemImage(ctx context.Context, image string) error
	// Rollback makes the previous boot entry the one the node boots and reboots it.
	Rollback(ctx context.Context) error
	// SetKubernetesImages sets the image of a Kubernetes component (apiServer, controllerManager, scheduler or
	// kubelet) in the running machine configuration, applied in no-reboot mode.
	SetKubernetesImages(ctx context.Context, component, image string) error
	Reboot(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// callTimeout bounds every call to the node that the caller did not bound: some calls, such as listing etcd members
// while etcd has no leader, otherwise never return.
var callTimeout = 30 * time.Second

const (
	roleKey   = "talos-role"
	roleAdmin = "os:admin"
)

type node struct {
	c  *client.Client
	st state.CoreState
}

// Dial connects to the machined Unix socket. Every call carries the admin role metadata that apid
// would otherwise add; machined rejects the call without it. A call without a deadline gives up after callTimeout.
func Dial(ctx context.Context, socket string) (Node, error) {
	c, err := client.New(ctx,
		client.WithUnixSocket(socket),
		client.WithGRPCDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithChainUnaryInterceptor(boundedUnary, withRoleUnary),
			grpc.WithChainStreamInterceptor(boundedStreamCall, withRoleStream),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to machined socket %s: %w", socket, err)
	}
	slog.Debug("connected to machined", "socket", socket)

	return &node{c: c, st: c.COSI}, nil
}

// DialAPI connects to apid at endpoint with mutual TLS, using the CA, the client certificate and the key of the
// current context of talosconfig; apid takes the role from the client certificate. A call without a deadline gives
// up after callTimeout.
func DialAPI(ctx context.Context, endpoint string, talosconfig []byte) (Node, error) {
	cfg, err := clientconfig.FromBytes(talosconfig)
	if err != nil {
		// The parser quotes the offending value, which may be the private key.
		return nil, errors.New("parse talosconfig: not a valid talosconfig")
	}
	current, ok := cfg.Contexts[cfg.Context]
	if !ok || current == nil {
		return nil, fmt.Errorf("talosconfig has no context %q", cfg.Context)
	}
	if current.Crt == "" || current.Key == "" {
		return nil, fmt.Errorf("talosconfig context %q has no client certificate", cfg.Context)
	}

	c, err := client.New(ctx,
		client.WithConfigContext(current),
		client.WithEndpoints(endpoint),
		client.WithGRPCDialOptions(
			grpc.WithChainUnaryInterceptor(boundedUnary),
			grpc.WithChainStreamInterceptor(boundedStreamCall),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to the Talos API at %s: %w", endpoint, err)
	}
	slog.Debug("connected to the Talos API", "endpoint", endpoint, "context", cfg.Context)

	return &node{c: c, st: c.COSI}, nil
}

func boundedUnary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	ctx, cancel := bounded(ctx)
	defer cancel()
	return invoker(ctx, method, req, reply, cc, opts...)
}

func boundedStreamCall(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption,
) (grpc.ClientStream, error) {
	ctx, cancel := bounded(ctx)
	s, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return &boundedStream{ClientStream: s, cancel: cancel}, nil
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

// bounded gives ctx the call timeout unless it already has a deadline.
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {
			// ctx keeps the caller's deadline, so there is no timeout of our own to release.
		}
	}
	return context.WithTimeout(ctx, callTimeout)
}

// boundedStream releases the call timeout once the stream ends.
type boundedStream struct {
	grpc.ClientStream
	cancel context.CancelFunc
}

func (s *boundedStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.cancel()
	}
	return err
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

func (n *node) ClientConfig(ctx context.Context, roles []string, ttl time.Duration) ([]byte, error) {
	resp, err := n.c.GenerateClientConfiguration(ctx, &machineapi.GenerateClientConfigurationRequest{
		Roles:  roles,
		CrtTtl: durationpb.New(ttl),
	})
	if err != nil {
		return nil, fmt.Errorf("generate a client configuration: %w", err)
	}
	msgs := resp.GetMessages()
	if len(msgs) == 0 || len(msgs[0].GetTalosconfig()) == 0 {
		return nil, errors.New("the node returned no client configuration")
	}
	return msgs[0].GetTalosconfig(), nil
}

// Reboot reboots the node.
func (n *node) Reboot(ctx context.Context) error {
	if err := n.c.Reboot(ctx); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}

// upgradeTimeout bounds the pull and the installation of an installer image.
const upgradeTimeout = 15 * time.Minute

// systemStore is the containerd store Talos keeps its own images in, the installer among them.
var systemStore = &common.ContainerdInstance{Driver: common.ContainerDriver_CONTAINERD, Namespace: common.ContainerdNamespace_NS_SYSTEM}

// Upgrade pulls image into the system store and installs it with the LifecycleService, which writes the new boot
// entry. The Machine service upgrade is not used: its staged form never installs with an unattended install
// configuration, and its direct form drains the node.
func (n *node) Upgrade(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	pulled, err := n.pull(ctx, image)
	if err != nil {
		return err
	}
	stream, err := n.c.LifecycleClient.Upgrade(ctx, &machineapi.LifecycleServiceUpgradeRequest{
		Containerd: systemStore, Source: &machineapi.InstallArtifactsSource{ImageName: pulled},
	})
	if err != nil {
		return fmt.Errorf("install %s: %w", image, err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("install %s: the installer ended without an exit code", image)
		}
		if err != nil {
			return fmt.Errorf("install %s: %w", image, err)
		}
		if code, ok := resp.GetProgress().GetResponse().(*machineapi.LifecycleServiceInstallProgress_ExitCode); ok {
			if code.ExitCode != 0 {
				return fmt.Errorf("install %s: exit code %d", image, code.ExitCode)
			}
			slog.Info("installed the Talos installer image", "image", image)
			return nil
		}
	}
}

// pull pulls image into the system store and returns the name it was stored under.
func (n *node) pull(ctx context.Context, image string) (string, error) {
	stream, err := n.c.ImageClient.Pull(ctx, &machineapi.ImageServicePullRequest{Containerd: systemStore, ImageRef: image})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", image, err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("pull %s: no image name reported", image)
		}
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", image, err)
		}
		if name := resp.GetName(); name != "" {
			return name, nil
		}
	}
}

// pullTimeout bounds the pull of an image into the system store, which may take longer than a call.
const pullTimeout = 15 * time.Minute

func (n *node) PullSystemImage(ctx context.Context, image string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	_, err := n.pull(ctx, image)
	return err
}

func (n *node) RebootPowercycle(ctx context.Context) error {
	if err := n.c.Reboot(ctx, client.WithPowerCycle); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}

func (n *node) Rollback(ctx context.Context) error {
	if err := n.c.Rollback(ctx); err != nil {
		return fmt.Errorf("roll back to the previous boot slot: %w", err)
	}
	return nil
}

// Shutdown shuts the node down.
func (n *node) Shutdown(ctx context.Context) error {
	if err := n.c.Shutdown(ctx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	return nil
}

// Versions returns the Talos version the node runs and the Kubernetes version of its machine configuration, the
// tag of the kubelet image.
func (n *node) Versions(ctx context.Context) (talosVersion, kubernetesVersion string, err error) {
	resp, err := n.c.Version(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read Talos version: %w", err)
	}
	msgs := resp.GetMessages()
	if len(msgs) == 0 || msgs[0].GetVersion().GetTag() == "" {
		return "", "", errors.New("node reported no Talos version")
	}
	talosVersion = msgs[0].GetVersion().GetTag()

	cfg, err := n.machineConfig(ctx)
	if err != nil {
		return "", "", err
	}
	kubelet := cfg.K8sKubeletConfig()
	if kubelet == nil || imageTag(kubelet.Image()) == "" {
		return "", "", errors.New("machine configuration names no tagged kubelet image")
	}

	return talosVersion, imageTag(kubelet.Image()), nil
}

// imageTag returns the tag of an image reference, or "" for a reference without one.
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}
