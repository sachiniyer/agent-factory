//go:build unix

package fsprobe

import "syscall"

// statfsFn is the test seam under Full — the metadata syscall that answers a
// filesystem's free-block count without needing write access to the path.
var statfsFn = syscall.Statfs

// fsFull runs statfs and reports whether the filesystem hosting dir has zero
// free blocks — the fullness verdict a write probe cannot produce on a path
// the process cannot write to at all (a root-owned daemon data root). Bfree
// rather than Bavail: the consumer whose writes this evidence stands in for
// (dockerd) runs as root and can use the reserved blocks, so the honest
// "would the write fail with ENOSPC" answer is the total-free count (#5295
// review).
func fsFull(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := statfsFn(dir, &st); err != nil {
		return false, err
	}
	return st.Bfree == 0, nil
}
