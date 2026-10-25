//go:build !linux && !darwin && !freebsd && !netbsd && !dragonfly

package config

import "os"

// statCTimeNS reports zero on platforms whose Stat_t field layout this cache
// has not been taught. The signature degrades to (dev, ino, size, mtime, perm)
// there — still correct for every atomic-rename writer, which is what af's own
// writes always are — it only loses the back-dated-mtime in-place rewrite case
// ctime exists to catch. A platform whose Sys() is not a syscall.Stat_t at all
// gets no signature either way, so its files are simply never cached.
func statCTimeNS(_ os.FileInfo) int64 {
	return 0
}
