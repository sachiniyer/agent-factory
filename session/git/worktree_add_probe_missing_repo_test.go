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
// so a missing repo (EvalSymlinks returns ENOENT) skips the creatable-parent and
// ancestor probes; the existing-destination probe and the worktree parent are
// unaffected (the worktree parent is under the missing repo, so the git failure
// reaches it as a structural ENOENT rather than a write probe the classifier reads).
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

// TestAnnotateWorktreeAddError_SymlinkedDestinationDirtiedByFailedAddIsProbed pins the
// contract the #5421 review added: when the destination is a symlink to a directory a
// failed `git worktree add` started populating, the nonempty target is partial-attempt
// debris (a `.git` entry git created before the add failed) rather than a pre-existing
// destination git rejects without writing. The empty-dir gate would otherwise skip the
// probe on the nonempty target, and a coincidental ENOSPC on the target's filesystem
// (the real resource errno the failed add hit) would be lost — the lost-restore loop
// would retry a structural "already exists" rejection that freeing the target cannot
// fix. Retain coverage when the contents show the failed worktree add started
// populating the target (a `.git` entry), the way an empty target is probed.
//
// Deterministic in both directions: against the form that skipped every nonempty
// target the write probe never targets the symlinked destination and the test fails;
// with the `.git`-debris check the probe targets the symlinked destination and the
// ENOSPC reaches the chain.
func TestAnnotateWorktreeAddError_SymlinkedDestinationDirtiedByFailedAddIsProbed(t *testing.T) {
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
	// directory whose contents are partial-attempt debris from a failed
	// `git worktree add`: a `.git` entry the add created before it failed with
	// ENOSPC mid-checkout. `git worktree add` rejects a pre-existing nonempty
	// destination without writing, but a target the failed add started populating
	// is one git already wrote into, so a full filesystem there is the real
	// resource errno the lost-restore classifier must read.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))
	// The partial-attempt debris: a `.git` *file* (a gitfile pointing at the
	// worktree's gitdir) the failed add created before it failed with ENOSPC
	// mid-checkout, plus a partial checkout file. `git worktree add` creates a
	// `.git` file, not a `.git` directory; a `.git` directory is a pre-existing
	// repository that git rejects without writing.
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
	// rather than blindly probing every symlinked directory. The empty-dir
	// read reports a nonempty target (the debris), so the `.git`-debris check
	// is the only thing that retains coverage for a target the failed add
	// started populating.
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
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a nonempty target the failed `git worktree add` started populating (a `.git` entry) "+
			"must be probed through to its target — a full filesystem there is the real ENOSPC the "+
			"failed add hit, not a coincidental ENOSPC on a pre-existing destination git rejects "+
			"without writing: %v", out)
	require.Contains(t, probedDirs, destPath,
		"the write probe must target the symlinked destination (%s) — its target's filesystem is "+
			"one the failed add already wrote into, so a full filesystem there is the real resource "+
			"errno the lost-restore classifier must read", destPath)
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationPreExistingRepoSkipsProbe pins the
// contract the #5421 review added: an unrelated pre-existing Git repository contains a
// `.git` *directory* (its object store), not a `.git` *file* (the gitfile `git worktree
// add` creates). When the final destination symlink points to such a nonempty repository,
// `git worktree add` rejects it as already existing without writing there, so the
// empty-dir gate must not probe the target: if that filesystem is coincidentally full,
// the resulting ENOSPC is a structural "already exists" rejection freeing the target
// cannot fix, not the real resource errno of a failed add. The earlier `.git`-existence
// check probed every nonempty target with a `.git` entry and turned that coincidental
// ENOSPC into an unbounded environmental wait; this test pins that only a `.git` *file*
// (partial-attempt debris from a failed add) retains coverage.
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
