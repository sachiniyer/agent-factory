//go:build linux

package fsprobe

import (
	"os"

	"golang.org/x/sys/unix"
)

// openAnonymousProbeFile asks the kernel for an O_TMPFILE descriptor in dir:
// an inode that NEVER has a name in any directory. It is the strongest answer
// to the probe-debris class — nothing exists for `git status`/`git add -A` to
// see at any instant, so probing inside a tracked worktree cannot leave
// stageable content even in a pathological unlink window. A filesystem or
// kernel without support fails with a normal errno (EOPNOTSUPP et al.) and the
// caller falls back to the named-probe path; supported targets cover tmpfs,
// ext4, xfs, btrfs, and overlayfs.
var openAnonymousProbeFile = func(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "af-add-probe"), nil
}
