//go:build darwin

package git

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// prepareDirectoryEntryRemoval grants owner access on a directory the cleanup
// is about to claim by name. Renaming a directory on macOS 15 requires write
// permission on the directory itself — renameatx_np fails EACCES on a node the
// process owns but cannot write, a check macOS 26 no longer makes and Linux
// never made. A restrictive umask can create the private staging root
// unrenamable, and a claimed worktree root can carry a user's read-only mode;
// either strands the secured name instead of removing it (#6641).
//
// Opened-tree removals already cover this through ensureOwnerWritableDirectory,
// which fchmods each descriptor the route walk holds. This is the counterpart
// for an entry no descriptor owns — the staging root whose open just failed,
// the root of a claimed tree. The caller has already verified the name still
// binds the node it means to remove; AT_SYMLINK_NOFOLLOW keeps the chmod on
// that node rather than following a link a racer could leave at the name, and
// only owner bits are added, so nothing widens for any other user.
//
// A name can never be chmodded with the binding fchmod gives a held descriptor:
// the mode that makes this necessary is exactly the mode that denies the open
// which would produce the descriptor. So the name is identified as the verified
// inode immediately before and after the fchmodat, and a swap that lands inside
// that bracket is detected and refused rather than silently widening a
// replacement. The residual is bounded to a same-UID node holding owner bits
// for the single syscall it occupied the name — the claim identity check above
// still refuses to remove it.
func prepareDirectoryEntryRemoval(parent *os.File, name string, expected pathIdentity) error {
	before, err := statAt(parent, name)
	if err != nil || !expected.same(identityFromStat(before)) {
		if err == nil {
			err = fmt.Errorf("entry %s changed before it could be prepared for removal", name)
		}
		return err
	}
	mode := uint32(before.Mode) & 0o7777
	if mode&0o700 == 0o700 {
		return nil
	}
	if err := unix.Fchmodat(int(parent.Fd()), name, mode|0o700, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	after, err := statAt(parent, name)
	if err != nil || !expected.same(identityFromStat(after)) {
		if err == nil {
			err = fmt.Errorf("entry %s changed while it was prepared for removal", name)
		}
		return err
	}
	return nil
}
