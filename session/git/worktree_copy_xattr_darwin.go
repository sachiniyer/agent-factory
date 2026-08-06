//go:build darwin

package git

import "golang.org/x/sys/unix"

// xattrAbsentErrnos is what darwin returns when an extended attribute is not present.
// ENOATTR is the one the xattr syscalls actually use; ENODATA is listed too because it
// is a distinct errno on this platform and a filesystem may report it.
// See isXattrVanished for why this is platform-split rather than a single list.
func xattrAbsentErrnos() []error { return []error{unix.ENOATTR, unix.ENODATA} }
