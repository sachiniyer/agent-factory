//go:build !linux && !darwin && !freebsd && !netbsd && !dragonfly

package config

import "syscall"

// statCTimeNS reports zero on platforms whose Stat_t field layout this cache
// has not been taught. The signature degrades to (dev, ino, size, mtime, perm)
// there — still correct for every atomic-rename writer, which is what af's own
// writes always are — it only loses the back-dated-mtime in-place rewrite case
// ctime exists to catch.
func statCTimeNS(st *syscall.Stat_t) int64 {
	return 0
}
