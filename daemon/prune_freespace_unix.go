//go:build !windows

package daemon

import "syscall"

// freeBytesAt returns the free space of path's filesystem and that filesystem's
// device id, so callers can count a shared mount once.
func freeBytesAt(path string) (free, dev uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	var fi syscall.Stat_t
	if err := syscall.Stat(path, &fi); err != nil {
		return 0, 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(fi.Dev), true //nolint:gosec // non-negative block counts
}
