package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testresidue"
)

// #4170's sandbox-HOME half, regressed for the af-test-user-home-* family: a
// killed test binary leaves an af-test-user-home-<n> under the temp dir next
// to its af-test-home-<n> AGENT_FACTORY_HOME, and (before the fix)
// internal/testresidue.Classify did not know the prefix, so af doctor's
// test-residue check never even saw it — one permanent leak per killed package
// run, silently exempt from the count. These tests pin the new SandboxUserHome
// kind's distinct content rule and liveness gate, which are the only new code
// paths beyond what the existing SandboxHome/tmux residue suite already covers
// (age gate, fix re-read, symlinked temp dir, sibling removal all run through
// the same shared assessTestResidueDir / testResidueRemoveFix code).

// sandboxUserHomeResidue is a leaked testguard sandbox USER home: the directory
// holding .zshrc and the testresidue.SandboxUserHomeMarker the harness always
// seeds it with (the exact bytes userhome.sandboxUserHome writes), plus any
// extra leaves named by files (use ".gitconfig" for the optional include, or
// any other name to stage content the harness does not leave).
func sandboxUserHomeResidue(t *testing.T, root, name string, files ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".zshrc"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, testresidue.SandboxUserHomeMarker),
		[]byte("testguard.SandboxHome (#4469)\n"), 0o600))
	paths := []string{dir, filepath.Join(dir, ".zshrc"), filepath.Join(dir, testresidue.SandboxUserHomeMarker)}
	for _, f := range files {
		p := filepath.Join(dir, f)
		require.NoError(t, os.WriteFile(p, []byte("seed\n"), 0o600))
		paths = append(paths, p)
	}
	ageTree(t, paths...)
	return dir
}

// TestALeakedSandboxUserHomeIsReportedThenRemoved: a leaked sandbox HOME is
// visible to af doctor (no longer the #4170 invisibility regression) and
// cleared by --fix, in both shapes the harness leaves (with and without the
// optional .gitconfig the content rule must accept).
func TestALeakedSandboxUserHomeIsReportedThenRemoved(t *testing.T) {
	for name, extra := range map[string][]string{
		"without gitconfig": nil,
		"with gitconfig":    {".gitconfig"},
	} {
		t.Run(name, func(t *testing.T) {
			root := socketTempHome(t)
			stubOpenFiles(t, true, nil)
			dir := sandboxUserHomeResidue(t, root, testresidue.SandboxUserHomePrefix+"1111", extra...)
			witness, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"1112", "dead")

			report, err := Run(residueOptions(t, root, false))
			require.NoError(t, err)
			f := residueFinding(t, report, dir)
			require.True(t, f.Actionable, f.Detail)
			require.NotEmpty(t, f.FixAction)
			require.DirExists(t, dir, "a report-only run removes nothing")
			require.True(t, residueFinding(t, report, witness).Actionable)

			report, err = Run(residueOptions(t, root, true))
			require.NoError(t, err)
			f = residueFinding(t, report, dir)
			require.True(t, f.Fixed, "fix error: %v", f.FixErr)
			require.NoDirExists(t, dir)
			require.NoDirExists(t, witness)
		})
	}
}

// Content is what licenses the removal, so anything beyond what the harness
// leaves is reported and kept, never auto-removed — the same guard a
// SandboxHome with a stranger's file gets, exercised against the new
// sandbox-HOME content rule.
func TestASandboxUserHomeHoldingMoreThanTheHarnessLeavesIsKept(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, true, nil)
	dir := sandboxUserHomeResidue(t, root, testresidue.SandboxUserHomePrefix+"4400", "notes.txt")

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)
	f := residueFinding(t, report, dir)
	require.False(t, f.Actionable, f.Detail)
	require.Empty(t, f.FixAction)
	require.Contains(t, f.Detail, "notes.txt, which the test harness does not leave behind")
	require.DirExists(t, dir)
	require.FileExists(t, filepath.Join(dir, "notes.txt"))
}

// A sandbox HOME the harness did not finish seeding (killed between MkdirTemp
// and the synchronous writes of .zshrc / the marker) is missing a leaf the
// harness always writes, so it reads as not-removable: reported, not removed.
func TestASandboxUserHomeMissingARequiredLeafIsKept(t *testing.T) {
	for name, drop := range map[string]string{
		"missing marker": testresidue.SandboxUserHomeMarker,
		"missing .zshrc": ".zshrc",
	} {
		t.Run(name, func(t *testing.T) {
			root := socketTempHome(t)
			stubOpenFiles(t, true, nil)
			dir := sandboxUserHomeResidue(t, root, testresidue.SandboxUserHomePrefix+"5500")
			require.NoError(t, os.Remove(filepath.Join(dir, drop)))
			ageTree(t, dir)

			report, err := Run(residueOptions(t, root, true))
			require.NoError(t, err)
			f := residueFinding(t, report, dir)
			require.False(t, f.Actionable, f.Detail)
			require.Empty(t, f.FixAction)
			require.Contains(t, f.Detail, "which the harness always writes into a sandbox HOME")
			require.DirExists(t, dir)
		})
	}
}

// Where open files cannot be seen, a sandbox HOME is reported not removed — a
// pane the test launched into it may keep a file open, the same hazard a
// SandboxHome's log poses — while a tmux socket dir, which has nothing to lose,
// is still cleared. This pins the holdsOpenFile gate's extension to the new
// kind; without it a sandboxed HOME would be removed even when the process
// table is unreadable.
func TestWithoutAnOpenFileViewSandboxUserHomesAreReportedNotRemoved(t *testing.T) {
	root := socketTempHome(t)
	stubOpenFiles(t, false, nil)
	home := sandboxUserHomeResidue(t, root, testresidue.SandboxUserHomePrefix+"7700")
	tmuxDir, _ := tmuxResidue(t, root, testresidue.PackageTmuxPrefix+"7701", "dead")

	report, err := Run(residueOptions(t, root, true))
	require.NoError(t, err)

	f := residueFinding(t, report, home)
	require.False(t, f.Actionable)
	require.Empty(t, f.FixAction)
	require.Contains(t, f.Detail, "cannot see which files processes hold open")
	require.Contains(t, f.Detail, "sandboxed HOME")
	require.DirExists(t, home)

	require.True(t, residueFinding(t, report, tmuxDir).Fixed)
	require.NoDirExists(t, tmuxDir)
}
