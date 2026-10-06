//go:build darwin || freebsd || netbsd || dragonfly

package config

import "syscall"

// statCTimeNS is the inode-change time in nanoseconds. Unlike mtime, ctime is
// not settable: a writer that back-dates mtimes to preserve a signature (cp -p,
// tar restore, rsync -t) still bumps ctime, so including it is what keeps the
// file-signature cache honest on coarse-mtime filesystems.
func statCTimeNS(st *syscall.Stat_t) int64 {
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
}
