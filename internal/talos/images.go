package talos

import (
	"context"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/meta"
	"github.com/siderolabs/talos/pkg/machinery/resources/etcd"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// execFailure marks a service task whose binary could not be executed at all, whatever the error: Talos reports
// the last log line of the task, and the container runtime logs "exec <path>: <error>" when it cannot start the
// binary. That is how an image whose unpacked files a power cut left empty or unreadable shows up.
const execFailure = `(last log "exec /`

// systemImage returns the image a Talos system service runs from, as named by the spec the service pulls it from.
type systemImage func(ctx context.Context, n *node) (string, error)

// imageServices are the Talos system services that run from a container image rather than from the root filesystem.
var imageServices = map[string]systemImage{
	"etcd": func(ctx context.Context, n *node) (string, error) {
		spec, err := safe.StateGetByID[*etcd.Spec](ctx, n.st, etcd.SpecID)
		if err != nil {
			return "", fmt.Errorf("read etcd spec: %w", err)
		}
		return spec.TypedSpec().Image, nil
	},
	"kubelet": func(ctx context.Context, n *node) (string, error) {
		spec, err := safe.StateGetByID[*k8s.KubeletSpec](ctx, n.st, k8s.KubeletID)
		if err != nil {
			return "", fmt.Errorf("read kubelet spec: %w", err)
		}
		return spec.TypedSpec().Image, nil
	},
}

// BrokenSystemImages returns the images of the system services that fail to start because their binary cannot be
// executed, in service order.
func (n *node) BrokenSystemImages(ctx context.Context) ([]string, error) {
	resp, err := n.c.ServiceList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	var broken []string
	for _, list := range resp.GetMessages() {
		for _, svc := range list.GetServices() {
			image, ok := imageServices[svc.GetId()]
			if !ok || !failsToExec(svc) {
				continue
			}
			ref, err := image(ctx, n)
			if err != nil {
				return nil, err
			}
			broken = append(broken, ref)
		}
	}
	return broken, nil
}

// failsToExec reports whether the most recent event of svc is a failure to execute its binary. Talos lists the
// events oldest first; a service that failed and was then started again and stayed up does not count.
func failsToExec(svc *machineapi.ServiceInfo) bool {
	events := svc.GetEvents().GetEvents()
	return len(events) > 0 && strings.Contains(events[len(events)-1].GetMsg(), execFailure)
}

// resetKey is the META key that records a reset of the node storage since the node was last ready. META is a
// partition of its own, so the record survives the reset.
const resetKey = meta.UserReserved1

// readyKey is the META key that marks a node that has been ready once. Nothing deletes it.
const readyKey = meta.UserReserved2

// ResetRecorded reports whether a reset of the node storage is recorded in META.
func (n *node) ResetRecorded(ctx context.Context) (bool, error) {
	return n.metaKeySet(ctx, resetKey, "the reset record")
}

// WasReady reports whether META marks the node as having been ready.
func (n *node) WasReady(ctx context.Context) (bool, error) {
	return n.metaKeySet(ctx, readyKey, "the ready marker")
}

// MarkReady marks the node in META as having been ready.
func (n *node) MarkReady(ctx context.Context) error {
	if err := n.c.MetaWrite(ctx, readyKey, []byte("ready")); err != nil {
		return fmt.Errorf("write the ready marker: %w", err)
	}
	return nil
}

// metaKeySet reports whether key is set in META. It reads the MetaKey resource, which Talos fills when it loads
// META early in boot, before any extension service starts.
func (n *node) metaKeySet(ctx context.Context, key uint8, what string) (bool, error) {
	_, err := safe.StateGetByID[*runtime.MetaKey](ctx, n.st, runtime.MetaKeyTagToID(key))
	if state.IsNotFoundError(err) || status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", what, err)
	}
	return true, nil
}

// RecordReset records a reset of the node storage in META.
func (n *node) RecordReset(ctx context.Context) error {
	if err := n.c.MetaWrite(ctx, resetKey, []byte("reset")); err != nil {
		return fmt.Errorf("record the reset: %w", err)
	}
	return nil
}

// ClearResetRecord forgets the reset of the node storage.
func (n *node) ClearResetRecord(ctx context.Context) error {
	err := n.c.MetaDelete(ctx, resetKey)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("clear the reset record: %w", err)
	}
	return nil
}

// ResetEphemeral wipes the EPHEMERAL partition, which holds /var with the container images, their unpacked layers
// and the cluster data, and reboots. STATE (the machine configuration), META and the other volumes are kept, so the
// node comes back configured. The reset is forced: a single node whose etcd does not run cannot leave the cluster.
func (n *node) ResetEphemeral(ctx context.Context) error {
	err := n.c.ResetGeneric(ctx, &machineapi.ResetRequest{
		Graceful: false,
		Reboot:   true,
		Mode:     machineapi.ResetRequest_SYSTEM_DISK,
		SystemPartitionsToWipe: []*machineapi.ResetPartitionSpec{
			{Label: constants.EphemeralPartitionLabel, Wipe: true},
		},
	})
	if err != nil {
		return fmt.Errorf("reset node storage: %w", err)
	}
	return nil
}
