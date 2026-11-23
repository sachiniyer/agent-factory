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

// simulateCrashWindowQuarantine reproduces the on-disk footprint the daemon
// leaves when it dies between the quarantine rename and the publish rename of
// a cross-device archive move:
//   - the recorded source worktree has been renamed wholesale to a
//     `.af-source-<32hex>` sibling (the complete worktree, with all uncommitted
//     changes, is intact there);
//   - a complete staged copy sits at a `.af-copy-<32hex>` sibling of the
//     (still absent) destination;
//   - the destination itself is absent;
//   - no relocation recovery record exists (the record is materialized only
//     after the publish rename).
//
// The instance keeps pointing its recorded worktree path at the now-absent
// source, exactly as a daemon restart would reload it. when withStaging is
// false only the quarantine is left, exercising the source-parent scan alone.
func simulateCrashWindowQuarantine(t *testing.T, srcPath, dest string, withStaging bool) (quarantine, staging string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	quarantine = filepath.Join(filepath.Dir(srcPath), ".af-source-"+strings.Repeat("a", 32))
	require.NoError(t, os.Rename(srcPath, quarantine))
	if withStaging {
		staging = filepath.Join(filepath.Dir(dest), ".af-copy-"+strings.Repeat("b", 32))
		out, err := exec.Command("cp", "-a", quarantine, staging).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	return quarantine, staging
}

// TestArchiveSession_RefusesCrashWindowQuarantineResidue is the regression for
// the cross-device archive crash window. Before the fix, a bare ENOENT from
// the record-free claim routed archiveSessionSourceAbsent onto the "deleted
// outside af" branch and committed the row Archived with the worktree-missing
// stamp — silently stranding the complete worktree (with its uncommitted
// changes) under an unguessable `.af-source-<32hex>` sibling no record, log, or
// sweep referenced. After the fix, the af residue is detected and the archive
// refuses, names the residue, leaves the session live and unfenced, and keeps
// the stranded work intact for the user to move back and retry.
func TestArchiveSession_RefusesCrashWindowQuarantineResidue(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "crashed")
	dest, err := archivedWorktreePath(repoID, "crashed")
	require.NoError(t, err)
	quarantine, staging := simulateCrashWindowQuarantine(t, wtPath, dest, true)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "crashed", RepoID: repoID})
	require.Error(t, err)
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"a record-free crash-window refusal must be shaped as an unresolved relocation so callers can tell it from a committed deletion")
	assert.Contains(t, err.Error(), ".af-source-", "the refusal must name the stranded residue so the user can find it")
	if staging != "" {
		assert.Contains(t, err.Error(), staging, "the refusal must name the staged copy beside the destination")
	}

	// The session was NOT archived: the fence is down, the row is not Archived,
	// and nothing committed a worktree-missing stamp.
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing the crash window must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released so the session is not left fenced forever")
	missing, _ := inst.WorktreeMissing()
	assert.False(t, missing,
		"the crash-window refusal must not stamp the row worktree-missing the way a deleted-outside-af archive would")
	rec := recordFor(t, repoID, "crashed")
	require.NotNil(t, rec)
	assert.NotEqual(t, session.Archived, rec.Status,
		"the durable row must not be committed Archived over the stranded residue")

	// The quarantined worktree survives untouched with its uncommitted file —
	// the exact loss the residue arbitration exists to prevent.
	dirty, rerr := os.ReadFile(filepath.Join(quarantine, "dirty.txt"))
	require.NoError(t, rerr, "the stranded worktree must survive the refused archive")
	assert.Equal(t, "uncommitted", string(dirty))

	// The diagnosis the user acts on is also in the daemon's own warning log —
	// not silent the way the pre-fix "deleted outside af; branch kept" path was.
	assert.Contains(t, logs.warnings.String(), "crashed")
	assert.Contains(t, logs.warnings.String(), ".af-source-")
}

// TestArchiveSession_RefusesCrashWindowWithSourceResidueOnly exercises the
// source-parent scan in isolation: only the `.af-source-` quarantine is
// present beside the vacated recorded path (e.g. a stale quarantine from an
// earlier interrupted move beside a session whose worktree was then deleted
// outside af). The destination side has no residue. The archive must still
// refuse rather than classify the absent source as a plain deletion, so the
// user is pointed at the stranded tree.
func TestArchiveSession_RefusesCrashWindowWithSourceResidueOnly(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "source-residue")
	dest, err := archivedWorktreePath(repoID, "source-residue")
	require.NoError(t, err)
	quarantine, staging := simulateCrashWindowQuarantine(t, wtPath, dest, false)
	require.Empty(t, staging)

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "source-residue", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), quarantine, "the refusal must point at the source-parent residue")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	assert.FileExists(t, filepath.Join(quarantine, "dirty.txt"),
		"the stranded worktree must survive the refused archive")
}

// TestArchiveSession_CrashWindowResidueIsRetryableAfterRecovery proves the
// refusal is not a dead end: the user moves the stranded quarantine back to
// the recorded path (the documented recovery), retries the archive, and it
// completes normally — the worktree relocates to the destination with its
// uncommitted changes intact, no data lost.
func TestArchiveSession_CrashWindowResidueIsRetryableAfterRecovery(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "recover")
	dest, err := archivedWorktreePath(repoID, "recover")
	require.NoError(t, err)
	quarantine, _ := simulateCrashWindowQuarantine(t, wtPath, dest, true)

	// First attempt refuses, exactly as above.
	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "recover", RepoID: repoID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".af-source-")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness())

	// The user moves the stranded worktree back to the recorded path and retries.
	require.NoError(t, os.Rename(quarantine, wtPath))
	require.NoError(t, os.RemoveAll(filepath.Dir(dest))) // any residue staging is cleared on recovery

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "recover", RepoID: repoID})
	require.NoError(t, err, "after moving the quarantine back, the archive must complete normally")
	assert.Equal(t, dest, archivedPath)
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.False(t, data.Worktree.Missing, "a completed archive is not a missing-worktree archive")
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the uncommitted work must reach the archive via the recovered worktree")
	assert.Equal(t, "uncommitted", string(dirty))

	// No af-private residue is left beside the original worktree parent: the
	// completed archive cleaned up the source rather than orphaning it.
	parent := filepath.Dir(wtPath)
	entries, lerr := os.ReadDir(parent)
	require.NoError(t, lerr)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), ".af-source-") || strings.HasPrefix(e.Name(), ".af-copy-"),
			"no af relocation residue should remain beside the original worktree parent after a completed archive: %s", e.Name())
	}
}

// TestArchiveSession_GenuineDeletionHasNoResidueStillArchivesAsDeleted is
// the negative control: a worktree deleted outside af with NO af residue
// present must still take the deletion route and commit Archived with the
// worktree-missing stamp. The residue arbitration must not over-refuse and
// strand a genuinely-deleted row forever.
func TestArchiveSession_GenuineDeletionHasNoResidueStillArchivesAsDeleted(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "genuinely-gone")
	require.NoError(t, os.RemoveAll(wtPath))
	branch := inst.GetBranch()
	if branch == "" {
		branch = "af/genuinely-gone"
	}

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "genuinely-gone", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, wtPath, archivedPath, "nothing moved, so the recorded path is the one reported")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.True(t, data.Worktree.Missing, "a genuine deletion still commits the worktree-missing stamp")
	assert.Equal(t, wtPath, data.Worktree.WorktreePath)
	assert.False(t, inst.Started())
	assert.Equal(t, session.OpNone, inst.GetInFlightOp())
	requireBranchExists(t, repoPath, branch)
	rec := recordFor(t, repoID, "genuinely-gone")
	require.NotNil(t, rec)
	assert.Equal(t, session.Archived, rec.Status)
	assert.True(t, rec.Worktree.Missing)
}

// TestArchiveSession_ForeignResidueDoesNotBlockGenuineDeletion is the
// shared-parent guard: the source parent and the archive root are shared by
// every session, so residue blocks this session's archive ONLY when it is
// proven to be this session's worktree. A residue belonging to ANOTHER session
// (another session's interrupted move, or a tree another session's archive
// retained on purpose) beside a session whose own worktree was genuinely
// deleted outside af must NOT block its archive, and the row must commit
// Archived as deleted rather than refuse forever.
func TestArchiveSession_ForeignResidueDoesNotBlockGenuineDeletion(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "foreign-residue")
	// A second, independent worktree in the SAME repo (another session),
	// stranded under a residue name beside this session's source parent —
	// exactly the footprint another session's interrupted cross-device move
	// leaves in the shared parent.
	otherBranch := "af/other-session"
	otherWt := filepath.Join(filepath.Dir(repoPath), "wt-other-session")
	out, err := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", otherBranch, otherWt).CombinedOutput()
	require.NoError(t, err, string(out))
	foreignResidue := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("f", 32))
	require.NoError(t, os.Rename(otherWt, foreignResidue))

	// This session's own worktree is genuinely deleted outside af.
	require.NoError(t, os.RemoveAll(wtPath))
	branch := inst.GetBranch()
	if branch == "" {
		branch = "af/foreign-residue"
	}

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "foreign-residue", RepoID: repoID})
	require.NoError(t, err, "a foreign session's residue must not block this session's genuine-deletion archive")
	assert.Equal(t, wtPath, archivedPath, "nothing of this session moved, so its recorded path is the one reported")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.True(t, data.Worktree.Missing, "a genuine deletion still commits the worktree-missing stamp")
	requireBranchExists(t, repoPath, branch)
	rec := recordFor(t, repoID, "foreign-residue")
	require.NotNil(t, rec)
	assert.Equal(t, session.Archived, rec.Status)
	assert.True(t, rec.Worktree.Missing)

	// The foreign residue is left untouched — it belongs to another session,
	// and this archive must never have told the operator to move it into this
	// session's path.
	assert.DirExists(t, foreignResidue, "the foreign session's residue must survive this archive untouched")
}

// TestArchiveSession_UnverifiedResidueRefusesWithoutClaimingOwnership pins
// the fail-closed shape for a residue whose git binding cannot be read (e.g.
// a stale orphan a completed move left, or a directory a user created with
// af's private naming). The archive refuses (it cannot prove a deletion over a
// scan it could not classify), but the refusal must NOT claim the residue is
// this session's worktree and must NOT tell the operator to move it back into
// this session's path.
func TestArchiveSession_UnverifiedResidueRefusesWithoutClaimingOwnership(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "unverified-residue")
	dest, err := archivedWorktreePath(repoID, "unverified-residue")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	// Strand this session's worktree, then drop an orphan residue with NO
	// linked-worktree `.git` pointer beside the source parent — a residue
	// whose git binding cannot be read.
	require.NoError(t, os.RemoveAll(wtPath))
	orphan := filepath.Join(filepath.Dir(wtPath), ".af-source-0123456789abcdef0123456789abcdee")
	require.NoError(t, os.Mkdir(orphan, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(orphan, "stale.txt"), []byte("orphan"), 0o644))

	_, _, err = manager.ArchiveSession(ArchiveSessionRequest{Title: "unverified-residue", RepoID: repoID})
	require.Error(t, err, "an unverified residue must refuse fail-closed rather than guess a deletion")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Contains(t, err.Error(), orphan, "the refusal must name the unverified residue so the user can inspect it")
	assert.NotContains(t, err.Error(), "move it back to",
		"the refusal for an unverified residue must never instruct the operator to move it into this session's path")
	assert.NotContains(t, err.Error(), "move it back to "+wtPath,
		"the refusal must not name this session's recorded path as a move-back target for unverified residue")
	assert.Contains(t, err.Error(), "could not verify",
		"the refusal must say the binding could not be verified, not claim the residue is this session's work")
	assert.NotEqual(t, session.LiveArchived, inst.GetLiveness(),
		"refusing over unverified residue must not commit the row Archived")
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"the archive fence must be released so the session is not left fenced forever")
}

// TestArchiveSession_AdoptsLandedArchiveMoveUnaffectedByResidueFix guards the
// #5102 adoption route: a move that landed at dest (whether by `git worktree
// move` or a published cross-device rename) must still be adopted in place,
// regardless of whether af's quarantine was cleaned up. The residue fix
// targets the DEST-ABSENT branch only — when dest is present the adoption path
// owns the decision, so an orphaned quarantine beside the source must not
// block adoption of the bytes that did reach dest.
func TestArchiveSession_AdoptsLandedArchiveMoveUnaffectedByResidueFix(t *testing.T) {
	manager, _, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst, wtPath := registerArchivable(t, manager, repoID, repoPath, "landed-residue")
	dest, err := archivedWorktreePath(repoID, "landed-residue")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))

	// Move the worktree to dest with `git worktree move` (atomic on the same
	// filesystem), modelling a landed-but-unrecorded earlier archive. The
	// recorded path is now vacated with no recovery record — a bare ENOENT.
	out, err := exec.Command("git", "-C", repoPath, "worktree", "move", wtPath, dest).CombinedOutput()
	require.NoError(t, err, string(out))

	// Drop a stale `.af-source-<32hex>` beside the vacated source, modelling
	// an orphaned quarantine a completed-but-unrecorded cross-device archive
	// left behind. The residue fix must not let this block adoption of the
	// bytes git proves at dest.
	staleQuarantine := filepath.Join(filepath.Dir(wtPath), ".af-source-"+strings.Repeat("c", 32))
	require.NoError(t, os.Mkdir(staleQuarantine, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staleQuarantine, "stale.txt"), []byte("orphan"), 0o644))

	archivedPath, data, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "landed-residue", RepoID: repoID})
	require.NoError(t, err)
	assert.Equal(t, dest, archivedPath)
	assert.Equal(t, dest, data.Worktree.WorktreePath)
	assert.False(t, data.Worktree.Missing, "an adopted move is a complete archive, not a missing worktree")
	assert.Equal(t, session.LiveArchived, data.Liveness)
	assert.False(t, inst.Started())
	dirty, rerr := os.ReadFile(filepath.Join(dest, "dirty.txt"))
	require.NoError(t, rerr, "the landed bytes must be kept where they are")
	assert.Equal(t, "uncommitted", string(dirty))
	rec := recordFor(t, repoID, "landed-residue")
	require.NotNil(t, rec)
	assert.Equal(t, dest, rec.Worktree.WorktreePath)
}
