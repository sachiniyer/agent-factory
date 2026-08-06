//go:build linux

package git

import (
	"time"

	"golang.org/x/sys/unix"
)

// statModTime reads a Stat_t's modification time. The field is the one genuine
// portability split in this file's neighbourhood: POSIX names the member
// st_mtim, and darwin exposes it as Mtimespec.
func statModTime(stat *unix.Stat_t) time.Time {
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}
