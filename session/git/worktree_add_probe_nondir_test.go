package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

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
