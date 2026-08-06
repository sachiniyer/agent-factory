//go:build linux

package git

import "golang.org/x/sys/unix"

// xattrAbsentErrnos is what Linux returns when an extended attribute is not present.
// See isXattrVanished for why this is platform-split rather than a single list.
func xattrAbsentErrnos() []error { return []error{unix.ENODATA} }
