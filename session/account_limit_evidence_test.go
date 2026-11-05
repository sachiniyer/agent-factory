package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// AccountLimitEvidenceFromData must name only the identity whose runtime
// produced the banner. A manual handoff off the default login persists a row
// whose account is the committed REPLACEMENT — pinned, with the outgoing
// identity's reset time and an empty limit_account — while its
// pending_account_swap says so. Reading that account as a pre-limit_account
// pin invents a wall the incoming identity never produced (#6764): sibling
// disk scans then refuse the healthy target, and a daemon restart in the
// commit→delivery window reloads the row as a real observation.
func TestAccountLimitEvidenceFromDataCommittedSwapAttribution(t *testing.T) {
	resetAt := time.Date(2026, 11, 3, 6, 34, 0, 0, time.UTC)
	base := InstanceData{
		ID:           "swapped",
		Title:        "swapped",
		Program:      tmux.ProgramCodex,
		Liveness:     LiveLimitReached,
		LimitAgent:   tmux.ProgramCodex,
		LimitResetAt: resetAt,
		Account:      "target",
	}

	t.Run("ambient outgoing identity leaves no named evidence", func(t *testing.T) {
		data := base
		data.PendingAccountSwap = &AccountSwapData{Manual: true, From: "", To: "target"}
		account, observations := AccountLimitEvidenceFromData(data)
		require.Equal(t, "", account,
			"the recorded account is the committed replacement, not the wall's producer")
		require.Empty(t, observations,
			"an account change must not manufacture a limit observation for the incoming identity")
	})

	t.Run("named outgoing identity keeps its own wall", func(t *testing.T) {
		data := base
		data.PendingAccountSwap = &AccountSwapData{Manual: true, From: "work", To: "target"}
		account, observations := AccountLimitEvidenceFromData(data)
		require.Equal(t, "work", account,
			"the wall belongs to the outgoing identity the swap recorded")
		require.Equal(t, []AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ResetAt: resetAt},
		}, observations)
	})

	t.Run("legacy explicit pin still rolls forward", func(t *testing.T) {
		data := base
		data.Account = "work"
		account, observations := AccountLimitEvidenceFromData(data)
		require.Equal(t, "work", account)
		require.Equal(t, []AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "work", ResetAt: resetAt},
		}, observations,
			"a pre-limit_account row with no committed swap is still a legacy pin")
	})

	t.Run("a wall attributed to the incoming identity is kept", func(t *testing.T) {
		data := base
		data.LimitAccount = "target"
		data.PendingAccountSwap = &AccountSwapData{Manual: true, From: "", To: "target"}
		account, observations := AccountLimitEvidenceFromData(data)
		require.Equal(t, "target", account,
			"ParkManualAccountSwapAtLimit evidence names the incoming identity's own wall")
		require.Equal(t, []AccountLimitObservationData{
			{Agent: tmux.ProgramCodex, Account: "target", ResetAt: resetAt},
		}, observations)
	})

	t.Run("a restart reloads no incoming-identity observation", func(t *testing.T) {
		data := base
		data.Path = t.TempDir()
		data.Worktree = GitWorktreeData{
			RepoPath:     t.TempDir(),
			WorktreePath: t.TempDir(),
			SessionName:  "swapped",
			BranchName:   "af/swapped",
		}
		data.PendingAccountSwap = &AccountSwapData{Manual: true, From: "", To: "target"}
		inst, err := FromInstanceData(data)
		require.NoError(t, err)
		agent, account, ok := inst.LimitIdentity()
		require.True(t, ok)
		require.Equal(t, tmux.ProgramCodex, agent)
		require.Equal(t, "", account,
			"a restart inside the commit→delivery window must not load the replacement as limited")
		require.Empty(t, inst.AccountLimitObservations())
	})
}
