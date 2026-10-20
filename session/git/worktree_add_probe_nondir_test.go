package git

import (
	"errors"
	"os"
	"path/filepath"
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
		probedDirs = append(probedDirs, dir)
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
