//go:build darwin

package git

import (
	"os"

	"golang.org/x/sys/unix"
)

// creationPinAt probes the creation pin of the entry `name` inside the
// already-open directory `parent`: darwin exposes the vnode birth time
// directly on Stat_t as Btim (the st_birthtimespec field — "whatever birth
// time Stat_t offers", #7900), returned here as nanoseconds since the epoch.
// The ordinary fstatat already carries it, so no statx equivalent is needed.
// A delete-and-recreate at the same path may recycle the inode slot but
// never the birth time.
//
// A filesystem that does not fill the birth time answers a zero Timespec —
// the caller records no pin, and a record carrying a pin treats a 0 answer
// as untracked rather than matching on the reusable half of the identity.
func creationPinAt(parent *os.File, name string) (int64, error) {
	stat, err := statAt(parent, name)
	if err != nil {
		return 0, err
	}
	return timespecNanos(stat.Btim), nil
}

// creationPinFile is creationPinAt for an already-open descriptor on the
// tree root: fstat describes the node the descriptor holds, so a rename of
// its pathname in between cannot redirect the pin.
func creationPinFile(f *os.File) (int64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return timespecNanos(stat.Btim), nil
}

func timespecNanos(ts unix.Timespec) int64 {
	return ts.Sec*1e9 + ts.Nsec
}
