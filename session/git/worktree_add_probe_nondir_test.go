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
