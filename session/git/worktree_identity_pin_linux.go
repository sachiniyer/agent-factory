//go:build linux

package git

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// creationPinAt probes the creation pin of the entry `name` inside the
// already-open directory `parent`: the inode's birth time as statx btime,
// returned as nanoseconds since the epoch. A delete-and-recreate at the same
// path may recycle the inode slot — on ext4 it does so every time (#7900) —
// but never the birth time, so a durable record that also pins it cannot be
// fooled the way dev+ino alone can.
//
// The kernel or the filesystem may offer no btime: a kernel before 4.11
// answers ENOSYS, and a filesystem that does not track birth times omits
// STATX_BTIME from the result mask. Both answer 0 — the caller records no
// pin for a tree it could not stamp, and a record carrying a pin treats a
// 0 answer as untracked rather than matching on the reusable half of the
// identity.
func creationPinAt(parent *os.File, name string) (int64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(int(parent.Fd()), name, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx); err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return 0, nil
		}
		return 0, err
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return 0, nil
	}
	return statxTimestampNanos(stx.Btime), nil
}

// creationPinFile is creationPinAt for an already-open descriptor on the tree
// root: AT_EMPTY_PATH makes the probe describe the node the descriptor holds,
// so a rename of its pathname in between cannot redirect the pin.
func creationPinFile(f *os.File) (int64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return 0, nil
		}
		return 0, err
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return 0, nil
	}
	return statxTimestampNanos(stx.Btime), nil
}

func statxTimestampNanos(ts unix.StatxTimestamp) int64 {
	return ts.Sec*1e9 + int64(ts.Nsec)
}
