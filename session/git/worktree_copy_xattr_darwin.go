//go:build darwin

package git

import "golang.org/x/sys/unix"

// xattrAbsentErrnos is what darwin returns when an extended attribute is not present.
// ENOATTR is the one the xattr syscalls actually use; ENODATA is listed too because it
// is a distinct errno on this platform and a filesystem may report it.
// See isXattrVanished for why this is platform-split rather than a single list.
func xattrAbsentErrnos() []error { return []error{unix.ENOATTR, unix.ENODATA} }

// xattrUnsupportedErrnos is what darwin returns when extended attributes are not
// available at all. Both spellings are listed because on this platform they are
// DIFFERENT errnos — ENOTSUP is 0x2d and EOPNOTSUPP is 0x66, where Linux makes them
// one value — and the xattr syscalls here return ENOTSUP. Matching only EOPNOTSUPP
// would send an unsupported destination down the "unexpected error" path and fail the
// whole archive on macOS.
func xattrUnsupportedErrnos() []error { return []error{unix.ENOTSUP, unix.EOPNOTSUPP} }
