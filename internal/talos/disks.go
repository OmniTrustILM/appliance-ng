package talos

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block/blockhelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Disk is a block device of the node.
type Disk struct {
	// ID is the serial of the disk, else its WWID; empty when it has neither. Unlike the device path it stays the
	// same when the disks are enumerated in another order.
	ID        string
	DevPath   string
	SizeBytes uint64
	// System marks the disk Talos is installed on or, before the install, the disk the installer will choose.
	System bool
	// Empty marks a disk with no partitions and no file system.
	Empty bool
	// Probed marks a disk the node has looked at for partitions and file systems; Empty is false until it has.
	Probed bool
	// USB marks a disk on the USB transport.
	USB bool
	// SharedID marks a disk whose ID is also the serial or WWID of another disk, so a selector on it could match
	// either.
	SharedID bool
}

// Disks lists the writable disks of the node, ordered by device path; read-only devices such as loop devices and
// CD-ROMs are left out. A disk is empty when the node probed it and found neither a partition nor a file system.
// Before the install, the disk installSelector matches first is the system disk, as the installer picks it.
func (n *node) Disks(ctx context.Context, installSelector string) ([]Disk, error) {
	systemID, err := n.systemDiskID(ctx, installSelector)
	if err != nil {
		return nil, err
	}

	disks, err := safe.StateListAll[*block.Disk](ctx, n.st)
	if err != nil {
		return nil, fmt.Errorf("list disks: %w", err)
	}

	volumes, err := safe.StateListAll[*block.DiscoveredVolume](ctx, n.st)
	if err != nil {
		return nil, fmt.Errorf("list discovered volumes: %w", err)
	}

	holders := idHolders(disks)
	content := map[string]bool{} // disk ID -> the disk has a file system, a partition table or a partition
	probed := map[string]bool{}

	for v := range volumes.All() {
		id, spec := v.Metadata().ID(), v.TypedSpec()
		if spec.Parent != "" {
			content[spec.Parent] = true
		}

		probed[id] = true

		if spec.Name != "" {
			content[id] = true
		}
	}

	out := make([]Disk, 0, disks.Len())

	for d := range disks.All() {
		id, spec := d.Metadata().ID(), d.TypedSpec()
		if spec.Readonly || spec.CDROM {
			continue
		}

		diskID := cmp.Or(spec.Serial, spec.WWID)
		out = append(out, Disk{
			ID:        diskID,
			DevPath:   spec.DevPath,
			SizeBytes: spec.Size,
			System:    systemID == id,
			Empty:     probed[id] && !content[id],
			Probed:    probed[id],
			USB:       spec.Transport == "usb",
			SharedID:  holders[diskID] > 1,
		})
	}

	slices.SortFunc(out, func(a, b Disk) int { return cmp.Compare(a.DevPath, b.DevPath) })

	return out, nil
}

// idHolders counts, for each serial and WWID, the disks that have it.
func idHolders(disks safe.List[*block.Disk]) map[string]int {
	holders := map[string]int{}

	for d := range disks.All() {
		spec := d.TypedSpec()
		for _, id := range slices.Compact([]string{spec.Serial, spec.WWID}) {
			holders[id]++
		}
	}

	delete(holders, "")

	return holders
}

// systemDiskID returns the ID of the disk Talos is installed on or, before the install, of the first disk that
// installSelector matches, which is the one the installer takes; empty when there is none.
func (n *node) systemDiskID(ctx context.Context, installSelector string) (string, error) {
	sys, err := safe.StateGetByID[*block.SystemDisk](ctx, n.st, block.SystemDiskID)
	switch {
	case err == nil:
		return sys.TypedSpec().DiskID, nil
	case !state.IsNotFoundError(err) && status.Code(err) != codes.NotFound:
		return "", fmt.Errorf("read system disk: %w", err)
	}

	expr, err := cel.ParseBooleanExpression(installSelector, celenv.DiskLocator())
	if err != nil {
		return "", fmt.Errorf("parse the install disk selector: %w", err)
	}

	matched, err := blockhelpers.MatchDisks(ctx, state.WrapCore(n.st), &expr)
	if err != nil {
		return "", fmt.Errorf("match the install disk: %w", err)
	}

	if len(matched) == 0 {
		return "", nil
	}

	return matched[0].Metadata().ID(), nil
}
