package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPendingCreateRowWithoutBackendClaimsOffBox (#4562 review): the daemon
// publishes its pending-create projection before the runtime exists, and older
// daemons publish it with no backend discriminator at all. Such a row
// materializes with the default local backend, but nothing about it is
// known-local — its title claim must answer off-box so admission judges it
// under the global prefix, the way the daemon judges a genuinely off-box
// create. A pending row that DOES carry a backend is classified by it.
func TestPendingCreateRowWithoutBackendClaimsOffBox(t *testing.T) {
	worktree := GitWorktreeData{RepoPath: "/repo", WorktreePath: "/repo/wt", BranchName: "global/x"}
	unclassified, err := FromInstanceData(InstanceData{
		Title: "pending", Status: Loading, Liveness: LiveReady, InFlightOp: OpCreating,
		Worktree: worktree,
	})
	require.NoError(t, err)
	assert.False(t, unclassified.BranchClaim().Local,
		"a pending create that cannot say where it runs is not a host-local claim")

	typed, err := FromInstanceData(InstanceData{
		Title: "pending", Status: Loading, Liveness: LiveReady, InFlightOp: OpCreating,
		BackendType: "docker",
	})
	require.NoError(t, err)
	assert.False(t, typed.BranchClaim().Local)

	pendingLocal, err := FromInstanceData(InstanceData{
		Title: "pending", Status: Loading, Liveness: LiveReady, InFlightOp: OpCreating,
		BackendType: "local", Worktree: worktree,
	})
	require.NoError(t, err)
	assert.True(t, pendingLocal.BranchClaim().Local,
		"a pending host-local create keeps its local claim")

	settled := InstanceData{
		Title: "running", Liveness: LiveRunning, Status: Running, Worktree: worktree,
	}.BranchClaim()
	assert.True(t, settled.Local,
		"control: a settled row with no discriminator remains a local claim")
	assert.True(t, (&Instance{Title: "running", backend: &LocalBackend{}}).BranchClaim().Local,
		"control: a settled instance with no pending marker remains a local claim")
}

// TestRelinquishedBranchIsPersistedNotImpliedByArchived (#4562 review): an
// archived row that was never renamed for title reuse still owns the branch
// its record points at — its worktree may sit on it, and its restore still
// names it. Marking every archived row relinquished lets a later create that
// derives the recorded branch adopt it out from under the record. Only a row
// whose reuse rename deliberately left the branch (RelinquishedBranch) yields
// the defense.
func TestRelinquishedBranchIsPersistedNotImpliedByArchived(t *testing.T) {
	wt := GitWorktreeData{RepoPath: "/repo", WorktreePath: "/arch/wt", BranchName: "foo-x"}
	unrenamed := InstanceData{
		Title: "#x", Liveness: LiveArchived, Status: Archived, Branch: "foo-x",
		Worktree: wt,
	}
	assert.False(t, unrenamed.BranchClaim().Relinquished,
		"an archived row never renamed still defends the branch its record points at")

	yielded := InstanceData{
		Title: "foo (archived)", Liveness: LiveArchived, Status: Archived,
		Branch: "global/foo", RelinquishedBranch: true,
		Worktree: GitWorktreeData{RepoPath: "/repo", WorktreePath: "/arch/wt2", BranchName: "global/foo"},
	}
	assert.True(t, yielded.BranchClaim().Relinquished,
		"the reuse rename's deliberate leave persists as the claim's Relinquished")

	// And the flag round-trips through the instance: FromInstanceData restores
	// what ToInstanceData wrote, so a daemon restart cannot turn a yielded row
	// back into a defender or an owning row into a yielded one.
	inst, err := FromInstanceData(yielded)
	require.NoError(t, err)
	assert.True(t, inst.BranchClaim().Relinquished)
	assert.True(t, inst.ToInstanceData().RelinquishedBranch,
		"the flag must ride ToInstanceData or the next restart loses it")

	kept, err := FromInstanceData(unrenamed)
	require.NoError(t, err)
	assert.False(t, kept.BranchClaim().Relinquished)
	assert.False(t, kept.ToInstanceData().RelinquishedBranch)
}
