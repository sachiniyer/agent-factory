package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// landDestWithRetainedQuarantine reproduces the on-disk footprint the daemon
// leaves when it dies after a cross-device archive published and repaired dest
// but before it persisted the new path and ArchiveReport (#7816):
//
//   - the destination holds the session's worktree, registered and repaired so
//     git proves it is this session's (modelled by `git worktree move`, which
//     lands the bytes and re-aims the registration backpointer at dest/.git,
//     exactly as the cross-device move + `git worktree repair` sequence does);
//   - the recorded source path is absent — a bare ENOENT with no relocation
//     recovery record, because the record was materialized only in memory and
//     never written;
//   - the `.af-source-<32hex>` quarantine — the COMPLETE original tree the
//     archive retained because the published copy skipped unreadable files —
//     survives beside the vacated source, bound to the same registration leaf
//     that now backpoints at dest.
//
// The quarantine is a verbatim copy of the landed tree (its `.git` pointer
// resolves into the same registration leaf), plus one extra file modelling the
// content the incomplete published copy skipped: it is the only complete tree.
func landDestWithRetainedQuarantine(t *testing.T, repoPath, srcPath, dest string) (quarantine string) {
	t.Helper()
	moveWorktreeForTest(t, repoPath, srcPath, dest)
	quarantine = filepath.Join(filepath.Dir(srcPath), ".af-source-"+strings.Repeat("a", 32))
	out, err := exec.Command("cp", "-a", dest, quarantine).CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(
		filepath.Join(quarantine, "skipped-unreadable.txt"), []byte("only in the complete original"), 0o644))
	return quarantine
}

// TestArchiveSession_DestPresentRefusesSurvivingSourceQuarantine is the #7816
// regression. Before the fix, the dest-present branch of
// archiveSessionSourceAbsent adopted the landed destination and committed an
// EMPTY archive report while this session's `.af-source-*` quarantine — the
// only complete tree, retained because the published copy skipped unreadable
// files — survived beside the source untracked. A later restore from dest then
// silently lacked the skipped files. After the fix the adoption refuses,
// names the quarantine as the complete original, and leaves every byte intact
// for manual recovery.
func TestArchiveSession_DestPresentRefusesSurvivingSourceQuarantine(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-present")
	dest, err := archivedWorktreePath(repoID, "dest-present")
	require.NoError(t, err)
	quarantine := landDestWithRetainedQuarantine(t, repoPath, wtPath, dest)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-present", RepoID: repoID})
	require.Error(t, err,
		"adopting a landed destination must refuse while this session's source quarantine survives")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"the refusal must be shaped as an unresolved relocation so callers can tell it from a committed archive")
	assert.Contains(t, err.Error(), quarantine,
		"the refusal must name the surviving quarantine")
	assert.Contains(t, err.Error(), "complete original",
		"the refusal must name the .af-source-* tree as the complete original, not an anonymous residue")

	// The session was NOT archived: the fence is down and the durable row was
	// not committed over the retained tree.
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing the dest-present crash window must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released so the session is not left fenced forever")
	rec := recordFor(t, repoID, "dest-present")
	require.NotNil(t, rec)
	assert.NotEqual(t, session.Archived, rec.Status,
		"the durable row must not be committed Archived over the stranded quarantine")

	// Nothing was adopted or dropped: the complete original survives beside
	// the source, and the landed copy is untouched at dest.
	skipped, rerr := os.ReadFile(filepath.Join(quarantine, "skipped-unreadable.txt"))
	require.NoError(t, rerr, "the only complete tree must survive the refused archive")
	assert.Equal(t, "only in the complete original", string(skipped))
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed destination must be left in place for the retry")
	assert.Equal(t, "uncommitted", string(dirty))

	assert.Contains(t, logs.warnings.String(), "dest-present")
	assert.Contains(t, logs.warnings.String(), ".af-source-",
		"the daemon warning must name the stranded quarantine")
}

// TestArchiveSession_DestPresentQuarantineRefusalIsRetryable proves the
// refusal is a manual-recovery gate, not a dead end: once the operator moves
// the retained quarantine aside to a safe place, the retry adopts the landed
// destination exactly as a complete landed move does, while the salvaged
// complete original stays in the operator's custody.
func TestArchiveSession_DestPresentQuarantineRefusalIsRetryable(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-retry")
	dest, err := archivedWorktreePath(repoID, "dest-retry")
	require.NoError(t, err)
	quarantine := landDestWithRetainedQuarantine(t, repoPath, wtPath, dest)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-retry", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".af-source-")

	// The operator moves the complete original aside (out of the shared
	// parent, under their own name — the documented manual recovery).
	salvage := filepath.Join(t.TempDir(), "salvaged-original")
	require.NoError(t, os.Rename(quarantine, salvage))

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-retry", RepoID: repoID})
	require.NoError(t, err, "with the quarantine moved aside, the landed destination is adopted in place")
	assert.Equal(t, dest, archivedPath)
	assert.Equal(t, dest, data.Worktree.WorktreePath)
	assert.False(t, data.Worktree.Missing, "an adopted move is a complete archive, not a missing worktree")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.False(t, inst.Started())
	assert.True(t, data.ArchiveReport == nil || data.ArchiveReport.Empty(),
		"an adoption after the operator salvaged the quarantine retains nothing")

	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed bytes must be kept where they are")
	assert.Equal(t, "uncommitted", string(dirty))
	skipped, rerr := os.ReadFile(filepath.Join(salvage, "skipped-unreadable.txt"))
	require.NoError(t, rerr, "the salvaged complete original must remain in the operator's custody")
	assert.Equal(t, "only in the complete original", string(skipped))
}

// TestArchiveSession_DestPresentUnverifiedQuarantineRefuses pins the
// fail-closed arm: a `.af-source-*`-shaped residue beside the source whose git
// binding cannot be read (a user-created lookalike, a stale orphan, or a tree
// whose registration cannot be answered) is NOT proven absence — the
// destination must not be adopted over it. The refusal names the residue but
// does not claim it is this session's retained original.
func TestArchiveSession_DestPresentUnverifiedQuarantineRefuses(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-unverified")
	dest, err := archivedWorktreePath(repoID, "dest-unverified")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	// A `.af-source-*`-shaped directory with NO linked-worktree `.git`
	// pointer: its binding cannot be read, so it cannot be proven foreign —
	// and it cannot be claimed either.
	unverified := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("e", 32))
	require.NoError(t, os.Mkdir(unverified, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unverified, "stale.txt"), []byte("orphan"), 0o644))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-unverified", RepoID: repoID})
	require.Error(t, err,
		"an unverifiable source-quarantine-shaped residue must refuse rather than be adopted over")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), unverified,
		"the refusal must name the unverified residue so the operator can inspect it")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal must say the binding could not be read, not claim the residue is this session's original")

	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing over unverified residue must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released")
	assert.DirExists(t, unverified,
		"the unverified residue must be left untouched for the operator")
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed destination must be left in place")
	assert.Equal(t, "uncommitted", string(dirty))
}

// TestArchiveSession_DestPresentAdoptsWithStagingCopyResidue pins the other
// side of the `.af-source-*` filter: a `.af-copy-*` staging copy is never the
// retained original — it is what dest's own publication consumed — so residue
// of that shape beside the source does not gate the adoption, even when its
// binding cannot be read.
func TestArchiveSession_DestPresentAdoptsWithStagingCopyResidue(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	_, wtPath := registerArchivable(t, manager, repoID, repoPath, "dest-staging")
	dest, err := archivedWorktreePath(repoID, "dest-staging")
	require.NoError(t, err)
	moveWorktreeForTest(t, repoPath, wtPath, dest)

	// Only `.af-copy-*` residue beside the source: possibly a stray staged
	// copy, never this source's retained quarantine.
	staging := filepath.Join(filepath.Dir(wtPath), ".af-copy-"+strings.Repeat("d", 32))
	require.NoError(t, os.Mkdir(staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "stale.txt"), []byte("staged"), 0o644))

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "dest-staging", RepoID: repoID})
	require.NoError(t, err,
		"a .af-copy-* staging copy is never the retained source quarantine and must not block adoption")
	assert.Equal(t, dest, archivedPath)
	assert.Equal(t, dest, data.Worktree.WorktreePath)
	assert.Equal(t, session.LiveArchived, data.Liveness)
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed bytes must be kept where they are")
	assert.Equal(t, "uncommitted", string(dirty))
	assert.DirExists(t, staging, "the stray staging copy must be left untouched")
}
