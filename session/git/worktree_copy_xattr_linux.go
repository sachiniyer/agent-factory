//go:build linux

package git

import "golang.org/x/sys/unix"

// xattrAbsentErrnos is what Linux returns when an extended attribute is not present.
// See isXattrVanished for why this is platform-split rather than a single list.
func xattrAbsentErrnos() []error { return []error{unix.ENODATA} }

// xattrUnsupportedErrnos is what Linux returns when extended attributes are not
// available at all. ENOTSUP and EOPNOTSUPP are the SAME value here (0x5f), so one
// entry covers both spellings; on darwin they are different numbers, which is why
// this is split alongside xattrAbsentErrnos.
func xattrUnsupportedErrnos() []error { return []error{unix.EOPNOTSUPP} }
