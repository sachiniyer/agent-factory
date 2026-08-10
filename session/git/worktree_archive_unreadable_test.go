package git

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

func TestRelocateWorktreeTo_ArchivePublicationSnapshotIsAtomic(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits; run this regression under a non-root uid")
	}
	previousFastMove := worktreeMoveFast
	worktreeMoveFast = func(*GitWorktree, string, string) error { return errors.New("force manual relocation") }
	t.Cleanup(func() { worktreeMoveFast = previousFastMove })
	previousRename := renamePath
	renamePath = func(_, _ string) error { return syscall.EXDEV }
	t.Cleanup(func() { renamePath = previousRename })

	published := make(chan struct{})
	releasePublish := make(chan struct{})
	previousAfterCommit := moveDirAfterDestCommit
	moveDirAfterDestCommit = func(string) error {
		close(published)
		<-releasePublish
		return nil
	}
	t.Cleanup(func() { moveDirAfterDestCommit = previousAfterCommit })

	gw, _, source := archiveTestWorktree(t)
	locked := filepath.Join(source, "private", "credential")
	require.NoError(t, os.MkdirAll(filepath.Dir(locked), 0o755))
	require.NoError(t, os.WriteFile(locked, []byte("not readable by af"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))
	destination := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "atomic-report")

	archiveDone := make(chan error, 1)
	go func() { archiveDone <- gw.relocateWorktreeTo(destination, "archive", nil) }()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("archive never reached destination publication")
	}

	type snapshot struct {
		path     string
		recovery bool
		report   ArchiveReport
	}
	snapshotDone := make(chan snapshot, 1)
	go func() {
		path, _, recovery, report := gw.PersistenceSnapshot()
		snapshotDone <- snapshot{path: path, recovery: recovery, report: report}
	}()
	var crossedBoundary *snapshot
	select {
	case observed := <-snapshotDone:
		crossedBoundary = &observed
	case <-time.After(100 * time.Millisecond):
		// Publication owns the persistence lock until path, recovery, and report
		// all describe the committed destination.
	}
	close(releasePublish)
	require.NoError(t, <-archiveDone)
	if crossedBoundary != nil {
		t.Fatalf("checkpoint crossed the publication boundary and observed path %q, recovery=%v, report=%+v", crossedBoundary.path, crossedBoundary.recovery, crossedBoundary.report)
	}

	select {
	case observed := <-snapshotDone:
		require.Equal(t, destination, observed.path)
		require.False(t, observed.report.Empty(), "published incomplete destination lost its retained-tree report")
	case <-time.After(5 * time.Second):
		t.Fatal("persistence snapshot stayed blocked after publication completed")
	}
}
