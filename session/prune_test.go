package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pruneEligibleData is the one record shape PruneSkipReason must admit: an
// archived local-tmux session with an af-owned, fully-moved archived worktree
// and an archive time old enough for the cutoff. Each test mutates exactly one
// field off this baseline and expects the matching refusal.
func pruneEligibleData(t *testing.T) InstanceData {
	t.Helper()
	worktreePath := filepath.Join(t.TempDir(), "archived-wt")
	require.NoError(t, os.MkdirAll(worktreePath, 0o755))
	return InstanceData{
		Title:    "old-session",
		Program:  tmux.ProgramClaude,
		Status:   Archived,
		Liveness: LiveArchived,
		Branch:   "siyer/old-session",
		Worktree: GitWorktreeData{
			RepoPath:     "/repo",
			WorktreePath: worktreePath,
			BranchName:   "siyer/old-session",
		},
		ArchivedAt: time.Now().Add(-90 * 24 * time.Hour),
		CreatedAt:  time.Now().Add(-120 * 24 * time.Hour),
		UpdatedAt:  time.Now().Add(-90 * 24 * time.Hour),
	}
}

func TestPruneSkipReason_EligibleBaseline(t *testing.T) {
	data := pruneEligibleData(t)
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	require.Empty(t, PruneSkipReason(data, cutoff), "the archived baseline must be a prune candidate")
}

func TestPruneSkipReason_Table(t *testing.T) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-24 * time.Hour)
	cases := []struct {
		name   string
		mutate func(*InstanceData)
		want   string
	}{
		{"already pruned", func(d *InstanceData) { d.PrunedAt = recent }, "already pruned"},
		{"kill tombstone", func(d *InstanceData) { d.UserKilled = true }, "kill tombstone"},
		{"live running", func(d *InstanceData) {
			d.Liveness = LiveRunning
			d.Status = Running
		}, "not archived"},
		{"lost", func(d *InstanceData) {
			d.Liveness = LiveLost
			d.Status = Dead
		}, "not archived"},
		{"op in flight", func(d *InstanceData) { d.InFlightOp = OpArchiving }, "operation in flight"},
		{"startup unknown", func(d *InstanceData) { d.StartupStateUnknown = true }, "startup state unknown"},
		{"external worktree", func(d *InstanceData) { d.Worktree.ExternalWorktree = true }, "user-owned"},
		{"relocation recovery", func(d *InstanceData) {
			d.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{}
		}, "did not provably finish"},
		{"incomplete archive", func(d *InstanceData) {
			d.ArchiveReport = &git.ArchiveReport{RetainedTrees: []git.ArchiveRetainedTree{{Path: "/retained"}}}
		}, "incomplete archive"},
		{"no worktree path", func(d *InstanceData) { d.Worktree.WorktreePath = "" }, "no archived worktree path"},
		{"too recent", func(d *InstanceData) { d.ArchivedAt = recent }, "archived too recently"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := pruneEligibleData(t)
			tc.mutate(&data)
			reason := PruneSkipReason(data, cutoff)
			require.NotEmpty(t, reason, "case %q must not be eligible", tc.name)
			assert.Contains(t, reason, tc.want)
		})
	}
}

// TestPruneSkipReason_LegacyRowFallsBackToUpdatedAt pins the conservative
// legacy fallback: a row archived before archived_at existed is measured by
// its UpdatedAt, so post-archive mutations can only make it look YOUNGER
// (under-delete), never older.
func TestPruneSkipReason_LegacyRowFallsBackToUpdatedAt(t *testing.T) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	data := pruneEligibleData(t)
	data.ArchivedAt = time.Time{}
	data.UpdatedAt = time.Now().Add(-24 * time.Hour) // mutated after a long-ago archive
	assert.Contains(t, PruneSkipReason(data, cutoff), "archived too recently",
		"a legacy row whose UpdatedAt is fresh must not prune on an invisible old archive")

	data.UpdatedAt = time.Now().Add(-90 * 24 * time.Hour)
	assert.Empty(t, PruneSkipReason(data, cutoff),
		"a legacy row whose UpdatedAt is old must be eligible on the fallback")
}

// TestPruneSkipReason_UnprovableArchiveTime pins the conservative edge the
// CreatedAt fallback cannot cover: a row that predates BOTH archived_at and
// updated_at has its archive age synthesized from creation time — which is
// not archive time at all, so the row must never become eligible on it.
func TestPruneSkipReason_UnprovableArchiveTime(t *testing.T) {
	cutoff := time.Now().Add(-30 * 24 * time.Hour)

	data := pruneEligibleData(t)
	data.ArchivedAt = time.Time{}
	data.UpdatedAt = data.CreatedAt // FromInstanceData's synthesized legacy shape
	reason := PruneSkipReason(data, cutoff)
	require.NotEmpty(t, reason, "a created==updated legacy row has no provable archive time")
	assert.Contains(t, reason, "archive time cannot be proven")

	data.UpdatedAt = time.Time{}
	reason = PruneSkipReason(data, cutoff)
	assert.Contains(t, reason, "archive time cannot be proven",
		"a row with no updated_at at all cannot prove its archive age either")
}

// TestArchiveCommitStampsArchivedAt proves the single tkCommitArchive edge —
// which every archive route converges on — records the durable timestamp the
// cutoff measures from, and that a re-archive replaces it rather than keeping
// the first.
func TestArchiveCommitStampsArchivedAt(t *testing.T) {
	inst := &Instance{liveness: LiveRunning, started: true}
	require.True(t, inst.ArchivedAt().IsZero())

	require.NoError(t, inst.Transition(BeginArchive()))
	before := time.Now()
	require.NoError(t, inst.Transition(CommitArchive()))
	stamped := inst.ArchivedAt()
	require.False(t, stamped.IsZero())
	assert.False(t, stamped.Before(before))

	// A restore + re-archive cycle refreshes the shelf time.
	require.NoError(t, inst.Transition(BeginRestore()))
	require.NoError(t, inst.Transition(ConfirmLive()))
	require.NoError(t, inst.Transition(BeginArchive()))
	require.NoError(t, inst.Transition(CommitArchive()))
	assert.False(t, inst.ArchivedAt().Before(stamped))
}

// TestPruneTimestampsRoundTrip locks the serialized contract: archived_at and
// pruned_at survive InstanceData → JSON → FromInstanceData → ToInstanceData,
// and a pruned tombstone suppresses LifecycleAction so no client advertises
// restore on files that are gone.
func TestPruneTimestampsRoundTrip(t *testing.T) {
	archived := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Millisecond)
	pruned := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	data := pruneEligibleData(t)
	data.ArchivedAt = archived
	payload, err := json.Marshal(data)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(payload, &wire))
	require.Contains(t, wire, "archived_at", "archived_at must be on the durable wire")
	require.NotContains(t, wire, "pruned_at", "an unpruned row must not emit the tombstone field")

	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, archived, inst.ArchivedAt())
	require.True(t, inst.PrunedAt().IsZero())
	require.False(t, inst.IsPruned())

	inst.MarkPruned(pruned)
	require.True(t, inst.IsPruned())
	out := inst.ToInstanceData()
	require.Equal(t, pruned, out.PrunedAt)
	assert.Equal(t, LifecycleActionNone, out.LifecycleAction,
		"a pruned tombstone must not advertise a lifecycle action")

	storedPayload, err := json.Marshal(out.ForStorage())
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(storedPayload, &decoded))
	require.True(t, decoded.PrunedAt.Equal(pruned),
		"pruned_at must round-trip the same instant (JSON normalizes the location)")

	reloaded, err := FromInstanceData(decoded)
	require.NoError(t, err)
	require.True(t, reloaded.IsPruned(), "the tombstone must survive a daemon reload")
}

func TestArchiveTimeForFallbackOrder(t *testing.T) {
	archived := time.Now().Add(-90 * 24 * time.Hour)
	updated := time.Now().Add(-10 * 24 * time.Hour)
	created := time.Now().Add(-120 * 24 * time.Hour)

	assert.Equal(t, archived, ArchiveTimeFor(InstanceData{ArchivedAt: archived, UpdatedAt: updated, CreatedAt: created}))
	assert.Equal(t, updated, ArchiveTimeFor(InstanceData{UpdatedAt: updated, CreatedAt: created}))
	assert.Equal(t, created, ArchiveTimeFor(InstanceData{CreatedAt: created}))
	assert.True(t, ArchiveTimeFor(InstanceData{}).IsZero())
}

// TestRenameArchived_PrunedTombstoneDoesNotLockTwice pins the deadlock CI
// caught on the first cut of this path: RenameArchived holds i.mu, so the
// pruned-tombstone branch must read i.prunedAt directly — routing through
// IsPruned() takes RLock on a held RWMutex and hangs forever. The goroutine +
// timeout makes the pre-fix shape fail in seconds rather than hang the suite
// (daemon TestReserveCreate_ReuseArchivedNameCutOffRecordsTheLocation did
// exactly that on the unfixed head).
func TestRenameArchived_PrunedTombstoneDoesNotLockTwice(t *testing.T) {
	worktreePath := filepath.Join(t.TempDir(), "archived-wt")
	gw, err := git.NewGitWorktreeFromStorage("/repo", worktreePath, "old-title", "dev/old-title", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title:       "old-title",
		Path:        "/repo",
		Branch:      "dev/old-title",
		liveness:    LiveArchived,
		gitWorktree: gw,
	}
	inst.MarkPruned(time.Now())
	require.True(t, inst.IsPruned(), "precondition: the row is a tombstone")

	dest := filepath.Join(t.TempDir(), "freed-title")
	done := make(chan error, 1)
	go func() { done <- inst.RenameArchived("freed-title", dest, "") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RenameArchived deadlocked on a pruned tombstone — it must not reacquire i.mu")
	}
	assert.Equal(t, "freed-title", inst.Title, "the tombstone's title must move aside")
	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr), "a tombstone has no worktree — nothing may be created at dest")
}

func TestDirSizeBytes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a"), make([]byte, 100), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b"), make([]byte, 50), 0o644))

	total, err := DirSizeBytes(root)
	require.NoError(t, err)
	assert.Equal(t, int64(150), total)

	missing, err := DirSizeBytes(filepath.Join(root, "gone"))
	require.NoError(t, err, "a root deleted out-of-band is already reclaimed, not an error")
	assert.Zero(t, missing)
}
