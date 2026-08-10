package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestRelocateWorktreeTo_ArchiveAccountsForUnreadableSource drives the complete
// archive relocation path, including both source and destination validation.
// Attempt 1 tested only the collector and therefore missed that omitting this
// entry made validateSource reject every real archive with "tree entry set
// changed" (#3066).
func TestRelocateWorktreeTo_ArchiveAccountsForUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits; run this regression under a non-root uid")
	}
	previousFastMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error {
		return errors.New("force the manual relocation path")
	}
	t.Cleanup(func() { worktreeMoveFast = previousFastMove })

	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })

	gw, _, source := archiveTestWorktree(t)
	const relativeLocked = "private/locked\ncredential"
	locked := filepath.Join(source, relativeLocked)
	require.NoError(t, os.MkdirAll(filepath.Dir(locked), 0o755))
	require.NoError(t, os.WriteFile(locked, []byte("not readable by af"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))

	destination := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")
	err := gw.relocateWorktreeTo(destination, "archive", nil)
	require.NoError(t, err,
		"archive may omit a permission-denied file only when the manifest accounts for it")

	assertLiveWorktreeAt(t, gw, destination)
	assert.NoFileExists(t, filepath.Join(destination, relativeLocked),
		"a file af cannot read must not be fabricated in the archive")
	assert.NoDirExists(t, source, "the registered worktree must move to the archive location")

	report := gw.GetArchiveReport()
	require.Len(t, report.RetainedTrees, 1)
	retained := report.RetainedTrees[0]
	require.Equal(t, []ArchiveSkippedEntry{{
		Path: relativeLocked, Reason: ArchiveSkipPermissionDenied,
	}}, retained.Skipped, "the report must preserve a newline-bearing path as one structured entry")
	require.NotEmpty(t, retained.Path)
	assert.Contains(t, report.Warning("archive"), `"private/locked\ncredential"`,
		"text output must quote the newline rather than injecting a second path")

	contents, readErr := os.ReadFile(filepath.Join(retained.filesystemPath(), relativeLocked))
	require.ErrorIs(t, readErr, os.ErrPermission,
		"the retained original must keep its permission boundary rather than being replaced")
	assert.Nil(t, contents)
}

func TestRelocateWorktreeTo_ArchiveRepairFailureRetainsUnreadableReport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits; run this regression under a non-root uid")
	}
	previousFastMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error { return errors.New("force manual relocation") }
	t.Cleanup(func() { worktreeMoveFast = previousFastMove })
	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })
	repairErr := errors.New("registration repair failed")
	previousRepair := worktreeRepair
	worktreeRepair = func(*GitWorktree, string) error { return repairErr }
	t.Cleanup(func() { worktreeRepair = previousRepair })

	gw, _, source := archiveTestWorktree(t)
	const relativeLocked = "private/locked\ncredential"
	locked := filepath.Join(source, relativeLocked)
	require.NoError(t, os.MkdirAll(filepath.Dir(locked), 0o755))
	require.NoError(t, os.WriteFile(locked, []byte("not readable by af"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))

	destination := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "repair-fails")
	err := gw.relocateWorktreeTo(destination, "archive", nil)
	require.ErrorIs(t, err, repairErr)
	report := gw.GetArchiveReport()
	require.Len(t, report.RetainedTrees, 1,
		"the report must exist before registration repair so the daemon can persist and surface it")
	require.Equal(t, relativeLocked, report.RetainedTrees[0].Skipped[0].Path)
	require.Contains(t, report.Warning("archive"), `"private/locked\ncredential"`)
}
