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

// gitDirAtRootPointsAtCommonDir reports whether <root>/.git is a regular
// "gitdir: <path>" file that points DIRECTLY at commonDir rather than into
// <commonDir>/worktrees/<name>. Two checkouts both using `git init
// --separate-git-dir <commonDir>` (or two regular .git files written by hand
// to point at the same external directory) share binding.gitCommonDir the
// same way a linked worktree does: the agent-factory checkout marker at
// <commonDir>/af/... is the same file for both, so deleting it through one
// checkout breaks identity resolution at the other. sharedWorktreeCommonDir
// deliberately rejects a target equal to commonDir (a separate-git-dir
// main's gitdir pointer is the common dir itself, not a worktree under
// it), and there need not be a <commonDir>/worktrees subdir, so this shape
// slips past both of those predicates; gitDirAtRootIsSymlink also misses
// it because <root>/.git is a regular file here. Treat a direct .git-file
// pointer at commonDir as evidence the marker could be shared so the
// registry scan still runs.
func gitDirAtRootPointsAtCommonDir(root, commonDir string) bool {
	gitFile := filepath.Join(root, ".git")
	info, err := os.Lstat(gitFile)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// <root>/.git is a symlink — gitDirAtRootIsSymlink handles that
		// case. ReadFile would follow the link and read past it, so
		// refuse here rather than double-count the shape.
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
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return sameProjectPath(target, commonDir)
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
// creates under <commonDir>/worktrees/<name>, so a worktree entry whose
// backlink closes back onto this <commonDir>/worktrees/<name> proves this
// main's git common directory backs more than this single checkout, and the
// marker at commonDir is shared with every such worktree. Deleting it would
// break identity resolution for them — the same hole sharedWorktreeCommonDir
// guards the deletion advice against on the linked-worktree side.
//
// A repository that has spawned linked worktrees can also be COPIED wholesale
// over a registered root (cp -R): the private copy carries both the owner's
// checkout marker and a <commonDir>/worktrees/<name> subtree copied along
// with it, but the gitdir file inside each copied <name> still points at the
// ORIGINAL worktree's <root>/.git, and that <root>/.git file's gitdir pointer
// in turn still names the ORIGINAL common directory's worktrees path, not
// this copy's. Treating any such copied entry as proof of active sharing
// makes the claimed-marker path refuse the safe marker-removal/rebind
// recovery for the copy indefinitely, incorrectly saying the copy spawned
// shared worktrees when the linked worktrees still belong to the original.
// So each entry is validated by following its gitdir file through the
// worktree's .git file and checking that the round trip lands back on this
// <commonDir>/worktrees/<name>; a copied or stale entry whose backlink
// points elsewhere or is gone does not count. Indeterminate failures (a
// worktree entry that exists but cannot be read for permissions or transient
// I/O, or whose gitdir or .git file is unreadable for any reason other than
// not-exist) are not "missing" — they may still name a live linked
// worktree — so fail closed the same way the read-error branch below does.
func mainCheckoutHasLinkedWorktrees(commonDir string) bool {
	worktreesDir := filepath.Join(commonDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
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
	for _, entry := range entries {
		switch worktreeEntryBacklinkState(worktreesDir, entry.Name()) {
		case worktreeBacklinkLive:
			return true
		case worktreeBacklinkIndeterminate:
			return true
		case worktreeBacklinkStale:
			continue
		}
	}
	return false
}

// worktreeBacklinkResult classifies a single entry under
// <commonDir>/worktrees/<name> by whether the entry's backlink closes back
// onto this common directory.
type worktreeBacklinkResult int

const (
	// worktreeBacklinkStale is the determinate-not-live result: gitdir's
	// target's .git file points at another common directory (copied
	// metadata) or is gone (the worktree was removed, leaving the entry
	// behind). The entry is not proof of a live linked worktree this
	// common directory backs, so the caller can disregard it.
	worktreeBacklinkStale worktreeBacklinkResult = iota
	// worktreeBacklinkLive is the determinate-live result: the entry's
	// gitdir closes back onto this <commonDir>/worktrees/<name>, proving
	// a linked worktree git has spawned from this main.
	worktreeBacklinkLive
	// worktreeBacklinkIndeterminate is the can't-tell result: gitdir or the
	// .git file is present but unreadable, or the worktree entry's gitdir
	// file is missing on a filesystem that may still back the entry. The
	// caller fails closed rather than skip.
	worktreeBacklinkIndeterminate
)

// worktreeEntryBacklinkState reads <commonDir>/worktrees/<name>/gitdir and
// follows it to the linked worktree's <root>/.git file, then reads that
// .git file's `gitdir: <path>` line and checks whether the canonicalized
// target closes back onto this <commonDir>/worktrees/<name>. A real linked
// worktree git has spawned from this main closes the loop; a copied or stale
// entry (the worktree was removed, or its gitdir was copied from another
// common directory whose worktree's .git still points at the original) does
// not. Missing/unreadable gitdir or .git files are indeterminate: the entry
// may still back a live worktree whose metadata just cannot be read, so the
// caller fails closed on those the way mainCheckoutHasLinkedWorktrees fails
// closed on an unreadable worktrees directory.
func worktreeEntryBacklinkState(worktreesDir, name string) worktreeBacklinkResult {
	entryDir := filepath.Join(worktreesDir, name)
	gitdirPath := filepath.Join(entryDir, "gitdir")
	gitdirContent, err := os.ReadFile(gitdirPath)
	if err != nil {
		// No gitdir file: this entry is not git worktree metadata. On
		// filesystems that return indeterminate entry types (NFS, FUSE)
		// the entry may still back a real linked worktree whose metadata
		// just cannot be classified by IsDir, so fail closed rather than
		// read the missing file as proof the entry is stale.
		if errors.Is(err, os.ErrNotExist) {
			return worktreeBacklinkIndeterminate
		}
		// Unreadable permissions/transient I/O: not determinate; may still
		// be live.
		return worktreeBacklinkIndeterminate
	}
	worktreePath := filepath.Clean(strings.TrimSpace(string(gitdirContent)))
	if !filepath.IsAbs(worktreePath) {
		worktreePath = filepath.Join(entryDir, worktreePath)
	}
	data, err := os.ReadFile(worktreePath)
	if err != nil {
		// The worktree's .git file is gone: the worktree was removed, so
		// the entry is stale metadata left behind. Read past missing as
		// "stale" so the entry no longer blocks the deletion advice; any
		// other read error is indeterminate.
		if errors.Is(err, os.ErrNotExist) {
			return worktreeBacklinkStale
		}
		return worktreeBacklinkIndeterminate
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		// Not a git worktree's .git file: the recorded worktree path has
		// been replaced by a fresh main checkout's .git directory or some
		// other file. The original linked worktree is gone; the entry is
		// stale.
		return worktreeBacklinkStale
	}
	target := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktreePath, target)
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	entryResolved := entryDir
	if resolved, err := filepath.EvalSymlinks(entryDir); err == nil {
		entryResolved = resolved
	}
	if sameProjectPath(target, entryResolved) {
		return worktreeBacklinkLive
	}
	return worktreeBacklinkStale
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

// directGitDirFileMarkerRefusal is the claimed-marker branch's refusal for a
// <root>/.git that is a REGULAR "gitdir: <path>" file pointing DIRECTLY at
// binding.gitCommonDir rather than a symlink to it. Two checkouts both using
// `git init --separate-git-dir <commonDir>` (or two regular .git files
// written by hand to point at the same external directory) share
// binding.gitCommonDir the same way the symlinked case does, and
// sharedWorktreeCommonDir and mainCheckoutHasLinkedWorktrees both miss the
// shape: a target equal to commonDir gives the rel ".." the
// linked-worktree predicate rejects, and the common dir may have no
// worktrees subdir. Without this guard the marker at
// binding.checkoutMarkerPath would fall through to the copied-marker
// deletion remedy even though another registered checkout may share it
// through the same external git directory. The returned error is the
// fail-closed refusal; nil means <root>/.git is not a direct gitdir file at
// commonDir and the caller should fall through to the copied-marker
// remedy.
func directGitDirFileMarkerRefusal(binding projectBinding, p Project, checkoutID string, owner Project) error {
	if !gitDirAtRootPointsAtCommonDir(binding.root, binding.gitCommonDir) {
		return nil
	}
	return fmt.Errorf(
		"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
			"the marker belongs to project %s, and project %s's registered root %s still carries a matching marker, "+
			"but this checkout's <root>/.git is a regular gitdir file pointing directly at the external git directory another registered checkout may share, "+
			"and the marker here may be project %s's own shared marker rather than a private copy; "+
			"af cannot tell which marker is the copy, so it will not recommend removing this one — "+
			"move this checkout to a path that does not share its git directory, or restore project %s's root so its own .git is private",
		binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, owner.Root, owner.ID, owner.ID)
}
