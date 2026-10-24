package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkedRepoFinalDot pins the #5421
// review contract for a restored worktreePath that ends in a trailing `.` and traverses a
// symlinked repository with `..`: `../dest/.` relative to a symlinked `/launch/repo -> /mnt/repo`
// must resolve the symlinked repo before collapsing `..` the way `git -C /launch/repo worktree
// add ../dest/.` does (it lands at /mnt/dest, not /launch/dest). The final-dot branch keeps the
// un-resolved parent as the probe target to preserve a final symlink for the existing-destination
// lstat gate, but the un-resolved `launchCwd/repo/../dest` parent's lexical filepath.Dir is
// `launchCwd` (the symlink's parent), and a coincidental ENOSPC there would convert a structural
// "already exists" rejection into an indefinite environmental retry. The intermediate traversal
// must be resolved before preserving the final destination component so the parent probe targets
// the real parent (`/mnt`) the way the non-dot `../wt` case does.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkedRepoFinalDot(t *testing.T) {
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

	// A real repo dir on a distinct filesystem location, with a symlink inside
	// the launch cwd pointing at it. The probe must resolve through the link
	// before collapsing `..`, the way `git -C /launch/repo worktree add ../dest/.`
	// does.
	realRepo := t.TempDir()
	require.NotEqual(t, realRepo, launchCwd)
	linkRepo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Symlink(realRepo, linkRepo))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// The destination already exists as a nonempty directory: git rejects the
	// add because the destination already exists, so the final-dot branch reaches
	// the existing-destination check (a missing dest returns ENOTDIR before the
	// parent probe and never exercises the lexical-parent hazard).
	resolvedRepo, err := filepath.EvalSymlinks(linkRepo)
	require.NoError(t, err)
	resolvedDest := filepath.Join(filepath.Dir(resolvedRepo), "dest")
	require.NoError(t, os.Mkdir(resolvedDest, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(resolvedDest, "occupied"), []byte("x"), 0o644))

	// Stat follows the resolved path and reports the existing destination as a
	// directory; Lstat reports a regular (non-symlink) entry so the lstat gate
	// probes the destination in its own right the way a non-dot destination does.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }

	// The lexical parent the un-resolved `launchCwd/repo/../dest` collapses to
	// (launchCwd) — a different filesystem the probe must not target. Only it is
	// full, so a probe of the real parent (the symlink's target's parent) sees
	// no ENOSPC and the git failure stays structural; a probe of launchCwd (the
	// bug) adds ENOSPC and the lost-restore loop would wait indefinitely on a
	// filesystem freeing it cannot fix.
	lexicalParent := launchCwd
	require.NotEqual(t, lexicalParent, filepath.Dir(resolvedDest),
		"the symlinked-repo final-dot case must diverge from the lexical parent, otherwise the test cannot distinguish the fix from the bug")
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == lexicalParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "../dest/."}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem at the lexical symlink parent (%s) must not add ENOSPC — git rejected the add "+
			"on the real filesystem, and the parent probe must target the symlink-resolved parent (%s), "+
			"not the un-resolved parent the lexical filepath.Dir collapses to: %v",
		lexicalParent, filepath.Dir(resolvedDest), out)
	require.Contains(t, probedDirs, filepath.Dir(resolvedDest),
		"the worktree parent dir probed must be the symlink-resolved parent (%s), not the lexical %s",
		filepath.Dir(resolvedDest), lexicalParent)
	require.NotContains(t, probedDirs, lexicalParent,
		"a coincidental ENOSPC on the symlink's parent filesystem (%s) must not be probed — "+
			"git rejects the add on the real filesystem, not the symlink's parent", lexicalParent)
	for _, d := range probedDirs {
		require.True(t, filepath.IsAbs(d),
			"a probed dir must be absolute (resolved through the symlink), not a raw relative path: %q", d)
	}
}
