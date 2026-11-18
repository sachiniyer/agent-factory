//go:build unix

package fsprobe

import (
	"fmt"
	"os"
	"syscall"
)

// openBoundedFile opens path for a bounded read with the same guard every
// sibling open inside an fsprobe flight carries (openCodexRollout in
// session/codex_rollout_scan.go, openCarryFileAt in
// session/conversation_carry_fs.go): O_NOFOLLOW rejects a symlink planted at
// the leaf, O_NONBLOCK keeps a FIFO planted there from hanging the open, and
// the regular-file check on the opened descriptor refuses a non-regular file
// swapped in after a listing's lstat — the stat-then-open hazard the listing
// cannot close. A planted FIFO is answered as a non-regular refusal within
// the bound rather than wedging the flight's work goroutine in openat forever
// and latching its registry entry against every later ask (#7383). The flags
// are no-ops on the regular file a real transcript is.
func openBoundedFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	return file, nil
}
