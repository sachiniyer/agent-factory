package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
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
		{"no recorded branch", func(d *InstanceData) {
			d.Branch = ""
			d.Worktree.BranchName = ""
		}, "no branch"},
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

// TestArchivedAtRoundTrip locks the serialized contract: archived_at survives
// InstanceData → JSON → FromInstanceData → ToInstanceData so the prune cutoff
// measures the same instant after a daemon reload.
func TestArchivedAtRoundTrip(t *testing.T) {
	archived := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Millisecond)

	data := pruneEligibleData(t)
	data.ArchivedAt = archived
	payload, err := json.Marshal(data)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(payload, &wire))
	require.Contains(t, wire, "archived_at", "archived_at must be on the durable wire")

	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	require.True(t, archived.Equal(inst.ArchivedAt()),
		"require.Equal is DeepEqual — it compares the location pointer too, so this fails off-UTC")

	storedPayload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	var decoded InstanceData
	require.NoError(t, json.Unmarshal(storedPayload, &decoded))
	require.True(t, decoded.ArchivedAt.Equal(archived),
		"archived_at must round-trip the same instant (JSON normalizes the location)")

	reloaded, err := FromInstanceData(decoded)
	require.NoError(t, err)
	require.True(t, archived.Equal(reloaded.ArchivedAt()),
		"the shelf time must survive a daemon reload — JSON decodes to UTC, so compare instants not locations")
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

// TestRestoreClearsArchivedAt: archived_at names the shelf the record sits on
// — leaving the archive (restore's begin edge, fenced or plain) must clear it,
// or a live row keeps reporting a dead shelf's age to readers and to a later
// prune pass (#5136 review).
func TestRestoreClearsArchivedAt(t *testing.T) {
	inst := &Instance{liveness: LiveRunning, started: true}
	require.NoError(t, inst.Transition(BeginArchive()))
	require.NoError(t, inst.Transition(CommitArchive()))
	require.False(t, inst.ArchivedAt().IsZero())

	require.NoError(t, inst.Transition(BeginRestore()))
	assert.True(t, inst.ArchivedAt().IsZero(),
		"the row left the archive — its archive time must not survive as a stale shelf age")
}

func TestDirSizeBytes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a"), make([]byte, 100), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b"), make([]byte, 50), 0o644))

	total, err := DirSizeBytes(root)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, int64(150),
		"allocated blocks must cover every byte actually written")
	assert.Zero(t, total%512, "the measure is 512-byte blocks")

	missing, err := DirSizeBytes(filepath.Join(root, "gone"))
	require.NoError(t, err, "a root deleted out-of-band is already reclaimed, not an error")
	assert.Zero(t, missing)
}

// TestDirSizeBytes_AllocatedNotApparent: reclaimable_bytes is the number the
// operator reads, so it must be the disk space deletion frees — a sparse
// file's apparent gigabytes reclaim only its populated extents, and a
// hard-linked file whose last link lives OUTSIDE the tree frees nothing at
// all (#5136 Codex round 4).
func TestDirSizeBytes_AllocatedNotApparent(t *testing.T) {
	root := t.TempDir()

	// Sparse: a 1 GiB apparent file whose blocks were never written.
	sparse := filepath.Join(root, "sparse.bin")
	f, err := os.Create(sparse)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<30))
	require.NoError(t, f.Close())
	info, err := os.Stat(sparse)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "this test asserts allocated-block accounting")
	if st.Blocks*512 >= info.Size() {
		t.Skip("this filesystem does not produce sparse files")
	}

	total, err := DirSizeBytes(root)
	require.NoError(t, err)
	assert.Less(t, total, info.Size(),
		"deletion frees only the populated extents, not the apparent 1 GiB")

	// Hard-linked entirely within the tree: the inode's blocks free once.
	require.NoError(t, os.Remove(sparse))
	target := filepath.Join(root, "data.bin")
	require.NoError(t, os.WriteFile(target, make([]byte, 256<<10), 0o644))
	require.NoError(t, os.Link(target, filepath.Join(root, "alias.bin")))
	targetSt, err := os.Stat(target)
	require.NoError(t, err)
	targetBlocks := targetSt.Sys().(*syscall.Stat_t).Blocks * 512
	require.Greater(t, targetBlocks, int64(0))
	total, err = DirSizeBytes(root)
	require.NoError(t, err)
	assert.Less(t, total, 2*targetBlocks+8*1024,
		"two names for one inode inside the tree free its blocks once, not twice")

	// Hard-linked to a name OUTSIDE the tree: deleting root leaves the inode
	// alive, so its blocks free ZERO times.
	require.NoError(t, os.Remove(filepath.Join(root, "alias.bin")))
	outside := filepath.Join(t.TempDir(), "keep.bin")
	require.NoError(t, os.Link(target, outside))
	total, err = DirSizeBytes(root)
	require.NoError(t, err)
	assert.Less(t, total, targetBlocks,
		"a link surviving outside the tree means deleting it frees nothing")
}
