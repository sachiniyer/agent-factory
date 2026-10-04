package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedPrunableArchive registers a session and archives it for real — the same
// fixture the archive tests use — then lets the stamped archive time age past
// the smallest admissible cutoff ("1ms"), which a 5ms sleep buys without
// reaching for a private setter.
func seedPrunableArchive(t *testing.T, title string) (*Manager, string, string, *session.Instance, string) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	archivedPath, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(archivedPath, "dirty.txt"))
	// Let the archive timestamp age past the tiniest admissible cutoff.
	time.Sleep(5 * time.Millisecond)
	return manager, repoID, repoPath, inst, archivedPath
}

func pruneReq(repoID string) PruneSessionsRequest {
	return PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms"}
}

// TestPruneSessions_DryRunChangesNothing: the default pass lists the candidate
// and its reclaimable bytes but deletes nothing and stamps nothing — the
// dry-run contract #5136 asks for before --apply exists.
func TestPruneSessions_DryRunChangesNothing(t *testing.T) {
	manager, repoID, _, inst, archivedPath := seedPrunableArchive(t, "dry-run-row")

	resp, err := manager.PruneSessions(pruneReq(repoID))
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.False(t, resp.Applied)
	require.Len(t, resp.Pruned, 1)
	entry := resp.Pruned[0]
	assert.Equal(t, "dry-run-row", entry.Title)
	assert.Equal(t, "af/dry-run-row", entry.Branch)
	assert.Greater(t, entry.ReclaimedBytes, int64(0))
	assert.True(t, entry.PrunedAt.IsZero(), "a dry run stamps no prune time")

	assert.True(t, exists(archivedPath), "dry run must not delete the archived worktree")
	assert.True(t, inst.PrunedAt().IsZero(), "dry run must not mark the record")
	persisted := persistedInstanceByTitle(t, repoID, "dry-run-row")
	assert.True(t, persisted.PrunedAt.IsZero(), "the stored row must not be tombstoned by a dry run")
}

// TestPruneSessions_ApplyDeletesAndTombstones is the heart of #5136: the
// archived worktree is deleted, the branch stays, and the record becomes a
// listed-but-unrestorable tombstone carrying title/branch/archived_at/pruned_at.
func TestPruneSessions_ApplyDeletesAndTombstones(t *testing.T) {
	manager, repoID, repoPath, inst, archivedPath := seedPrunableArchive(t, "pruned-row")

	resp, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.True(t, resp.Applied)
	require.Len(t, resp.Pruned, 1)
	assert.False(t, resp.Pruned[0].PrunedAt.IsZero())
	require.Empty(t, resp.Incomplete)

	assert.False(t, exists(archivedPath), "the archived worktree directory must be deleted")
	assert.False(t, inst.PrunedAt().IsZero(), "the live record is tombstoned")
	assert.True(t, inst.IsPruned())

	// The branch is the tombstone's promise — verify it survives.
	out, err := exec.Command("git", "-C", repoPath, "branch", "--list", "af/pruned-row").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "af/pruned-row", "prune must never delete the session's branch")

	// The persisted tombstone: still a row, still archived liveness, marked.
	stored := persistedInstanceByTitle(t, repoID, "pruned-row")
	assert.Equal(t, session.LiveArchived, stored.Liveness)
	assert.False(t, stored.PrunedAt.IsZero(), "stored row carries pruned_at")
	assert.False(t, stored.ArchivedAt.IsZero(), "stored row carries archived_at")
	assert.Equal(t, "af/pruned-row", pruneBranchFor(stored))
	assert.Equal(t, session.LifecycleActionNone, stored.LifecycleAction,
		"a tombstone must not advertise restore to any client")

	// Re-running the same apply is a clean skip, not a second deletion.
	again, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)
	require.Empty(t, again.Pruned)
	require.Len(t, again.Skipped, 1)
	assert.Contains(t, again.Skipped[0].Reason, "already pruned")
}

// TestPruneSessions_RefusesRestoreNamingBranch: the tombstone's only way back
// is the branch, so the restore refusal must name it (#5136 spec).
func TestPruneSessions_RefusesRestoreNamingBranch(t *testing.T) {
	manager, repoID, _, _, _ := seedPrunableArchive(t, "branchy-row")

	_, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)

	_, _, restoreErr := manager.RestoreSession(RestoreSessionRequest{Title: "branchy-row", RepoID: repoID})
	require.Error(t, restoreErr)
	assert.Contains(t, restoreErr.Error(), "af/branchy-row",
		"the refusal must name the kept branch so the user can recreate the work")
	assert.Contains(t, restoreErr.Error(), "pruned")
}

// TestPruneSessions_SkipsIneligibleRows: live rows, in-flight operations, and
// too-recent archives are reported with reasons — never silently filtered and
// never deleted.
func TestPruneSessions_SkipsIneligibleRows(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)

	// A live row in the same repo.
	registerStarted(t, manager, repoID, repoPath, "live-row", session.NewFakeBackend(), true, session.Running)

	// An archived row old enough to take the cutoff.
	oldInst, _ := registerArchivable(t, manager, repoID, repoPath, "old-row")
	oldInst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "old-row", RepoID: repoID})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	resp, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)

	var titles []string
	for _, e := range resp.Pruned {
		titles = append(titles, e.Title)
	}
	assert.ElementsMatch(t, []string{"old-row"}, titles)

	skips := make(map[string]string)
	for _, s := range resp.Skipped {
		skips[s.Title] = s.Reason
	}
	require.Contains(t, skips, "live-row")
	assert.Contains(t, skips["live-row"], "not archived")

	// A row archived moments ago under a tight cutoff reports "too recent".
	young, _ := registerArchivable(t, manager, repoID, repoPath, "young-row")
	young.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})
	archivedYoung, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "young-row", RepoID: repoID})
	require.NoError(t, err)

	resp2, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "24h", Apply: true})
	require.NoError(t, err)
	require.Empty(t, resp2.Pruned)
	skips2 := make(map[string]string)
	for _, s := range resp2.Skipped {
		skips2[s.Title] = s.Reason
	}
	require.Contains(t, skips2, "young-row")
	assert.Contains(t, skips2["young-row"], "archived too recently")
	assert.True(t, exists(archivedYoung), "a too-recent archive must be left alone")
}

// TestPruneSessions_ConcurrentArchiveIsSkipped: a session mid-archive (its
// in-flight op legitimately raised by BeginArchive) is exactly the race the
// op gate exists for — prune must report it, not delete underneath it.
func TestPruneSessions_ConcurrentArchiveIsSkipped(t *testing.T) {
	manager, repoID, _, inst, archivedPath := seedPrunableArchive(t, "racy-row")
	// A lifecycle claim raised ON the archived row — kill keeps the liveness
	// while owning the op slot — is the honest spelling of "concurrent op".
	require.NoError(t, inst.Transition(session.BeginKill()))
	defer inst.Transition(session.RevertKill()) //nolint:errcheck

	resp, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)
	require.Empty(t, resp.Pruned)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "operation in flight")
	assert.True(t, exists(archivedPath), "nothing may be deleted under a held lifecycle op")
	assert.True(t, inst.PrunedAt().IsZero())
}

// TestPruneSessions_KillsInFlightClaimIsSkipped: the daemon's exclusive
// lifecycle claim (kill/restore/delete-project) is honored alongside the
// op flag — a row another operation owns reports busy, never disappears.
func TestPruneSessions_KillsInFlightClaimIsSkipped(t *testing.T) {
	manager, repoID, _, inst, archivedPath := seedPrunableArchive(t, "claimed-row")
	key := daemonInstanceKey(repoID, "claimed-row")
	manager.mu.Lock()
	manager.killsInFlight[key] = struct{}{}
	manager.mu.Unlock()

	resp, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)
	require.Empty(t, resp.Pruned)
	require.Len(t, resp.Skipped, 1)
	assert.Contains(t, resp.Skipped[0].Reason, "in progress")
	assert.True(t, exists(archivedPath))
	assert.True(t, inst.PrunedAt().IsZero())
}

// TestPruneSessions_PreservesProviderFiles: provider transcript files belong
// to the agent, not af — a pruned session's conversation may be live
// elsewhere via a carry, a handoff, or a manual resume (#5136 review).
// Apply must leave every byte of the provider home untouched.
func TestPruneSessions_PreservesProviderFiles(t *testing.T) {
	manager, repoID, _, inst, _ := seedPrunableArchive(t, "captured-row")

	claudeHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", t.TempDir())

	const convID = "deadbeef-1111-2222-3333-444444444444"
	inst.SetAgentConversation(session.AgentConversationData{
		Agent:       tmux.ProgramClaude,
		ID:          convID,
		CaptureKind: session.ConversationCaptureInjected,
	})
	projectDir := filepath.Join(claudeHome, "projects", "-some-encoded-path")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	transcript := filepath.Join(projectDir, convID+".jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte("captures"), 0o644))
	before, err := os.ReadFile(transcript)
	require.NoError(t, err)

	resp, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)
	require.Len(t, resp.Pruned, 1)

	after, err := os.ReadFile(transcript)
	require.NoError(t, err, "provider transcript files belong to the agent — prune must not delete them")
	assert.Equal(t, before, after, "the provider file must be byte-identical after prune")
}

// TestPruneSessions_RequiresScopeAndDuration: the validation that keeps the
// command honest — no retention guess, no unscoped apply.
func TestPruneSessions_RequiresScopeAndDuration(t *testing.T) {
	manager, _, _, _, _ := seedPrunableArchive(t, "guard-row")

	_, err := manager.PruneSessions(PruneSessionsRequest{RepoID: "x", Apply: true})
	require.ErrorContains(t, err, "older_than is required")

	_, err = manager.PruneSessions(PruneSessionsRequest{RepoID: "x", OlderThan: "not-a-duration", Apply: true})
	require.ErrorContains(t, err, "not a positive Go duration")

	_, err = manager.PruneSessions(PruneSessionsRequest{RepoID: "x", OlderThan: "-1h", Apply: true})
	require.ErrorContains(t, err, "not a positive Go duration")

	_, err = manager.PruneSessions(PruneSessionsRequest{OlderThan: "1ms", Apply: true})
	require.ErrorContains(t, err, "a scope is required")
}

// TestPruneSessions_TombstoneSurvivesSnapshot: a pruned row still reads back
// through the daemon's in-memory projection — the `af sessions list --all`
// visibility the spec requires.
func TestPruneSessions_TombstoneSurvivesSnapshot(t *testing.T) {
	manager, repoID, _, _, _ := seedPrunableArchive(t, "listed-row")
	_, err := manager.PruneSessions(PruneSessionsRequest{RepoID: repoID, OlderThan: "1ms", Apply: true})
	require.NoError(t, err)

	snap := manager.Snapshot(repoID)
	require.Len(t, snap, 1)
	row := snap[0]
	assert.Equal(t, "listed-row", row.Title)
	assert.False(t, row.PrunedAt.IsZero(), "the listed tombstone carries pruned_at")
	assert.False(t, row.ArchivedAt.IsZero())
	assert.Equal(t, session.LiveArchived, row.Liveness)
	assert.Equal(t, session.LifecycleActionNone, row.LifecycleAction)
	assert.True(t, strings.Contains(row.Branch, "listed-row") || strings.Contains(row.Worktree.BranchName, "listed-row"),
		"the tombstone keeps its branch")
}
