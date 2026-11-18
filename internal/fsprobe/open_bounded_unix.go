//go:build unix

package fsprobe

import (
	"os"
	"syscall"
)

// OpenRegular opens path for a bounded read — the one guarded open every
// bounded filesystem caller shares (#7527): O_NOFOLLOW rejects a symlink
// planted at the leaf, O_NONBLOCK keeps a FIFO planted there from hanging the
// open, and RegularFileInfo refuses a non-regular file swapped in after a
// listing's lstat — the stat-then-open hazard the listing cannot close. A
// planted FIFO is answered as a *NonRegularError refusal within the bound
// rather than wedging the flight's work goroutine in openat forever and
// latching its registry entry against every later ask (#7383). The flags are
// no-ops on the regular file a real transcript is.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := RegularFileInfo(file, path)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}
