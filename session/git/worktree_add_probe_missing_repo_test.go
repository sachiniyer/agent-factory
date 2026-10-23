package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// TestAnnotateWorktreeAddError_MissingRepoPathSkipsCreatableParentProbe pins the
// contract the #5421 review added: a restored record with a relative repoPath that
// no longer exists (the user deleted it after the session was saved) is a structural
// failure (git cannot enter its cmd.Dir), so the worktree-prefix walker must not
// treat the missing repository as a creatable prefix and write-probe its existing
// parent. The existing-ancestor search would otherwise walk up to the missing
// repo's parent and probe it, and a coincidental ENOSPC on that parent would turn
// the structural missing-repository failure into an indefinite environmental retry.
// The existence check uses the bounded EvalSymlinks flight the walk already uses,
// so a missing repo (EvalSymlinks returns ENOENT) skips all worktree-location
// probes (the creatable-parent, ancestor, destination, and parent probes): the
// git child cannot enter its cmd.Dir, so it exits before creating the destination,
// and a relative path that traverses above the missing repo (for example ../dest)
// resolves outside it, so any ENOSPC on the resolved destination's filesystem is
// unrelated and would convert the structural missing-repository failure into an
// indefinite environmental retry.
//
// Deterministic in both directions: against the form that probed the missing repo's
// parent a write probe mocked to fail only there surfaces ENOSPC and the test fails;
// with the repo-boundary gate the write probe never targets the missing repo's
// parent, the ENOSPC never reaches the chain, and the structural git failure is the
// only answer.
func TestAnnotateWorktreeAddError_MissingRepoPathSkipsCreatableParentProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The repo is intentionally NOT created: a restored record with a relative
	// repoPath that no longer exists. The git child cannot enter its cmd.Dir,
	// so the worktree add is a structural missing-repository failure before it
	// writes anything.

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// The mock stat returns ENOENT for every path (the existing-destination
	// probe stays silent), the way the existing tests do, but the
	// evalSymlinksSpaceProbe seam is the real fsprobe.EvalSymlinks so the
	// repo-boundary gate sees the missing repo and skips the creatable-parent
	// and ancestor probes. A write probe mocked to fail only on the missing
	// repo's parent (the launch cwd) surfaces the bug: without the gate the
	// ancestor walk or a creatable parent would probe the launch cwd and the
	// ENOSPC would turn a structural missing-repo failure into an environmental
	// retry.
	statSpaceProbe = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	resolvedLaunchCwd, err := filepath.EvalSymlinks(launchCwd)
	require.NoError(t, err)
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == resolvedLaunchCwd {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a missing repo is a structural failure (git cannot enter its cmd.Dir), so the "+
			"missing repo's parent must not be probed the way a creatable worktree parent is — "+
			"a coincidental ENOSPC on the launch cwd would turn the structural missing-repository "+
			"failure into an indefinite environmental retry: %v", out)
	require.NotContains(t, probedDirs, resolvedLaunchCwd,
		"the missing repo's parent (the launch cwd) must not be probed when the repo does not "+
			"exist — git never writes there, so a full filesystem on it cannot fix the structural "+
			"missing-repository failure and must not reach the chain")
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationOrphanedGitfileSkipsProbe pins the
// contract the #5421 review added: a nonempty target whose `.git` *file* (a gitfile) points
// at an absent gitdir is NOT partial-attempt debris from a failed `git worktree add`. `git
// worktree add` creates the gitdir before the gitfile, so a present gitfile with an absent
// gitdir is a stale linked worktree whose registration was pruned or deleted — a
// pre-existing destination git rejects the add over without writing, not one the failed
// add started populating. The empty-dir gate skips the nonempty target, and the `.git`
// file must not re-enable the probe: a coincidental ENOSPC on the target's filesystem is
// not the real resource errno of a failed add, so it must not turn the structural
// "already exists" rejection into an indefinite environmental retry.
//
// Deterministic in both directions: against the form that treated every orphaned gitfile
// as debris the write probe targeted the symlinked destination and the ENOSPC reached the
// chain, so the test failed; with the gitdir-absence-is-not-debris fix the write probe
// never targets the destination and the ENOSPC never reaches the chain.
func TestAnnotateWorktreeAddError_SymlinkedDestinationOrphanedGitfileSkipsProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The relative worktree path's final component is a symlink to a nonempty
	// directory whose `.git` *file* points at a gitdir that no longer exists:
	// `git worktree add` creates the gitdir before the gitfile, so a present
	// gitfile with an absent gitdir is a stale linked worktree whose
	// registration was pruned (git rejected the add over a pre-existing
	// destination without writing there), not a failed add's partial-attempt
	// debris. A coincidental ENOSPC on the target's filesystem cannot fix the
	// structural "already exists" rejection and must not stall the lost-restore
	// loop.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// A `.git` *file* (a gitfile) pointing at a gitdir that does not exist —
	// the registration of a stale linked worktree, not a failed add's gitfile.
	// `git worktree add` creates a `.git` file, not a `.git` directory; a
	// `.git` directory is a pre-existing repository that git rejects without
	// writing.
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, ".git"), []byte("gitdir: /repo/.git/worktrees/wt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "partial"), []byte("x"), 0o644))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	// Stat follows the symlink and reports a directory (the nonempty target);
	// Lstat reports the symlink itself so the gate reads the target's entries
	// rather than blindly probing every symlinked directory. The empty-dir read
	// reports a nonempty target, so the `.git`-file check is the only thing that
	// could re-enable the probe — an orphaned gitfile must not.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target whose `.git` file points at an absent gitdir is a stale linked "+
			"worktree git rejected the add over (git creates the gitdir before the gitfile, so an "+
			"absent gitdir is not a failed add's debris), not a target the failed add started "+
			"populating — a coincidental ENOSPC on its filesystem must not turn the structural "+
			"'already exists' rejection into an environmental retry: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target the symlinked destination (%s) — its `.git` file points "+
			"at an absent gitdir, so the destination already existed and git rejected the add "+
			"without writing there", destPath)
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationPreExistingRepoSkipsProbe pins the
// contract the #5421 review added: an unrelated pre-existing Git repository contains a
// `.git` *directory* (its object store), not a `.git` *file* (the gitfile `git worktree
// add` creates). When the final destination symlink points to such a nonempty repository,
// `git worktree add` rejects it as already existing without writing there, so the
// empty-dir gate must not probe the target: if that filesystem is coincidentally full,
// the resulting ENOSPC is a structural "already exists" rejection freeing the target
// cannot fix, not the real resource errno of a failed add. A `.git` *file* (a gitfile)
// with an absent gitdir is also not partial-attempt debris: `git worktree add` creates the
// gitdir before the gitfile, so an absent gitdir is a stale linked worktree git rejected
// the add over, not a failed add's debris — see the orphaned-gitfile test for that case.
func TestAnnotateWorktreeAddError_SymlinkedDestinationPreExistingRepoSkipsProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The relative worktree path's final component is a symlink to a nonempty
	// directory that is an unrelated pre-existing Git repository: its `.git` is a
	// directory (the object store), not the `.git` file a `git worktree add`
	// creates. Git rejects the add without writing, so a coincidental ENOSPC on
	// the target's filesystem is a structural "already exists" rejection freeing
	// the target cannot fix — the probe must not target it.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// A pre-existing repository: a `.git` directory, not a `.git` file.
	require.NoError(t, os.Mkdir(filepath.Join(realTarget, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "README"), []byte("x"), 0o644))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a pre-existing repository's `.git` directory is not partial-attempt debris from a failed "+
			"`git worktree add` (which creates a `.git` file), so a coincidental ENOSPC on its "+
			"filesystem must not turn the structural 'already exists' rejection into an environmental "+
			"retry: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target a symlinked destination whose nonempty target is a "+
			"pre-existing repository (`.git` directory) — git rejects the add without writing there, "+
			"so a full filesystem on that target cannot fix the structural failure")
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationRegisteredWorktreeSkipsProbe pins the
// contract the #5421 review added: an already-registered linked worktree also has a `.git`
// *file* (its gitdir registration), not just a `.git` *directory* (a pre-existing
// repository). When the final destination symlink points to such a nonempty worktree, git
// rejects the add as already existing without writing there, so a coincidental ENOSPC on the
// target's filesystem is not the real resource errno of a failed add. A reachable gitdir does
// not prove this failed invocation created the gitfile (the worktree could predate this add,
// including one registered concurrently after the setup cleanup), so the probe must not
// target the destination the way it must for an orphaned gitfile from a failed add whose
// gitdir was cleaned up.
//
// Deterministic in both directions: against the form that treated every non-directory `.git`
// as partial-attempt debris the write probe targeted the symlinked destination and returned
// ENOSPC, so the test failed; with the gitdir validation the write probe never targets the
// destination, the ENOSPC never reaches the chain, and the structural git failure is the
// only answer.
func TestAnnotateWorktreeAddError_SymlinkedDestinationRegisteredWorktreeSkipsProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The relative worktree path's final component is a symlink to a nonempty
	// directory that is an already-registered linked worktree: its `.git` file
	// points at a live gitdir (the worktree's registration), so git rejects the
	// add as already existing without writing. A reachable gitdir does not prove
	// this failed invocation created the gitfile (the worktree could predate this
	// add), so a coincidental ENOSPC on the target's filesystem is not the real
	// resource errno of a failed add, so the probe must not target it.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// The `.git` file points at a live gitdir: the registration of an
	// already-registered worktree, not partial-attempt debris from a failed add
	// whose gitdir was cleaned up. The gitdir exists, so the worktree is
	// registered and git rejects the add without writing.
	gitDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(gitDir, "gitdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "README"), []byte("x"), 0o644))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	// Stat follows the symlink and reports a directory (the nonempty target);
	// Lstat reports the symlink itself so the gate reads the target's entries
	// rather than blindly probing every symlinked directory. The empty-dir
	// read reports a nonempty target (the registered worktree), so the
	// `.git`-file check is the only thing that retains coverage — and a live
	// gitdir means it is not partial-attempt debris.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target whose `.git` file points at a live gitdir is an already-registered "+
			"worktree git rejects the add for without writing, not partial-attempt debris from a "+
			"failed add — a coincidental ENOSPC on its filesystem must not turn the structural 'already "+
			"exists' rejection into an environmental retry: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target a symlinked destination whose `.git` file points at a "+
			"live gitdir (an already-registered worktree) — git rejects the add without writing there, "+
			"so a full filesystem on that target cannot fix the structural failure")
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationGitfileReadErrorPreserved pins the
// contract the #5421 review added: when a failed add has left a nonempty symlink target whose
// regular `.git` file stalls or returns an I/O error, the bounded read error must be
// preserved as environmental evidence — the same way a non-ENOENT lstat or stat error on the
// gitdir is preserved — so the lost-restore classifier reads the error alongside the git
// failure rather than charging the structural budget for a filesystem that did not answer.
// Discarding the read error (converting it to ok == false) would leave no evidence from the
// affected target when the parent and common directory remain writable.
func TestAnnotateWorktreeAddError_SymlinkedDestinationGitfileReadErrorPreserved(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// A nonempty target whose `.git` entry is a regular file the bounded
	// read cannot answer. The target is nonempty so the empty-dir gate
	// falls through to the `.git`-file branch, the lstat reports a regular
	// file, but the read returns an error the caller must preserve.
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "file"), []byte("x"), 0o644))
	// A regular file whose Stat is used as the fake lstat result for the
	// non-existent `.git` entry — the code sees a regular file and calls
	// BoundedReadFileLimit, which fails because the file does not exist.
	fakeInfo, err := os.Stat(filepath.Join(realTarget, "file"))
	require.NoError(t, err)

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) {
		if p == filepath.Join(destPath, ".git") {
			return fakeInfo, nil
		}
		return os.Lstat(p)
	}
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target whose `.git` file could not be read must not be probed as "+
			"partial-attempt debris — the read failure is environmental evidence, not proof of a "+
			"gitfile this add created: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target a symlinked destination whose `.git` file could not be "+
			"read — the read error is evidence, not debris, so the destination must not be probed")
	require.Error(t, out,
		"a bounded read error on the `.git` file must be preserved as environmental evidence so "+
			"the lost-restore classifier reads it alongside the git failure: %v", out)
}

// TestAnnotateWorktreeAddError_AbsoluteWorktreePathTraversalResolvesSymlink pins the contract
// the #5421 review added: an absolute worktree path containing a symlink followed by `..`
// (for example /base/link/../wt with /base/link -> /mnt) must run the symlink-aware walk the
// way a relative path does, not the lexical filepath.Dir that collapses the `..` before the
// symlink is followed. A lexical filepath.Dir would probe the path's lexical parent (/base)
// rather than the resolved parent (the symlink target's parent, /mnt), so a coincidental
// ENOSPC on the filesystem git actually writes to would be missed and the restore charged to
// the structural retry budget.
//
// Deterministic in both directions: against the form that left resolveRelative false for
// absolute paths the write probe targeted the lexical parent and missed the resolved
// parent, so the test failed; with the symlink-aware walk the probe targets the resolved
// parent and the ENOSPC reaches the chain.
func TestAnnotateWorktreeAddError_AbsoluteWorktreePathTraversalResolvesSymlink(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// An absolute worktree path with a symlink followed by `..`: the symlink
	// base/link points at other/sub, so base/link/../wt resolves to other/wt
	// (the symlink is followed before `..` collapses, the way the kernel's
	// chdir does). filepath.Dir would lexically collapse the `..` and probe
	// /base (the lexical parent), missing the filesystem git actually writes
	// to. The symlink-aware walk resolves the symlink so the probe targets
	// other (the resolved parent).
	base := t.TempDir()
	other := t.TempDir()
	require.NotEqual(t, base, other)
	require.NoError(t, os.MkdirAll(filepath.Join(other, "sub"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(other, "sub"), filepath.Join(base, "link")))
	// filepath.Join would lexically collapse the `..` to base/wt, so construct
	// the path with the `..` preserved the way a restored session record would.
	worktreePath := base + string(filepath.Separator) + "link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "wt"

	// Resolve the symlink target the way the production code does so the
	// expected path matches across hosts (macOS /var/folders ->
	// /private/var/folders).
	resolvedOther, err := filepath.EvalSymlinks(filepath.Join(other, "sub"))
	require.NoError(t, err)
	resolvedParent := filepath.Dir(resolvedOther)
	lexicalParent := base // filepath.Join collapses base/link/.. to base

	statSpaceProbe = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == resolvedParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}

	g := &GitWorktree{repoPath: base, worktreePath: worktreePath}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"an absolute worktree path with a symlink followed by `..` must resolve the symlink the "+
			"way git's chdir does, so a coincidental ENOSPC on the resolved parent (the filesystem "+
			"git actually writes to) reaches the chain: %v", out)
	require.Contains(t, probedDirs, resolvedParent,
		"the write probe must target the resolved parent (the filesystem git actually writes to), "+
			"not the lexical parent the symlink collapses to")
	require.NotContains(t, probedDirs, lexicalParent,
		"the lexical parent (base) must not be probed — the symlink resolves to a different "+
			"filesystem, so a coincidental ENOSPC there would be unrelated to the git failure")
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationNonGitfileSkipsProbe pins the contract the
// #5421 review added: a nonempty symlinked destination whose `.git` file is not a gitfile (its
// contents lack the `gitdir:` prefix, for example an ordinary file such as README) must not be
// treated as failed-add debris. strings.TrimPrefix returns the original contents when the prefix
// is absent, so without an explicit HasPrefix check any nonempty regular `.git` file would be
// parsed as a gitdir path, the derived path would usually stat as missing, and the destination
// would be write-probed even though git rejects a pre-existing destination without writing there.
func TestAnnotateWorktreeAddError_SymlinkedDestinationNonGitfileSkipsProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// A regular `.git` file whose contents lack the `gitdir:` prefix — not a
	// gitfile from `git worktree add`, so the destination is pre-existing content
	// git rejects without writing, not partial-attempt debris.
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, ".git"), []byte("README"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "file"), []byte("x"), 0o644))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target whose `.git` file is not a gitfile (no `gitdir:` prefix) is pre-existing "+
			"content git rejected the add over, not partial-attempt debris — a coincidental ENOSPC on "+
			"its filesystem must not turn the structural 'already exists' rejection into an environmental "+
			"retry: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target a symlinked destination whose `.git` file is not a gitfile — "+
			"git rejects the add without writing there, so a full filesystem on that target cannot fix the "+
			"structural failure")
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationLiveGitdirNonENOENTSkipsProbe pins the
// contract the #5421 review added: a `.git` file pointing at a live gitdir whose stat returns a
// non-ENOENT error (EACCES, EIO, ErrProbeTimedOut) must not be treated as orphaned debris. Only
// ENOENT establishes an orphaned gitfile; a live but temporarily inaccessible gitdir means the
// worktree is registered and git rejected the add without writing, so the probe must not target
// the destination and the stat error must be preserved so the classifier reads it as
// environmental evidence rather than bare structural silence.
func TestAnnotateWorktreeAddError_SymlinkedDestinationLiveGitdirNonENOENTSkipsProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// A `.git` file pointing at a live gitdir whose stat returns a non-ENOENT
	// error (EACCES) — the worktree is registered, so git rejects the add
	// without writing; the inaccessible gitdir is environmental evidence, not
	// an orphaned gitfile.
	gitDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "README"), []byte("x"), 0o644))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	statErr := errors.New("gitdir stat: permission denied")
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		if p == gitDir {
			return nil, statErr
		}
		return os.Stat(p)
	}
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target whose `.git` file points at a live but inaccessible gitdir is an "+
			"already-registered worktree git rejects the add for without writing, not orphaned "+
			"debris — a coincidental ENOSPC on its filesystem must not turn the structural 'already "+
			"exists' rejection into an environmental retry: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target a symlinked destination whose `.git` file points at a "+
			"live gitdir whose stat returned a non-ENOENT error — the gitdir is inaccessible, not "+
			"orphaned, so the destination is not failed-add debris")
	require.True(t, errors.Is(out, statErr),
		"a non-ENOENT stat error on a live gitdir must be preserved as environmental evidence so "+
			"the lost-restore classifier reads it alongside the git failure: %v", out)
}

// TestAnnotateWorktreeAddError_AbsoluteWorktreePathTrailingSepPreservesFinalSymlink pins the
// contract the #5421 review added for an absolute worktree path ending in a separator: the
// trailing-separator and final-dot preservation the resolveRelative block applies to a relative
// path must also apply to an absolute path. Without it, filepath.Dir collapses the trailing
// separator and os.Lstat follows the final symlink to its nonempty target, so the
// destination-directory probe write-probes a target git rejects without writing — a coincidental
// ENOSPC on that target would stall the lost-restore loop on a structural "already exists"
// rejection.
func TestAnnotateWorktreeAddError_AbsoluteWorktreePathTrailingSepPreservesFinalSymlink(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// An absolute worktree path ending in a separator: the final component is
	// a symlink to a nonempty directory, so `git worktree add` rejects the add
	// without writing to the symlink target. The destination probe must not
	// follow the trailing separator and write-probe the target.
	base := t.TempDir()
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(base, "link"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(base, "link")))
	worktreePath := filepath.Join(base, "link") + string(filepath.Separator)

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }

	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	symlinkTargetParent := filepath.Dir(resolvedRealTarget)
	require.NotEqual(t, symlinkTargetParent, base,
		"the symlink target's parent must differ from the base dir, otherwise the test cannot "+
			"distinguish the fix from the bug")

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == symlinkTargetParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: base, worktreePath: worktreePath}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem at the symlink target's parent must not add ENOSPC — git rejected the "+
			"add because the destination already exists, so the trailing separator must not make the "+
			"probe follow the symlink and write-probe the target: %v", out)
	require.NotContains(t, probedDirs, symlinkTargetParent,
		"the write probe must not target the symlink target's parent (the trailing separator must "+
			"not make Lstat follow the final symlink the way a non-trailing-separator path does not)")
}
