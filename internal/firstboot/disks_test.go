package firstboot

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/OmniTrustILM/appliance-ng/internal/machineconfig"
	"github.com/OmniTrustILM/appliance-ng/internal/talos"
)

const gb = 1000 * 1000 * 1000

func TestChooseDataDisk(t *testing.T) {
	system := talos.Disk{ID: "sys", DevPath: "/dev/vda", SizeBytes: 50 * gb, System: true}
	small := talos.Disk{ID: "small", DevPath: "/dev/vdb", SizeBytes: 19 * gb, Empty: true}
	exact := talos.Disk{ID: "exact", DevPath: "/dev/vdc", SizeBytes: 20 * gb, Empty: true}
	large := talos.Disk{ID: "large", DevPath: "/dev/vdd", SizeBytes: 100 * gb, Empty: true}
	used := talos.Disk{ID: "used", DevPath: "/dev/vde", SizeBytes: 500 * gb}
	noID := talos.Disk{DevPath: "/dev/vdf", SizeBytes: 500 * gb, Empty: true}
	emptySystem := talos.Disk{ID: "sys2", DevPath: "/dev/vdg", SizeBytes: 500 * gb, System: true, Empty: true}

	usb := talos.Disk{ID: "usb", DevPath: "/dev/sdb", SizeBytes: 500 * gb, Empty: true, USB: true}
	shared := talos.Disk{ID: "same", DevPath: "/dev/vdh", SizeBytes: 500 * gb, Empty: true, SharedID: true}
	// Before the install, the disk the installer will take is marked System and is as empty as the others.
	installTarget := talos.Disk{ID: "target", DevPath: "/dev/vda", SizeBytes: 500 * gb, System: true, Empty: true, Probed: true}

	tests := []struct {
		name  string
		disks []talos.Disk
		want  string
	}{
		{"none", nil, ""},
		{"system only", []talos.Disk{system}, ""},
		{"one small", []talos.Disk{system, small}, ""},
		{"exactly the minimum", []talos.Disk{system, exact}, "exact"},
		{"two empty, the largest", []talos.Disk{system, exact, large}, "large"},
		{"in use", []talos.Disk{system, used}, ""},
		{"usb", []talos.Disk{system, usb}, ""},
		{"without an ID", []talos.Disk{system, noID}, ""},
		{"system disk is never chosen", []talos.Disk{emptySystem}, ""},
		{"largest eligible", []talos.Disk{system, noID, used, small, exact}, "exact"},
		{"ID shared with another disk", []talos.Disk{system, shared, exact}, "exact"},
		{"never the install target", []talos.Disk{installTarget, large}, "large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := chooseDataDisk(tt.disks)
			if ok != (tt.want != "") || got.ID != tt.want {
				t.Errorf("got %q, %v; want %q", got.ID, ok, tt.want)
			}
		})
	}
}

func TestRunListsDisksWithTheInstallSelector(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	if got := node.snapshot().installSelector; got != machineconfig.InstallDiskSelector {
		t.Errorf("disks listed with install selector %q, want %q", got, machineconfig.InstallDiskSelector)
	}
}

func TestRunConfiguresTheDataDisk(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}, disks: []talos.Disk{
		{ID: "sys", DevPath: "/dev/vda", SizeBytes: 50 * gb, System: true},
		{ID: "ilm-data", DevPath: "/dev/vdb", SizeBytes: 20 * gb, Empty: true},
	}}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	if got := h.inputs[0].DataDisk; got != "ilm-data" {
		t.Errorf("DataDisk = %q, want ilm-data", got)
	}
	logs := h.log.String()
	if strings.Contains(logs, "ilm-data") {
		t.Errorf("the log names the disk ID:\n%s", logs)
	}
	if !strings.Contains(logs, "/dev/vdb") {
		t.Errorf("the log does not name the chosen device:\n%s", logs)
	}
}

func TestRunKeepsDataOnTheSystemDiskWithoutASecondDisk(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}}
	h := newHarness(node, nil)
	node.disks = []talos.Disk{{ID: "sys", DevPath: "/dev/vda", SizeBytes: 50 * gb, System: true, Probed: true},
		{DevPath: "/dev/vdb", SizeBytes: 30 * gb}}
	h.start(t)

	eventually(t, "apply", func() bool { return len(node.snapshot().applied) == 1 })
	logs := h.log.String()
	if !strings.Contains(logs, "not probed yet") || !strings.Contains(logs, "/dev/vdb") ||
		!strings.Contains(logs, "system disk\" device=/dev/vda") {
		t.Errorf("the log does not explain the fallback:\n%s", logs)
	}
	if got := h.inputs[0].DataDisk; got != "" {
		t.Errorf("DataDisk = %q, want the system disk", got)
	}
}

func TestRunNeedsTheDiskList(t *testing.T) {
	node := &fakeNode{stage: talos.StageMaintenance, addrs: [][]netip.Prefix{globalAddr}, disksErr: errors.New("machined unavailable")}
	h := newHarness(node, nil)
	h.start(t)

	eventually(t, "error status", func() bool { return strings.Contains(h.store.Get().Message, "machined unavailable") })
	if s := node.snapshot(); len(s.applied) != 0 {
		t.Errorf("applied %d configurations without the disk list", len(s.applied))
	}
}
