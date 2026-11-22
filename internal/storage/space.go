// Package storage reports the space of the data volume.
package storage

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrNoSpace is returned by Check when the volume lacks the space an operation needs.
var ErrNoSpace = errors.New("not enough free space")

// Usage returns the size of the file system at path and the space free to unprivileged writers, in bytes.
func Usage(path string) (size, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	block := blockSize(&st)
	return uint64(st.Blocks) * block, uint64(st.Bavail) * block, nil //nolint:unconvert
}

// Check refuses an operation that needs expected bytes while less than twice that is free, which leaves room for the
// copy the operation works on. An expected size of zero is unknown and always allowed.
func Check(path string, expected uint64) error {
	if expected == 0 {
		return nil
	}
	_, free, err := Usage(path)
	if err != nil {
		return err
	}
	if !enough(free, expected) {
		return fmt.Errorf("%w: %d bytes needed twice over, %d bytes free", ErrNoSpace, expected, free)
	}
	return nil
}

// enough tells whether free bytes hold expected bytes twice over.
func enough(free, expected uint64) bool {
	// Equal to free >= 2*expected, without the overflow.
	return free/2 >= expected
}
