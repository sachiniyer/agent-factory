//go:build !linux

package git

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setTimesFromStat copies a source node's access and modification times onto the
// descriptor that owns its copy.
//
// futimes(2) rather than utimensat(2): AT_EMPTY_PATH is a Linux extension, so
// utimensat elsewhere has no way to name an open descriptor, and re-deriving a
// path from the fd would reintroduce exactly the name-based race the
// descriptor-anchored copier exists to avoid.
//
// Two portability traps, both found by building rather than by reading:
// timespec fields are Atim/Mtim on Linux but Atimespec/Mtimespec here, and
// Timeval.Usec is int32 on darwin but int64 on the BSDs — so the conversion goes
// through unix.NsecToTimeval instead of a struct literal, which compiles on one
// and not the other. Verified with GOOS=darwin and GOOS=freebsd.
//
// The cost is resolution: futimes() takes microseconds, so a source timestamp is
// reproduced to the microsecond rather than the nanosecond here. That is a real
// difference from the rename path, and it is why the differential test compares
// mtime at second granularity — a nanosecond comparison would pass on Linux and
// fail on macOS for a copy behaving exactly as designed.
func setTimesFromStat(fd int, stat *syscall.Stat_t) error {
	times := []unix.Timeval{
		unix.NsecToTimeval(stat.Atimespec.Nano()),
		unix.NsecToTimeval(stat.Mtimespec.Nano()),
	}
	return unix.Futimes(fd, times)
}
