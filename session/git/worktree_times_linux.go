//go:build linux

package git

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// setTimesFromStat copies a source node's access and modification times onto the
// descriptor that owns its copy.
//
// AT_EMPTY_PATH lets utimensat(2) act on the descriptor itself rather than on a
// name, which keeps the write anchored to the object this process opened and
// verified — the discipline the rest of the copier follows — at full nanosecond
// resolution. It is a Linux extension, which is why the other platforms take a
// different route rather than sharing this one.
func setTimesFromStat(fd int, stat *syscall.Stat_t) error {
	times := []unix.Timespec{
		{Sec: stat.Atim.Sec, Nsec: stat.Atim.Nsec},
		{Sec: stat.Mtim.Sec, Nsec: stat.Mtim.Nsec},
	}
	return unix.UtimesNanoAt(fd, "", times, unix.AT_EMPTY_PATH)
}
