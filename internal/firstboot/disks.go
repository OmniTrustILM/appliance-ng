package firstboot

import "github.com/OmniTrustILM/appliance-ng/internal/talos"

// minDataDiskBytes is the size a second disk needs to hold the data volume instead of the system disk.
const minDataDiskBytes = 20 * 1000 * 1000 * 1000

// chooseDataDisk returns the largest empty disk, other than the system disk, of at least 20 GB that has an ID the
// data volume selector matches on that disk alone. USB disks are never chosen. It reports false when there is none,
// which leaves the data on the system disk.
func chooseDataDisk(disks []talos.Disk) (talos.Disk, bool) {
	var best talos.Disk
	found := false

	for _, d := range disks {
		if d.System || d.USB || !d.Empty || d.ID == "" || d.SharedID || d.SizeBytes < minDataDiskBytes {
			continue
		}

		if !found || d.SizeBytes > best.SizeBytes {
			best, found = d, true
		}
	}

	return best, found
}
