//go:build !windows

package daemon

import (
	"syscall"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// freeBytesAt returns the free space of path's filesystem and that filesystem's
// device id, so callers can count a shared mount once. Bounded: a stalled mount
// answers "unknown", not a hang.
func freeBytesAt(path string) (free, dev uint64, ok bool) {
	type probe struct{ free, dev uint64 }
	p, err := sessiongit.BoundedFSOp("measuring free space at "+path, func() (probe, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return probe{}, err
		}
		var fi syscall.Stat_t
		if err := syscall.Stat(path, &fi); err != nil {
			return probe{}, err
		}
		return probe{uint64(st.Bavail) * uint64(st.Bsize), uint64(fi.Dev)}, nil //nolint:gosec // non-negative block counts
	})
	if err != nil {
		return 0, 0, false
	}
	return p.free, p.dev, true
}
