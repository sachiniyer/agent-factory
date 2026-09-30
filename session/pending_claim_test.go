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
