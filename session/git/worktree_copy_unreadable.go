// Unreadable-source handling for the cross-device copier, split from
// worktree_copy_tree.go when that file reached the 1000-line limit (#1145).
package git

import "errors"

// errSourceUnreadable marks a source file the copier has no permission to read.
//
// It is a SENTINEL rather than a plain error because the walker must tell it
// apart from a real failure: everything else aborts the archive, and this one
// records the path and continues. An archive is the SAFE action in this product —
// the one users are told to prefer over kill because it is restorable — so a
// single unreadable file must not block it and push people toward the
// destructive alternative (#3066).
var errSourceUnreadable = errors.New("source file cannot be read by this process")

// copyTreeCollectingUnreadable is copyTree plus the skipped-file report, for
// callers that must surface it. copyTree itself discards the list because its
// callers are the ones with no way to report it; the archive path uses
// copyTreeWithIdentities and reports (#3066).
func copyTreeCollectingUnreadable(src, dest string, unreadable *[]string) error {
	copied, err := copyTreeWithIdentities(src, dest)
	if copied != nil {
		*unreadable = append(*unreadable, copied.unreadable...)
		copied.close()
	}
	return err
}
