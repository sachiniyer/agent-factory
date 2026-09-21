package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitDirAtRootIsSymlink reports whether <root>/.git is a symbolic link rather
// than a directory or a regular gitdir file. sharedWorktreeCommonDir calls
// os.Stat, which follows the symlink, so a <root>/.git that points at an
// external git common directory another registered checkout may also point
// at reads as a plain directory and the marker at <commonDir>/af/... is
// neither a linked-worktree marker (git did not spawn this checkout through
// `git worktree`) nor one shared through a `<commonDir>/worktrees` subdir.
// The two predicates the absent-marker scan uses to decide the marker could
// be shared therefore both return false and the scan is skipped, even though
// another registered checkout whose <root>/.git points at the same target
// shares the marker through the canonicalized common directory. Treat a
// symlinked <root>/.git as evidence that the common directory may be shared
// with another registration, so the scan runs.
func gitDirAtRootIsSymlink(root string) bool {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// sharedWorktreeCommonDir reports whether commonDir is the git common
// directory of a LINKED WORKTREE — the bare's shared dir, which lives outside
// the worktree root and is shared by every worktree on that bare — rather than
// a MAIN checkout's <root>/.git. A linked worktree's marker at commonDir is
// shared with every checkout on that bare. A main checkout's marker IS
// private to that checkout only when it has not spawned linked worktrees of
// its own; mainCheckoutHasLinkedWorktrees guards the remaining case, so this
// predicate alone is not proof that the marker here is private.
//
// Linked-worktree membership is read from git's worktree metadata rather than
// from directory containment alone: a repository built with `git init
// --separate-git-dir <gitdir>` and a submodule alike keep the git common
// directory outside the worktree root without being linked worktrees, so a
// pure `filepath.Rel` check would falsely report their markers as shared and
// refuse the safe deletion/rebind recovery. A linked worktree's
// `<root>/.git` is a regular file pointing at `<commonDir>/worktrees/<name>`;
// a main checkout's `<root>/.git` is a directory, and under
// --separate-git-dir (or a submodule) it is a regular file pointing at the
// common dir itself rather than into its `worktrees` subdir. Read that file
// so only a true linked worktree reads as shared, not every checkout whose
// common dir happens to sit outside the root.
func sharedWorktreeCommonDir(root, commonDir string) bool {
	gitFile := filepath.Join(root, ".git")
	info, err := os.Stat(gitFile)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	target := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	worktreesDir := filepath.Join(commonDir, "worktrees")
	rel, err := filepath.Rel(worktreesDir, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mainCheckoutHasLinkedWorktrees reports whether a MAIN checkout's
// <root>/.git common directory — which sharedWorktreeCommonDir classifies as
// INSIDE the root and so returns false — is nonetheless shared by linked
// worktrees git has spawned from it. git stores each linked worktree it
// creates under <commonDir>/worktrees/<name>, so a non-empty worktrees
// directory proves this main's git common directory backs more than this
// single checkout, and the marker at commonDir is shared with every such
// worktree. Deleting it would break identity resolution for them — the same
// hole sharedWorktreeCommonDir guards the deletion advice against on the
// linked-worktree side.
func mainCheckoutHasLinkedWorktrees(commonDir string) bool {
	entries, err := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if err != nil {
		// Only a determinate not-exist result proves there are no linked
		// worktrees. A read error from permissions, a transient I/O
		// failure, or any other indeterminate cause is not evidence: the
		// directory may still hold linked worktrees git has spawned, so
		// the marker at commonDir may still be shared with them. Fail
		// closed — treat the unknown case as "worktrees may exist" — so the
		// caller refuses the deletion advice rather than fall through to
		// the copied-marker remedy and recommend deleting a marker that
		// may still be in use by active worktrees.
		return !errors.Is(err, os.ErrNotExist)
	}
	// Conservatively treat any entry in <commonDir>/worktrees as evidence
	// of sharing rather than trust DirEntry.IsDir alone. Directory
	// enumeration on some filesystems (notably NFS and FUSE) reports the
	// unknown entry type for entries it could not classify; IsDir is then
	// false even when the entry is a directory, so a main checkout that
	// actually spawned linked worktrees could be miscounted as private
	// and the copied-marker remedy would tell the user to delete a marker
	// still shared by those worktrees. Any entry — a real linked
	// worktree's directory, a stray file, or an indeterminate-type entry
	// — proves the shared common directory backs more than this single
	// private checkout, so fail closed and let the caller refuse the
	// deletion advice rather than recommend removing a marker that may
	// still be in use by active linked worktrees.
	return len(entries) > 0
}

// symlinkedRootMarkerRefusal is the claimed-marker branch's refusal for a
// <root>/.git that is a symlink to an external git directory another
// registered checkout may share. sharedWorktreeCommonDir and
// mainCheckoutHasLinkedWorktrees both miss this shape (os.Stat follows the
// symlink and reads a directory, and the canonicalized target need not
// carry a <commonDir>/worktrees subdir), so without this guard the marker
// at binding.checkoutMarkerPath would fall through to the copied-marker
// deletion remedy even though it may be the owner's own shared marker. The
// returned error is the fail-closed refusal; nil means <root>/.git is not a
// symlink and the caller should fall through to the copied-marker remedy.
func symlinkedRootMarkerRefusal(binding projectBinding, p Project, checkoutID string, owner Project) error {
	if !gitDirAtRootIsSymlink(binding.root) {
		return nil
	}
	return fmt.Errorf(
		"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
			"the marker belongs to project %s, and project %s's registered root %s still carries a matching marker, "+
			"but this checkout's <root>/.git is a symlink to an external git directory another registered checkout may share, "+
			"and the marker here may be project %s's own shared marker rather than a private copy; "+
			"af cannot tell which marker is the copy, so it will not recommend removing this one — "+
			"move this checkout to a path that does not share its git directory, or restore project %s's root so its own .git is private",
		binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, owner.Root, owner.ID, owner.ID)
}
