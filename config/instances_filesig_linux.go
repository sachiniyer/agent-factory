//go:build linux

package config

import (
	"os"
	"syscall"
)

// statCTimeNS is the inode-change time in nanoseconds. Unlike mtime, ctime is
// not settable: a writer that back-dates mtimes to preserve a signature (cp -p,
// tar restore, rsync -t) still bumps ctime, so including it is what keeps the
// file-signature cache honest on coarse-mtime filesystems.
func statCTimeNS(info os.FileInfo) int64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
