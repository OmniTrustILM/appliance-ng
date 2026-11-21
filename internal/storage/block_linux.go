package storage

import "golang.org/x/sys/unix"

// blockSize is the unit of the block counts of st: Linux counts them in fragments, which are never negative.
func blockSize(st *unix.Statfs_t) uint64 {
	return uint64(st.Frsize) //nolint:gosec
}
