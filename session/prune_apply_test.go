package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func prunedBaseline(t *testing.T) InstanceData {
	d := pruneEligibleData(t)
	d.ID = "id-1"
	return d
}

// A pruned row is a tombstone: it round-trips through the persisted form, offers
// no lifecycle verb (restore would refuse it), and carries its intent until the
// tombstone replaces it.
func TestPrunedRowRoundTripsAndOffersNoRestore(t *testing.T) {
	d := prunedBaseline(t)
	inst, err := FromInstanceData(d)
	require.NoError(t, err)
	require.Equal(t, LifecycleActionRestore, inst.LifecycleAction(), "premise: an ordinary archive offers restore")

	intent := PruneIntentData{WorktreePath: "/a/wt", TombstonePath: "/a/.prune-x", Head: "abc", Branch: "siyer/old-session", At: time.Now()}
	require.NoError(t, inst.SetPruneIntent(intent))
	got := inst.ToInstanceData()
	require.NotNil(t, got.PruneIntent)
	assert.Equal(t, "/a/.prune-x", got.PruneIntent.TombstonePath)
	got.PruneIntent.TombstonePath = "mutated"
	assert.Equal(t, "/a/.prune-x", inst.PruneIntent().TombstonePath, "the projection is a copy")

	require.NoError(t, inst.MarkPruneRenamed())
	require.NoError(t, inst.CompletePrune(time.Now()))
	assert.True(t, inst.IsPruned())
	assert.Nil(t, inst.PruneIntent())
	assert.Equal(t, LifecycleActionNone, inst.LifecycleAction())

	again, err := FromInstanceData(inst.ToInstanceData())
	require.NoError(t, err)
	assert.True(t, again.IsPruned(), "pruned_at survives a reload")
	assert.Equal(t, LifecycleActionNone, again.ToInstanceData().LifecycleAction)
	assert.Contains(t, PruneSkipReason(again.ToInstanceData(), time.Now()), "already pruned")
}

func TestPruneIntentRefusesAConcurrentOrRepeatedClaim(t *testing.T) {
	inst, err := FromInstanceData(prunedBaseline(t))
	require.NoError(t, err)
	require.NoError(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a", At: time.Now()}))
	require.Error(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a"}), "a second run cannot also own the deletion")
	inst.ClearPruneIntent()
	require.NoError(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a", At: time.Now()}))
	require.NoError(t, inst.CompletePrune(time.Now()))
	require.Error(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a"}), "a tombstone is not pruned twice")
}

func TestPrunedRestoreErrorNamesTheKeptBranch(t *testing.T) {
	err := PrunedRestoreError("old", "siyer/old", time.Now())
	assert.Contains(t, err.Error(), "siyer/old")
	assert.Contains(t, err.Error(), "git worktree add")
}

// Prune's own bookkeeping must not move UpdatedAt: for a legacy row it IS the
// archive time, and a prune that advanced it would make its interrupted work look
// too recent to resume (Codex on #6186).
func TestPruneBookkeepingDoesNotMoveTheArchiveTimeOfALegacyRow(t *testing.T) {
	d := prunedBaseline(t)
	d.ArchivedAt = time.Time{} // archived before archived_at existed
	inst, err := FromInstanceData(d)
	require.NoError(t, err)
	before := ArchiveTimeFor(inst.ToInstanceData())

	require.NoError(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a", At: time.Now()}))
	require.NoError(t, inst.MarkPruneRenamed())
	inst.ClearPruneIntent()
	require.NoError(t, inst.SetPruneIntent(PruneIntentData{WorktreePath: "/a", At: time.Now()}))
	assert.True(t, before.Equal(ArchiveTimeFor(inst.ToInstanceData())))
	assert.Contains(t, PruneSkipReason(inst.ToInstanceData(), time.Now()), "interrupted")
}

// A row whose branch lives only in Worktree.BranchName still names it (Codex on
// #6186): the refusal and the verdict resolve the kept branch the same way.
func TestKeptBranchFallsBackToTheWorktreesCopy(t *testing.T) {
	d := prunedBaseline(t)
	d.Branch = ""
	d.Worktree.BranchName = "siyer/legacy"
	assert.Equal(t, "siyer/legacy", KeptBranchOf(d))
	inst, err := FromInstanceData(d)
	require.NoError(t, err)
	assert.Equal(t, "siyer/legacy", inst.KeptBranch())
	d.Branch = "siyer/top"
	assert.Equal(t, "siyer/top", KeptBranchOf(d))
}
