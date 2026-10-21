package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/stretchr/testify/require"
)

// TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsDirTraversalAfterNonDirectory pins the
// contract the #5421 review added: when a relative worktree path's tail traverses a symlink that
// resolves to a regular file (for example `repo/link -> /mnt/file` with `worktreePath =
// missing/../link/../wt`), the kernel's chdir does not traverse `..` through a non-directory —
// `git worktree add` fails with "Not a directory" before writing there — so the tail walk must
// surface ENOTDIR the way the kernel does instead of applying filepath.Dir to the regular
// file's path. A lexical filepath.Dir would back out to the regular file's parent and the
// subsequent probe would target a filesystem git never reaches, so a coincidental ENOSPC on that
// parent would be joined to a structural ENOTDIR failure and stall the lost-restore loop
// indefinitely on a filesystem freeing it cannot fix.
//
// Deterministic in both directions: against the form that applied filepath.Dir to a non-directory
// the probe targeted the regular file's parent and a write probe mocked to fail only there
// returned ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with the
// ENOTDIR guard the regular file's parent is never probed, the ENOTDIR reaches the chain, and
// the ENOSPC never does.
func TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsDirTraversalAfterNonDirectory(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link points at a regular file on a distinct filesystem; `git
	// worktree add missing/../link/../wt` creates `missing`, backs out to the
	// repo, follows `link` to the regular file, and then fails with "Not a
	// directory" traversing `..` through it before writing anywhere. The tail
	// walk resolves `link` to the regular file and must surface ENOTDIR
	// rather than backing out lexically to the regular file's parent, which a
	// full filesystem cannot fix and would otherwise stall the lost-restore
	// loop on a structural ENOTDIR failure.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	regularTarget := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(regularTarget, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(regularTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlink target the way the production code does so the
	// expected path matches across hosts (macOS /var/folders ->
	// /private/var/folders). The lexical parent the buggy filepath.Dir
	// backs out to is the regular file's parent — the filesystem git never
	// writes to for this path.
	resolvedTarget, err := filepath.EvalSymlinks(regularTarget)
	require.NoError(t, err)
	regularFileParent := filepath.Dir(resolvedTarget)

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the regular file's parent is the bug: git
		// fails with ENOTDIR before writing there, so freeing it cannot fix
		// the structural failure and the probe must not target it.
		if dir == regularFileParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "missing/../link/../wt"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a `..` after a non-directory must surface ENOTDIR the way the kernel does, not back out "+
			"lexically to the regular file's parent — git fails with \"Not a directory\" before writing "+
			"there, so a coincidental ENOSPC on that parent must not stall the lost-restore loop on a "+
			"structural ENOTDIR failure: %v", out)
	require.NotContains(t, probedDirs, regularFileParent,
		"the regular file's parent the non-directory traversal would back out to must not be probed — "+
			"git never writes there for this path")
	require.True(t, errors.Is(out, syscall.ENOTDIR),
		"the non-directory traversal must surface ENOTDIR so the lost-restore classifier reads the "+
			"structural failure rather than probing a filesystem git never reaches: %v", out)
	require.Contains(t, out.Error(), "symlink resolution of",
		"the non-directory resolution error must be named in the annotation, not dropped: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsFinalDirTraversalAfterNonDirectory pins the
// contract the final-`..` case of the #5421 review added: when a relative worktree path's final
// component is `..` after a symlink that resolves to a regular file (for example `repo/link ->
// /mnt/file` with `worktreePath = link/..`), the kernel's chdir does not traverse `..` through a
// non-directory — `git worktree add link/..` fails with "Not a directory" before writing there — so
// the join of the resolved parent and the final `..` must surface ENOTDIR the way the tail walk
// does instead of lexically collapsing to the regular file's parent. The final `..` is held in
// base after filepath.Split and never reaches resolveSymlinkTail's non-directory check, so
// filepath.Join would collapse resolvedParent/.. to the regular file's parent and the subsequent
// probe would target a filesystem git never reaches, so a coincidental ENOSPC on that parent would
// be joined to a structural ENOTDIR failure and stall the lost-restore loop indefinitely on a
// filesystem freeing it cannot fix.
//
// Deterministic in both directions: against the form that applied filepath.Join to a non-directory
// the probe targeted the regular file's parent and a write probe mocked to fail only there returned
// ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with the ENOTDIR guard
// the regular file's parent is never probed, the ENOTDIR reaches the chain, and the ENOSPC never
// does.
func TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsFinalDirTraversalAfterNonDirectory(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link points at a regular file on a distinct filesystem; `git
	// worktree add link/..` follows `link` to the regular file and then fails
	// with "Not a directory" traversing `..` through it before writing
	// anywhere. The final `..` is held in base after the parent walk resolves
	// `link`, so it never reaches resolveSymlinkTail's non-directory check; the
	// join must surface ENOTDIR rather than collapsing to the regular file's
	// parent, which a full filesystem cannot fix and would otherwise stall the
	// lost-restore loop on a structural ENOTDIR failure.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	regularTarget := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(regularTarget, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(regularTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlink target the way the production code does so the
	// expected path matches across hosts (macOS /var/folders ->
	// /private/var/folders). The lexical parent the buggy filepath.Join
	// collapses to is the regular file's parent — the filesystem git never
	// writes to for this path.
	resolvedTarget, err := filepath.EvalSymlinks(regularTarget)
	require.NoError(t, err)
	regularFileParent := filepath.Dir(resolvedTarget)

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the regular file's parent is the bug: git
		// fails with ENOTDIR before writing there, so freeing it cannot fix
		// the structural failure and the probe must not target it.
		if dir == regularFileParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/.."}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a final `..` after a non-directory must surface ENOTDIR the way the kernel does, not collapse "+
			"lexically to the regular file's parent — git fails with \"Not a directory\" before writing "+
			"there, so a coincidental ENOSPC on that parent must not stall the lost-restore loop on a "+
			"structural ENOTDIR failure: %v", out)
	require.NotContains(t, probedDirs, regularFileParent,
		"the regular file's parent the non-directory traversal would collapse to must not be probed — "+
			"git never writes there for this path")
	require.True(t, errors.Is(out, syscall.ENOTDIR),
		"the final `..` after a non-directory must surface ENOTDIR so the lost-restore classifier reads the "+
			"structural failure rather than probing a filesystem git never reaches: %v", out)
	require.Contains(t, out.Error(), "symlink resolution of",
		"the non-directory resolution error must be named in the annotation, not dropped: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsFinalDotTraversalAfterNonDirectory pins the
// contract the `.`-traversal case of the #5421 review added: when a relative worktree path's final
// component is `.` after a symlink that resolves to a regular file (for example `repo/link -> /mnt/file`
// with `worktreePath = link/.`), the kernel's chdir does not traverse `.` through a non-directory —
// `cd file/.` (and `git worktree add link/.`) fails with "Not a directory" before writing there — so
// the join of the resolved parent and the final `.` must surface ENOTDIR the way the `..` guard does
// instead of lexically collapsing to the regular file's path. The final `.` is held in base after
// filepath.Split and never reaches resolveSymlinkTail's non-directory check, so filepath.Join would
// collapse resolvedParent/. to the regular file and the subsequent probe would target a filesystem
// git never reaches, so a coincidental ENOSPC on that filesystem would stall the lost-restore loop
// indefinitely on a structural ENOTDIR failure.
//
// Deterministic in both directions: against the form that applied filepath.Join to a non-directory
// the probe targeted the regular file's parent and a write probe mocked to fail only there returned
// ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with the ENOTDIR guard the
// regular file's parent is never probed, the ENOTDIR reaches the chain, and the ENOSPC never does.
func TestAnnotateWorktreeAddError_RelativeWorktreePathRejectsFinalDotTraversalAfterNonDirectory(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link points at a regular file on a distinct filesystem; `git
	// worktree add link/.` follows `link` to the regular file and then fails
	// with "Not a directory" traversing `.` through it before writing
	// anywhere. The final `.` is held in base after the parent walk resolves
	// `link`, so it never reaches resolveSymlinkTail's non-directory check;
	// the join must surface ENOTDIR rather than collapsing to the regular
	// file's path, which a full filesystem cannot fix and would otherwise
	// stall the lost-restore loop on a structural ENOTDIR failure.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	regularTarget := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(regularTarget, []byte("x"), 0o644))
	require.NoError(t, os.Symlink(regularTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlink target the way the production code does so the
	// expected path matches across hosts (macOS /var/folders ->
	// /private/var/folders). The lexical parent the buggy filepath.Join
	// collapses to is the regular file itself — the filesystem git never
	// writes to for this path.
	resolvedTarget, err := filepath.EvalSymlinks(regularTarget)
	require.NoError(t, err)
	regularFileParent := filepath.Dir(resolvedTarget)

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the regular file's parent is the bug: git
		// fails with ENOTDIR before writing there, so freeing it cannot fix
		// the structural failure and the probe must not target it.
		if dir == regularFileParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/."}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a `.` after a non-directory must surface ENOTDIR the way the kernel does, not collapse "+
			"lexically to the regular file's path — git fails with \"Not a directory\" before writing "+
			"there, so a coincidental ENOSPC on that parent must not stall the lost-restore loop on a "+
			"structural ENOTDIR failure: %v", out)
	require.NotContains(t, probedDirs, regularFileParent,
		"the regular file's parent the non-directory `.` traversal would collapse to must not be probed — "+
			"git never writes there for this path")
	require.True(t, errors.Is(out, syscall.ENOTDIR),
		"the `.` after a non-directory must surface ENOTDIR so the lost-restore classifier reads the "+
			"structural failure rather than probing a filesystem git never reaches: %v", out)
	require.Contains(t, out.Error(), "symlink resolution of",
		"the non-directory resolution error must be named in the annotation, not dropped: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathProbesExistingAncestorForNestedDestination
// pins the contract the #5421 review added: when a relative worktreePath has a
// nested destination like wt/dest, git creates the missing leading directory wt
// via safe_create_leading_directories, so an ENOSPC creating it surfaces on the
// filesystem of the existing ancestor (the repo), not on the non-existent wt.
// Before the fix the write probe of the non-existent wt returned ENOENT rather
// than the ENOSPC git encountered, and a common dir on another writable
// filesystem left the opaque git failure structural — the lost-restore loop
// exhausted its retry budget on a full filesystem freeing it could not fix. The
// probe now walks up to the nearest existing ancestor and probes it too, so the
// resource errno reaches the chain the way a write probe of an existing parent
// does.
//
// Deterministic in both directions: against the form that probed only the
// non-existent parent the existing ancestor was never probed, a write probe
// mocked to fail only there returned nil, so errors.Is(out, syscall.ENOSPC)
// was false and the test failed; with the walk-up the ancestor is probed and
// the ENOSPC reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathProbesExistingAncestorForNestedDestination(t *testing.T) {
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

	// The repo exists but the nested worktree parent wt/dest does not — git
	// creates the missing leading directory wt via safe_create_leading
	// directories, so the ENOSPC git encounters creating it is on the repo's
	// filesystem (the existing ancestor), not on the non-existent wt. The
	// write probe of wt returns ENOENT, not ENOSPC, so the ancestor is the
	// only probe that can catch the resource errno.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the launch cwd the way the production code does so the expected
	// paths match across hosts (macOS /var/folders -> /private/var/folders).
	resolvedLaunchCwd, err := filepath.EvalSymlinks(launchCwd)
	require.NoError(t, err)
	resolvedRepo := filepath.Join(resolvedLaunchCwd, "repo")

	// Use the real stat so the walk-up finds the existing ancestor the way
	// production does: the non-existent wt returns ENOENT and the walk
	// ascends to the existing repo.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A full filesystem on the existing ancestor (the repo) is the bug:
		// git creates wt there via safe_create_leading_directories, so the
		// ENOSPC is on this filesystem. A non-existent wt returns ENOENT
		// from the write probe, not ENOSPC, so the ancestor is the only
		// probe that can catch it.
		if dir == resolvedRepo {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a nested relative destination whose leading directory does not exist "+
			"must probe the nearest existing ancestor so a full filesystem there "+
			"(where git creates the missing parent) surfaces the resource errno "+
			"the subprocess's opaque ExitError drops: %v", out)
	require.Contains(t, probedDirs, resolvedRepo,
		"the nearest existing ancestor of the non-existent worktree parent must "+
			"be probed so the ENOSPC git encounters creating it reaches the chain")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathProbesCreatableParentEscapingMount pins the
// contract the #5421 review added: when a relative worktree path's tail creates a missing
// component on a mounted symlink target and a later `..` escapes that mount (for example
// `link/missing/../../dest` with `<repo>/link -> /mnt/x`), git's safe_create_leading_directories
// creates `missing` under the link target (/mnt/x) before the final worktree lands under a
// different filesystem (the target's parent's parent). The existing-parent walk
// (filepath.Dir(worktreePath) upward) ascends from the final destination's parent toward the
// root and never reaches the mount the missing component was created on, so without retaining
// the creatable parent a full filesystem on /mnt/x is missed and the restore consumes its
// structural retry budget. The tail walk retains the existing parent of each creatable
// component it advances past, and the probe write-probes it so a full filesystem on the mount
// git actually wrote the missing directory into surfaces the resource errno the subprocess's
// opaque ExitError drops.
//
// Deterministic in both directions: against the form that did not retain the creatable parent
// the write probe only targeted the final parent and the common dir, a write probe mocked to
// fail only on the link target returned nil, so errors.Is(out, syscall.ENOSPC) was false and
// the test failed; with the creatable parent retained the link target is probed and the ENOSPC
// reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathProbesCreatableParentEscapingMount(t *testing.T) {
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

	// <repo>/link points at a real dir on a distinct location; `missing` does
	// not exist, so the longest existing prefix is <repo>/link (resolved to
	// the link target) and the tail `missing/../..` must create `missing` under
	// the link target (the mount), back out twice to the target's parent's
	// parent, and land there — a different filesystem than the link target
	// where git's safe_create_leading_directories wrote `missing`. A lexical
	// join would collapse `link/missing/../..` to <repo> and miss the mount
	// entirely; the existing-parent walk from the final parent never reaches the
	// link target, so only the creatable parent the tail walk retains can
	// surface ENOSPC on the mount git wrote the missing directory into.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "wt"))
	link := filepath.Join(repo, "link")
	require.NoError(t, os.Symlink(realTarget, link))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Pre-creation: every stat returns ENOENT so the tail walk treats every
	// tail component as creatable the way git's safe_create_leading_directories
	// would, and the existing-destination check stays silent.
	statSpaceProbe = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	// A full filesystem on the link target (the mount where `missing` is
	// created) is the bug: git creates `missing` there via
	// safe_create_leading_directories, so the ENOSPC is on that filesystem.
	// Without the creatable parent only the final parent and the common dir
	// are probed, neither is the link target, and the ENOSPC never reaches the
	// chain.
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == resolvedRealTarget {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/missing/../../dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem on the mounted symlink target where git creates the "+
			"missing component must surface the resource errno: the existing-parent walk "+
			"from the final destination never reaches that mount, so only the creatable "+
			"parent the tail walk retains can probe it: %v", out)
	require.Contains(t, probedDirs, resolvedRealTarget,
		"the existing parent of the creatable `missing` component (the link target, where "+
			"git creates it via safe_create_leading_directories) must be probed when a later "+
			"`..` escapes the mount, not just the final parent and the common dir")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathProbesCreatableParentBeforeDanglingSymlink pins
// the contract the #5421 review added: when the tail walk records a creatable parent and a later
// component makes the final destination unresolvable (a dangling symlink git fails resolving before
// it reaches the final filesystem), the creatable parent the walk already accumulated is still
// probed — git creates the missing component under it via safe_create_leading_directories before
// reaching the dangling link, so a full filesystem on the mount git wrote the missing directory into
// is the resource errno the lost-restore classifier must read even when the final destination is
// structurally unresolvable. The resolution error (dangling symlink) is still joined to the chain so
// the classifier sees it, but the creatable parent is not discarded: the write probe surfaces ENOSPC
// on the mount git wrote the missing directory into.
//
// Deterministic in both directions: against the form that discarded the creatable parents on the
// dangling-symlink error the write probe only targeted the common dir, a write probe mocked to fail
// only on the creatable parent returned nil, so errors.Is(out, syscall.ENOSPC) was false and the test
// failed; with the creatable parent retained on the error path the mount is probed and the ENOSPC
// reaches the chain alongside the dangling-symlink error.
func TestAnnotateWorktreeAddError_RelativeWorktreePathProbesCreatableParentBeforeDanglingSymlink(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link points at a real dir; <realTarget>/dangling is a dangling symlink
	// (its target does not exist). `missing` does not exist, so the longest existing
	// prefix is <repo>/link (resolved to the link target) and the tail
	// `missing/../dangling/..` must create `missing` under the link target (the mount)
	// before reaching the dangling symlink, which makes the final destination
	// unresolvable. The existing parent the walk accumulates for `missing` is the link
	// target; a full filesystem on the mount git wrote the missing directory into must
	// surface ENOSPC even though the dangling symlink makes the tail fail.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(realTarget, "dangling")))
	link := filepath.Join(repo, "link")
	require.NoError(t, os.Symlink(realTarget, link))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// A full filesystem on the link target (the mount where `missing` is created) is
	// the bug: git creates `missing` there via safe_create_leading_directories before
	// reaching the dangling symlink, so the ENOSPC is on that filesystem. Without
	// retaining the creatable parent on the error path only the common dir is probed
	// and the ENOSPC never reaches the chain.
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == resolvedRealTarget {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/missing/../dangling/../dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem on the mount where git creates `missing` (the link target) must surface the "+
			"resource errno even when a later dangling symlink makes the final destination unresolvable — "+
			"the creatable parent the tail walk accumulated before the error is probed: %v", out)
	require.Contains(t, probedDirs, resolvedRealTarget,
		"the existing parent of the creatable `missing` component (the link target) must be probed even "+
			"when a later dangling symlink makes the final destination unresolvable")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathJoinsAncestorStatError pins the contract the
// #5421 review added: when the existing-ancestor stat search (the walk up from a nested worktree
// parent that does not yet exist) reaches an ancestor whose stat returns a non-ENOENT error (EIO,
// ETIMEDOUT, the bounded ErrProbeTimedOut, ...), the resolver must retain and join that error the
// way the destination and symlink-resolution probes join theirs. Before the fix the branch
// discarded sErr and stopped, so a writable parent and common dir left the opaque Git failure with
// no environmental evidence and the lost-restore classifier charged a full filesystem on the mount
// the ancestor stat reported on to the structural retry budget. The joined error lets the
// classifier read the environmental errno alongside the Git failure.
func TestAnnotateWorktreeAddError_RelativeWorktreePathJoinsAncestorStatError(t *testing.T) {
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

	// The repo exists but the nested worktree parent wt does not, so the
	// existing-ancestor search walks up from wt to the repo. The repo's stat
	// returns a non-ENOENT environmental error (EIO) the way a stalled mount or
	// a filesystem returning an environmental errno does; before the fix the
	// search discarded it and stopped, leaving the opaque Git failure with no
	// environmental evidence when the parent and common dir are writable.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the launch cwd the way the production code does so the expected
	// paths match across hosts (macOS /var/folders -> /private/var/folders).
	resolvedLaunchCwd, err := filepath.EvalSymlinks(launchCwd)
	require.NoError(t, err)
	resolvedRepo := filepath.Join(resolvedLaunchCwd, "repo")

	// The nested worktree parent does not exist (the walk ascends from wt to
	// the repo), and the repo returns a non-ENOENT stat error (EIO) instead of
	// the existence the walk is looking for. A writable parent and common dir
	// would otherwise leave the opaque Git failure with no environmental errno
	// to classify, so the ancestor stat error must reach the chain.
	statErr := &os.PathError{Op: "stat", Path: resolvedRepo, Err: syscall.EIO}
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		if p == resolvedRepo {
			return nil, statErr
		}
		return os.Stat(p)
	}
	// The parent (wt) and common dir are writable, so the ancestor stat is the
	// only environmental evidence the classifier can read — discarding it
	// (the pre-fix behavior) charged the EIO to the structural budget.
	writeSpaceProbe = func(string) error { return nil }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.EIO),
		"a non-ENOENT stat error on an existing ancestor the missing-parent walk "+
			"reaches must be joined into the chain the way the destination and "+
			"symlink-resolution probes join theirs, not discarded: %v", out)
	require.Contains(t, out.Error(), "stat probe ancestor",
		"the ancestor stat error must be named in the annotation so the lost-restore "+
			"classifier reads the environmental errno alongside the Git failure: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathPreservesCreatableParentsOnOuterTimeout pins the
// contract the #5421 review added: when the shared symlink-resolution deadline fires while the tail
// walk has already accumulated a creatable parent (git wrote a missing directory into the mount the
// parent sits on before a later metadata lookup consumed the remaining budget), the bounded
// non-answer the outer deadline returns must still carry those parents so annotateGitWriteError
// write-probes them. Before the fix the outer-deadline branch returned a hard-coded nil parent
// slice, so a full filesystem on the mount git wrote into was never probed and the lost-restore
// classifier charged it to the structural retry budget instead of waiting for that filesystem to
// recover.
//
// The deadline is a constant (symlinkResolveTimeout) the test cannot inject, so the tail walk blocks
// a tail component's lstat on a channel the test releases just after the deadline fires: the
// worker accumulates the creatable parent before the block, the outer timer fires while the lstat
// is in flight, the deadline's receive drains the worker's accumulated parents once the lstat
// returns, and the annotate path write-probes the mount git wrote the missing directory into.
func TestAnnotateWorktreeAddError_RelativeWorktreePathPreservesCreatableParentsOnOuterTimeout(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link points at a real dir on a distinct location; `missing` does
	// not exist, so the longest existing prefix is <repo>/link (resolved to the
	// link target) and the tail `missing/block` must create `missing` under the
	// link target (the mount) before reaching `block`. The tail walk accumulates
	// the link target as the creatable parent of `missing`, then the `block`
	// component's lstat blocks until the test releases it just after the shared
	// deadline fires — the outer deadline must drain the accumulated parent the
	// worker hands back rather than returning a hard-coded nil.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	// The `block` tail component's lstat blocks on this channel until the test
	// releases it just after the outer deadline fires, so the worker is still
	// in flight when the deadline trips the timer branch (the deadline is a
	// constant, so the release waits symlinkResolveTimeout plus a short grace).
	blockRelease := make(chan struct{})
	time.AfterFunc(symlinkResolveTimeout+200*time.Millisecond, func() { close(blockRelease) })
	blockPath := filepath.Join(resolvedRealTarget, "missing", "block")
	lstatSpaceProbe = func(p string) (os.FileInfo, error) {
		if p == blockPath {
			<-blockRelease
			return nil, os.ErrNotExist
		}
		return os.Lstat(p)
	}
	// Pre-creation: every stat returns ENOENT so the existing-destination check
	// stays silent and the walk treats the missing component as creatable.
	statSpaceProbe = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	// A full filesystem on the link target (the mount where `missing` is
	// created) is the bug: the outer deadline must drain the accumulated
	// creatable parent so the write probe surfaces ENOSPC there instead of
	// probing only the common dir.
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == resolvedRealTarget {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	// The base `extra` is split off by filepath.Split so the tail the walk runs
	// is `missing/block`; the tail accumulates the link target as the creatable
	// parent of `missing`, then the `block` lstat blocks until the test
	// releases it after the deadline.
	g := &GitWorktree{repoPath: "repo", worktreePath: "link/missing/block/extra"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, fsprobe.ErrProbeTimedOut),
		"the outer deadline must surface the typed non-answer the way a single "+
			"timed-out flight does: %v", out)
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem on the mounted symlink target where git creates the "+
			"missing component must surface the resource errno even when the shared "+
			"deadline fires while the tail walk is in flight — the outer deadline "+
			"drains the creatable parents the worker accumulated before the block: %v", out)
	require.Contains(t, probedDirs, resolvedRealTarget,
		"the existing parent of the creatable `missing` component (the link target) must be probed "+
			"even when the shared deadline fires while the tail walk is in flight")
}

// TestAnnotateGitWriteError_ConcurrentProbesPreserveOtherFilesystemsAfterTimeout pins the
// contract the #5421 review added: a timed-out write probe on one filesystem must not skip a
// later probe on a separate full filesystem. The sequential loop a shared deadline replaced
// could not advance past the first timed-out probe because a single writeSpaceProbe consumes
// its full probeTimeout (the same duration as symlinkResolveTimeout), so the deadline elapsed
// before the next parent on a different filesystem was reached and its ENOSPC was omitted —
// the lost-restore classifier then read only the bounded-deadline evidence and could exhaust
// its capped retries even though disk exhaustion was the actual failure. The concurrent loop
// runs each dir's probe in its own goroutine, so a stalled write on one filesystem does not
// consume the budget a later probe on a separate filesystem needs: every distinct filesystem
// gets its own probe within the same bound, and a full filesystem's ENOSPC reaches the chain.
//
// The stalled dir blocks for longer than the sequential loop's shared deadline would have
// waited (symlinkResolveTimeout), so the reverted form skips the second dir (the deadline
// checks before each probe); the concurrent form probes both in parallel and the second dir's
// ENOSPC reaches the chain.
func TestAnnotateGitWriteError_ConcurrentProbesPreserveOtherFilesystemsAfterTimeout(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	stalledDir := filepath.Join(t.TempDir(), "stalled")
	enospcDir := filepath.Join(t.TempDir(), "enospc")

	var probedDirs []string
	blockRelease := make(chan struct{})
	time.AfterFunc(symlinkResolveTimeout+200*time.Millisecond, func() { close(blockRelease) })
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == stalledDir {
			<-blockRelease
			return nil
		}
		if dir == enospcDir {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{}
	out := g.annotateGitWriteError(errors.New("git worktree add failed"), stalledDir, enospcDir)

	require.Contains(t, probedDirs, enospcDir,
		"a later dir on a separate filesystem must be probed even while the first dir's write "+
			"probe stalls, so its ENOSPC reaches the chain rather than being skipped by a sequential "+
			"deadline that a single probeTimeout consumes")
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"the second dir's ENOSPC must reach the chain: %v", out)
}

// TestAnnotateGitWriteError_BoundsConcurrentWriteProbeFanOut pins the contract the #5421
// review added for the write-probe fan-out: a hand-edited relative worktree path can
// accumulate many distinct creatable parents (repeated `dirN/missing/../../` segments, each
// retained by the tail walk), and annotateGitWriteError starts one goroutine and one 1 MiB
// write probe per parent. Without a bound all of them could target the same mount at once —
// hundreds of simultaneous probes could exhaust file descriptors or manufacture ENOSPC on a
// filesystem a structural git failure does not actually write to, stalling the lost-restore
// loop on a coincidental resource errno. The fan-out is capped by writeProbeConcurrency so a
// degraded same-mount path cannot fan out unbounded resource use while still letting probes
// on distinct filesystems run in parallel (a timed-out probe on one mount does not consume
// the budget a probe on a separate mount needs).
//
// Deterministic in both directions: against the form that started one goroutine per parent
// with no bound, the in-flight count exceeded writeProbeConcurrency and the test failed; with
// the bound the in-flight count never exceeds writeProbeConcurrency while every dir is still
// probed and a full filesystem's ENOSPC still reaches the chain.
func TestAnnotateGitWriteError_BoundsConcurrentWriteProbeFanOut(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// More dirs than writeProbeConcurrency so the bound is the limiter, not the
	// number of dirs. Each probe blocks until a shared gate opens so the in-flight
	// count is observable, and a full filesystem on the last dir must still
	// reach the chain (the bound does not drop coverage of distinct paths).
	dirs := make([]string, writeProbeConcurrency*4)
	base := t.TempDir()
	for i := range dirs {
		dirs[i] = filepath.Join(base, fmt.Sprintf("d%d", i))
	}

	var mu sync.Mutex
	var inFlight, maxInFlight int
	blockRelease := make(chan struct{})
	writeSpaceProbe = func(dir string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Hold each probe in flight long enough for the fan-out to reach the
		// bound. The gate opens after a short delay so the test does not stall
		// the whole poll; the bound is what limits concurrency, not the gate.
		<-blockRelease
		mu.Lock()
		inFlight--
		mu.Unlock()
		if dir == dirs[len(dirs)-1] {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	time.AfterFunc(50*time.Millisecond, func() { close(blockRelease) })
	g := &GitWorktree{}
	out := g.annotateGitWriteError(errors.New("git worktree add failed"), dirs...)

	require.LessOrEqual(t, maxInFlight, writeProbeConcurrency,
		"the concurrent write-probe fan-out must be bounded by writeProbeConcurrency so a "+
			"hand-edited path with many creatable parents cannot start hundreds of simultaneous "+
			"1 MiB probes on the same mount (which would exhaust file descriptors or manufacture "+
			"ENOSPC on a filesystem a structural git failure does not write to): saw %d in flight",
		maxInFlight)
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"the bound must not drop coverage of distinct paths: the full filesystem's ENOSPC must "+
			"still reach the chain: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFinalDotSymlinkedNonemptyDirSkipsDestinationProbe
// pins the contract the #5421 review added for a final `.` after a symlinked directory: `link/.`
// with `link` targeting an existing nonempty directory is rejected by `git worktree add` because
// the destination already exists, without writing through the symlink. The earlier final-`.` fix
// handled a symlink to a regular file (it surfaces ENOTDIR), but a symlink to a nonempty
// directory bypassed that non-directory check — `filepath.Join(resolvedParent, ".")` collapses
// the dot after resolving `link`, so the later Lstat saw the target as an ordinary directory
// and the destination-directory probe unconditionally write-probed it; a coincidental ENOSPC on
// the target's filesystem turned a structural "already exists" failure into an indefinite
// environmental retry on a filesystem freeing it cannot fix. Keeping the un-resolved parent as
// the probe target preserves the symlink as the final component the existing-destination
// check's Lstat sees, so the empty-dir gate skips a nonempty target the way a non-dot
// symlinked destination is skipped — git populates an empty target through the symlink but
// rejects a nonempty one without writing.
//
// Deterministic in both directions: against the form that collapsed the dot, the write probe
// targeted the symlink target and a write probe mocked to fail only there returned ENOSPC, so
// errors.Is(out, syscall.ENOSPC) was true and the test failed; with the un-resolved parent the
// Lstat gate sees the symlink, the empty-dir read skips the nonempty target, and the ENOSPC
// never reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFinalDotSymlinkedNonemptyDirSkipsDestinationProbe(t *testing.T) {
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

	// <repo>/link points at a real nonempty directory on a distinct filesystem; `git
	// worktree add link/.` follows `link` to the directory and rejects the add because the
	// destination already exists, without writing through the symlink. A coincidental
	// ENOSPC on the target's filesystem must not stall the lost-restore loop on a
	// structural failure freeing it cannot fix — the empty-dir gate skips a nonempty
	// symlinked destination the way a non-dot final symlink does.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "link"))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "occupied"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlink target the way the production code does so the expected path
	// matches across hosts (macOS /var/folders -> /private/var/folders).
	resolvedTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)

	// Stat follows the symlink and reports a directory (the existing target); Lstat
	// reports the symlink itself so the gate excludes it before the write probe targets
	// the symlink's target. The empty-dir read counts the target's entries and skips the
	// probe because the target is nonempty — git rejects the add without writing.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the symlink target's filesystem is the bug:
		// git rejects `link/.` without writing there, so freeing it cannot fix
		// the structural failure and the probe must not target it.
		if dir == resolvedTarget {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/."}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a `.` after a symlink to a nonempty directory must not write-probe the target — git "+
			"rejects the add because the destination already exists, so a full target filesystem "+
			"cannot fix the structural failure and must not stall the lost-restore loop: %v", out)
	require.NotContains(t, probedDirs, resolvedTarget,
		"the symlink target's filesystem must not be probed for a `.` after a symlink to a "+
			"nonempty directory — git never writes there")
}

// TestAnnotateGitWriteError_BoundsConcurrentWriteProbeFanOutPastDeadline pins the contract
// the #5421 review added for the post-deadline write-probe fan-out: when the first
// writeProbeConcurrency parents stall past what was previously a deadline, the remaining
// parents must still be bounded by writeProbeConcurrency so a hand-edited path with hundreds
// of creatable parents on the same responsive mount cannot start hundreds of simultaneous
// 1 MiB probes (manufacturing ENOSPC or exhausting file descriptors on a filesystem a
// structural git failure does not write to). A prior form launched the remaining probes
// concurrently without the semaphore once the deadline elapsed, preserving cross-filesystem
// coverage but bypassing the fan-out cap the steady state defends; the bound now applies at
// all times. The first batch holds the semaphore until its probes drain on their per-dir
// bound, after which the remaining probes acquire it in batches of writeProbeConcurrency, so
// the in-flight count never exceeds writeProbeConcurrency while every dir is still probed and
// a later parent's ENOSPC still reaches the chain.
//
// Deterministic in both directions: against the form that launched the remaining probes
// without the semaphore after the deadline, the in-flight count exceeded writeProbeConcurrency
// once the first batch stalled past the deadline; with the bound the in-flight count stays at
// or below writeProbeConcurrency while every dir is still probed and a full filesystem's
// ENOSPC still reaches the chain.
func TestAnnotateGitWriteError_BoundsConcurrentWriteProbeFanOutPastDeadline(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// The first writeProbeConcurrency parents stall past the deadline the
	// prior form switched on (each blocks for just past symlinkResolveTimeout),
	// and the remaining parents return ENOSPC immediately. A prior form
	// launched the remaining probes without the semaphore once the deadline
	// elapsed, so the in-flight count exceeded writeProbeConcurrency while the
	// first batch stalled; the bound now applies at all times, so the remaining
	// probes wait on the semaphore the first batch holds and the in-flight
	// count never exceeds writeProbeConcurrency.
	stalled := make([]string, writeProbeConcurrency)
	stalledBase := t.TempDir()
	for i := range stalled {
		stalled[i] = filepath.Join(stalledBase, fmt.Sprintf("stalled%d", i))
	}
	rest := make([]string, writeProbeConcurrency*3)
	restBase := t.TempDir()
	for i := range rest {
		rest[i] = filepath.Join(restBase, fmt.Sprintf("r%d", i))
	}
	dirs := append(stalled, rest...)

	var mu sync.Mutex
	var inFlight, maxInFlight int
	blockRelease := make(chan struct{})
	time.AfterFunc(symlinkResolveTimeout+200*time.Millisecond, func() { close(blockRelease) })
	writeSpaceProbe = func(dir string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		isStalled := false
		for _, s := range stalled {
			if dir == s {
				isStalled = true
				break
			}
		}
		if isStalled {
			<-blockRelease
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{}
	out := g.annotateGitWriteError(errors.New("git worktree add failed"), dirs...)

	// The in-flight count never exceeds writeProbeConcurrency: the first batch
	// holds the semaphore while it stalls, and the remaining probes wait on it
	// rather than fanning out unbounded once the first batch stalls past the
	// deadline. A prior form launched the remaining probes without the
	// semaphore once the deadline elapsed, so the in-flight count exceeded
	// writeProbeConcurrency while the first batch stalled; the bound now
	// applies at all times.
	require.LessOrEqual(t, maxInFlight, writeProbeConcurrency,
		"the post-deadline write-probe fan-out must stay within writeProbeConcurrency so a "+
			"hand-edited path with many creatable parents on the same responsive mount cannot "+
			"start hundreds of simultaneous 1 MiB probes once the first batch stalls past the "+
			"deadline: saw %d in flight", maxInFlight)
	// Every dir is still probed — the bound serializes the probes into
	// batches of writeProbeConcurrency but does not drop a later parent on a
	// separate filesystem the way a per-input-order cutoff would.
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a later parent's ENOSPC must still reach the chain once the stalled first batch "+
			"releases the semaphore: %v", out)
}

// TestAnnotateGitWriteError_AggregateDeadlineProbesLaterFilesystemAfterFirstBatchStalls pins
// the #5421-review contract that a later parent on a separate filesystem must still be probed
// when the first writeProbeConcurrency parents stall: a crafted relative path can place several
// creatable parents on a degraded mount before a later parent on a separate ENOSPC mount, and
// dropping the later parent's resource errno would leave the lost-restore classifier exhausting
// its bounded retries while the full filesystem remains the actual outage. The first batch
// holds the semaphore while its probes stall (each consumes its full per-dir bound on the
// degraded mount); the later parent waits on the semaphore the first batch holds, and once the
// stalled probes drain on their per-dir bound the later parent acquires the semaphore and is
// probed. The bound applies at all times so the post-deadline fan-out stays within
// writeProbeConcurrency, but the semaphore is released once the stalled probes drain so the
// later parent's ENOSPC still reaches the chain rather than being truncated by input order.
//
// Deterministic in both directions: against the form that dropped the later parent at the
// deadline, the later parent's ENOSPC never reached the chain; with the bounded form the
// later parent is probed once the stalled first batch releases the semaphore and the ENOSPC
// reaches the chain.
func TestAnnotateGitWriteError_AggregateDeadlineProbesLaterFilesystemAfterFirstBatchStalls(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// writeProbeConcurrency stalled parents fill the first batch and hold the
	// semaphore while their probes stall; a later parent on a separate
	// filesystem returns ENOSPC and must still be probed once the stalled
	// first batch releases the semaphore.
	stalled := make([]string, writeProbeConcurrency)
	stalledBase := t.TempDir()
	for i := range stalled {
		stalled[i] = filepath.Join(stalledBase, fmt.Sprintf("stalled%d", i))
	}
	enospcDir := filepath.Join(t.TempDir(), "enospc")
	dirs := append(stalled, enospcDir)

	blockRelease := make(chan struct{})
	time.AfterFunc(symlinkResolveTimeout+200*time.Millisecond, func() { close(blockRelease) })
	writeSpaceProbe = func(dir string) error {
		if dir == enospcDir {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		// Each stalled parent consumes its full per-dir bound so the first
		// batch holds the semaphore while it stalls.
		<-blockRelease
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{}
	out := g.annotateGitWriteError(errors.New("git worktree add failed"), dirs...)

	// The later parent's ENOSPC reaches the chain even though it sits after
	// the stalled first batch: the later parent waits on the semaphore the
	// first batch holds, and once the stalled probes drain the later parent
	// acquires it and is probed, so the resource errno the subprocess's
	// opaque ExitError drops still classifies the failure as environmental and
	// the lost-restore loop waits for the full filesystem rather than
	// exhausting its retries.
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a later parent on a separate ENOSPC mount must still be probed once the stalled first "+
			"batch releases the semaphore: %v", out)
}

// TestAnnotateWorktreeAddError_AncestorStatSearchDeadlineSurfacesTypedNonAnswer pins the
// contract the #5421 review added for the existing-ancestor stat search's shared deadline:
// when a deeply nested missing parent sits on a slow filesystem whose successive bounded
// stats each return ENOENT just before the per-call timeout, the search's shared
// symlinkResolveTimeout deadline can elapse before the walk reaches an existing ancestor.
// Before the fix the loop silently broke at the deadline without setting ancestorStatErr, so
// a writable parent and common dir left the opaque Git failure with no environmental errno
// and the lost-restore classifier charged a full filesystem on the mount the missing
// ancestor git creates the leading directories into to the structural retry budget. The fix
// joins the same typed non-answer the symlink walk joins on its shared deadline
// (fsprobe.ErrProbeTimedOut + context.DeadlineExceeded) so the classifier reads the bounded
// environmental non-answer the way it reads a stat-probe timeout, instead of bare structural
// silence.
//
// Deterministic in both directions: against the form that broke at the deadline without
// joining the deadline evidence, the writable parent and common dir probes succeeded and the
// chain carried no ErrProbeTimedOut, so errors.Is(out, fsprobe.ErrProbeTimedOut) was false
// and the test failed; with the fix the deadline break joins the typed non-answer and the
// chain carries it.
func TestAnnotateWorktreeAddError_AncestorStatSearchDeadlineSurfacesTypedNonAnswer(t *testing.T) {
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

	// The repo exists so the symlink resolution (which uses the real
	// fsprobe.EvalSymlinks, not the mocked stat) resolves the longest existing
	// prefix quickly; the nested worktree parent wt/dest does not exist, so the
	// existing-ancestor search walks up from the nonexistent wt to the repo. The
	// search's statSpaceProbe is mocked to return ENOENT after a short delay for
	// every path so the walk keeps ascending, and the shared ancestorDeadline
	// (symlinkResolveTimeout) elapses during the second stat — the loop breaks at
	// the deadline before reaching the existing ancestor, without setting the
	// non-ENOENT stat error the existing-ancestor branch joins. Before the fix
	// the break was silent; with the fix the deadline break joins the typed
	// non-answer so the classifier reads it.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Each stat returns ENOENT after a delay long enough that two stats exceed the
	// shared ancestorDeadline (symlinkResolveTimeout): the first stat returns at
	// just under the deadline so the loop continues, the second stat pushes the
	// elapsed time past the deadline, and the third iteration's deadline check
	// breaks the loop without reaching the existing ancestor. The delay is well
	// under fsprobe's per-call bound (the mocked stat does not go through the
	// bounded flight), so a real per-call timeout does not race the deadline.
	statDelay := symlinkResolveTimeout/2 + 100*time.Millisecond
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		time.Sleep(statDelay)
		return nil, os.ErrNotExist
	}
	// A writable parent and common dir so the only environmental evidence is the
	// ancestor stat search's typed non-answer; without the fix the chain would
	// carry no ErrProbeTimedOut and the structural budget would be charged.
	writeSpaceProbe = func(string) error { return nil }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.True(t, errors.Is(out, fsprobe.ErrProbeTimedOut),
		"the existing-ancestor stat search's shared deadline must surface the typed non-answer "+
			"(fsprobe.ErrProbeTimedOut) the way the symlink walk does, so a slow filesystem that "+
			"elapses the deadline before reaching an existing ancestor does not leave the opaque "+
			"git failure with bare structural silence: %v", out)
	require.True(t, errors.Is(out, context.DeadlineExceeded),
		"the ancestor stat search deadline must pair ErrProbeTimedOut with context.DeadlineExceeded "+
			"the way the bounded-probe non-answer class does, so the lost-restore classifier reads the "+
			"bounded environmental non-answer rather than a hung session operation: %v", out)
	require.Contains(t, out.Error(), "ancestor stat search",
		"the ancestor stat search deadline error must be named in the annotation so the lost-restore "+
			"classifier reads it alongside the git failure: %v", out)
}

// TestAnnotateGitWriteError_AggregateDeadlineBoundsSemaphoreBatchWait pins the #5421-review
// contract the aggregate write-probe wait does not scale with the number of creatable parents:
// the semaphore caps the fan-out at writeProbeConcurrency, but without a shared deadline a
// hand-edited path with many distinct creatable parents on a degraded mount (where each
// writeSpaceProbe consumes its full per-dir bound) would serialize ceil(N/8) batches of
// probeTimeout each, stalling the synchronous restore poll for minutes. A prior form
// (3d8ddb3c9e) added a shared deadline but broke at it, dropping a later parent on a
// separate ENOSPC mount whose ENOSPC the lost-restore classifier must read; a later form
// (d872d3acf1) preserved coverage by launching the remaining probes without the semaphore
// once the deadline elapsed, but that re-introduced the unbounded fan-out the semaphore caps
// in the steady state (manufacturing ENOSPC or exhausting file descriptors on a filesystem a
// structural git failure does not write to). The current form reinstates a shared aggregate
// deadline at 2 × symlinkResolveTimeout that stops launching new probes once it elapses,
// keeping the semaphore cap (the remaining probes are not launched without it) so the fan-out
// stays within writeProbeConcurrency at all times, and the in-flight probes drain on their
// own per-dir bound so the aggregate stays within ~2 × symlinkResolveTimeout + a single
// probeTimeout rather than ceil(N/writeProbeConcurrency) × probeTimeout. A later parent on a
// separate filesystem whose probe acquires the semaphore before the deadline still reaches the
// chain the way the post-deadline coverage test pins; the common-dir probe below still covers
// the independently known location.
//
// Deterministic in both directions: against the form that serialized every batch without a
// deadline, 4 × writeProbeConcurrency parents that each block for symlinkResolveTimeout stalled
// the loop for ~4 × symlinkResolveTimeout (4 batches of 8, each waiting its full bound); with
// the aggregate deadline the loop stops launching at 2 × symlinkResolveTimeout, the first two
// batches drain on their per-dir bound, and the elapsed stays under 3 × symlinkResolveTimeout
// rather than scaling to ceil(N/writeProbeConcurrency) × symlinkResolveTimeout. The in-flight
// count never exceeds writeProbeConcurrency (the semaphore cap is retained, the remaining
// probes are not launched without it once the deadline elapses).
func TestAnnotateGitWriteError_AggregateDeadlineBoundsSemaphoreBatchWait(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// writeProbeConcurrency parents each block for symlinkResolveTimeout (the per-dir
	// bound a degraded mount consumes), so without the aggregate deadline the loop
	// would serialize ceil(N/writeProbeConcurrency) batches of symlinkResolveTimeout.
	// Use 4 × writeProbeConcurrency so the unbounded aggregate would be
	// 4 × symlinkResolveTimeout, well past the 2 × symlinkResolveTimeout deadline.
	dirs := make([]string, writeProbeConcurrency*4)
	base := t.TempDir()
	for i := range dirs {
		dirs[i] = filepath.Join(base, fmt.Sprintf("d%d", i))
	}

	var mu sync.Mutex
	var inFlight, maxInFlight, probeCount int
	writeSpaceProbe = func(dir string) error {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		// Simulate a degraded mount where each probe consumes slightly more
		// than the per-dir bound (symlinkResolveTimeout + 200ms, the same
		// margin the AggregateDeadlineProbesLaterFilesystemAfterFirstBatchStalls
		// stall uses) so the second batch is still in flight when the 2 x
		// symlinkResolveTimeout aggregate deadline elapses; the deadline fires
		// before the second batch releases the semaphore, so the third batch
		// does not start. The unbounded aggregate would be
		// ceil(len(dirs)/writeProbeConcurrency) x (symlinkResolveTimeout+200ms);
		// the deadline stops launching new probes at 2 x symlinkResolveTimeout
		// and the in-flight second batch drains on its per-dir bound.
		time.Sleep(symlinkResolveTimeout + 200*time.Millisecond)
		mu.Lock()
		inFlight--
		probeCount++
		mu.Unlock()
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	start := time.Now()
	g := &GitWorktree{}
	g.annotateGitWriteError(errors.New("git worktree add failed"), dirs...)
	elapsed := time.Since(start)

	// The aggregate deadline stops launching new probes at 2 x
	// symlinkResolveTimeout, so the elapsed stays under 3 x symlinkResolveTimeout
	// (the deadline plus the in-flight second batch's per-dir drain) rather than
	// the ceil(len(dirs)/writeProbeConcurrency) x (symlinkResolveTimeout+200ms)
	// the unbounded semaphore serialization would take for 4 x
	// writeProbeConcurrency parents (~8.8s). The first two batches (16 probes)
	// drain before the deadline stops the third, so the elapsed is bounded at
	// ~2 x symlinkResolveTimeout + one per-dir drain.
	require.Less(t, elapsed, 3*symlinkResolveTimeout,
		"the aggregate write-probe wait must be bounded by the deadline + a single "+
			"per-dir drain, not ceil(len(dirs)/writeProbeConcurrency) x probeTimeout: "+
			"saw %v for %d dirs", elapsed, len(dirs))
	// The semaphore cap is retained: the in-flight count never exceeds
	// writeProbeConcurrency, so a hand-edited path with many creatable parents
	// cannot start hundreds of simultaneous 1 MiB probes once the deadline elapses.
	require.LessOrEqual(t, maxInFlight, writeProbeConcurrency,
		"the write-probe fan-out must stay within writeProbeConcurrency even past the "+
			"aggregate deadline so a degraded mount does not fan out unbounded resource "+
			"use: saw %d in flight", maxInFlight)
	// The first two batches (writeProbeConcurrency each) are probed before the
	// 2 x symlinkResolveTimeout deadline elapses; the remaining batches are not
	// launched once the deadline fires. The common-dir probe is a no-op here
	// (gitCommonDir returns ""), so only the per-dir probes count.
	require.LessOrEqual(t, probeCount, 2*writeProbeConcurrency,
		"the aggregate deadline must stop launching new probes once it elapses, so only "+
			"the first two batches (writeProbeConcurrency each) are probed before the deadline: "+
			"saw %d probes", probeCount)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathMultipleTrailingDotsSymlinkedNonemptyDirSkipsDestinationProbe
// pins the #5421-review contract for more than one trailing dot component after a symlinked
// directory: `link/./.` with `link` targeting an existing nonempty directory is rejected by
// `git worktree add` because the destination already exists, without writing through the
// symlink. The earlier final-dot fix handled a single `.` (it preserved the un-resolved parent
// so the lstat gate saw the symlink), but filepath.Split returns only the last component as
// `base`, so `link/./.` left `parent` ending in `.` after the separator trim and
// os.Lstat("link/.") followed the symlink to its target the way a trailing separator would;
// the lstat gate saw the target directory rather than the symlink, the empty-dir gate did not
// skip it, and the destination-directory probe write-probed the target's filesystem. A
// coincidental ENOSPC on the target's filesystem would stall the lost-restore loop on a
// structural "already exists" failure the way a single trailing dot or a trailing separator
// would. The fix strips every trailing `.<sep>` from the un-resolved parent so the lstat gate
// sees the symlink the way a single-dot destination does.
//
// Deterministic in both directions: against the form that preserved only the last dot, the
// write probe targeted the symlink target and a write probe mocked to fail only there returned
// ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with the strip the
// lstat gate sees the symlink, the empty-dir read skips the nonempty target, and the ENOSPC
// never reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathMultipleTrailingDotsSymlinkedNonemptyDirSkipsDestinationProbe(t *testing.T) {
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

	// <repo>/link points at a real nonempty directory on a distinct filesystem; `git
	// worktree add link/./.` follows `link` to the directory and rejects the add
	// because the destination already exists, without writing through the symlink.
	// A coincidental ENOSPC on the target's filesystem must not stall the lost-restore
	// loop on a structural failure freeing it cannot fix — the empty-dir gate skips a
	// nonempty symlinked destination the way a single trailing dot or a non-dot final
	// symlink does.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "link"))
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "occupied"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlink target the way the production code does so the expected path
	// matches across hosts (macOS /var/folders -> /private/var/folders).
	resolvedTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)

	// Stat follows the symlink and reports a directory (the existing target); Lstat
	// reports the symlink itself so the gate excludes it before the write probe targets
	// the symlink's target. The empty-dir read counts the target's entries and skips the
	// probe because the target is nonempty — git rejects the add without writing.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the symlink target's filesystem is the bug:
		// git rejects `link/./.` without writing there, so freeing it cannot fix
		// the structural failure and the probe must not target it.
		if dir == resolvedTarget {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/./."}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"multiple trailing dots after a symlink to a nonempty directory must not "+
			"write-probe the target — git rejects the add because the destination already "+
			"exists, so a full target filesystem cannot fix the structural failure and must "+
			"not stall the lost-restore loop: %v", out)
	require.NotContains(t, probedDirs, resolvedTarget,
		"the symlink target's filesystem must not be probed for multiple trailing dots "+
			"after a symlink to a nonempty directory — git never writes there")
}
