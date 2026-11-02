//go:build darwin

package git

import (
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
func prepareDirectoryEntryRemoval(parent *os.File, name string) error {
	stat, err := statAt(parent, name)
	if err != nil {
		return err
	}
	mode := uint32(stat.Mode) & 0o7777
	if mode&0o700 == 0o700 {
		return nil
	}
	return unix.Fchmodat(int(parent.Fd()), name, mode|0o700, unix.AT_SYMLINK_NOFOLLOW)
}
