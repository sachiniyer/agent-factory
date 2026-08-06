//go:build darwin

package git

import (
	"time"

	"golang.org/x/sys/unix"
)

// statModTime reads a Stat_t's modification time. See the linux sibling: the
// member is Mtim there and Mtimespec here, which is why this is split rather
// than written once.
func statModTime(stat *unix.Stat_t) time.Time {
	return time.Unix(stat.Mtimespec.Sec, stat.Mtimespec.Nsec)
}
