//go:build !windows

package daemon

import "syscall"

func freeBytesAt(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true //nolint:gosec // block counts are non-negative
}
