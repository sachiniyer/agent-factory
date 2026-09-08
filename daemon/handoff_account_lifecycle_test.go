package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountCheckpointFailureLeavesRecoverableOldIdentity(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	failed, _, heal := fullDiskFor(t, inst.Title, errors.New("checkpoint unavailable"))
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "checkpoint unavailable")
	require.Positive(t, failed())
	require.Equal(t, session.LiveLost, inst.GetLiveness(), "the stopped old runtime must never be reported healthy")
	require.Equal(t, "work", inst.Account)
	require.Empty(t, inst.Handoffs())
	require.Equal(t, session.OpNone, inst.GetInFlightOp())
	require.Contains(t, m.settleOwed, stableSessionKey(repo, inst))
	heal()
	m.FlushOwedSettlements()
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.Equal(t, session.LiveLost, saved.Liveness)
	require.Nil(t, saved.PendingAccountSwap, "the uncommitted swap must not fence ordinary recovery")
	m.RestoreLostSessions()
	recovered, _, _ := backend.snapshot()
	require.Equal(t, 1, recovered)
}

func TestHandoffAccountRestartRefreshPreservesHealthyCheckpoint(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", false, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	require.NoError(t, inst.BeginManualAccountSwap())
	require.NoError(t, inst.ValidateManualAccountSwap("personal", "claude"))
	_, err = inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	require.NoError(t, m.persistSettlement(repo, daemonInstanceKey(repo, inst.Title), inst))
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.Equal(t, session.LiveRunning, saved.Liveness)
	restored, err := session.FromInstanceData(saved)
	require.NoError(t, err)
	restored.SetBackend(backend)
	restarted, err := NewManager(m.Config())
	require.NoError(t, err)
	restarted.instances[daemonInstanceKey(repo, restored.Title)] = restored
	// Match the production poll order on a new manager and a disk-restored row.
	restarted.RefreshStatuses()
	require.Equal(t, session.LiveRunning, restored.GetLiveness(), "status reconciliation must leave the transaction with its owner")
	restarted.RestoreLostSessions()
	restarted.ResumeLimitedSessions()
	recovered, respawns, prompts := backend.snapshot()
	require.Zero(t, recovered)
	require.Equal(t, 1, respawns)
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], "to claude account")
	_, _, pending := restored.PendingAccountSwap()
	require.False(t, pending)
}

func TestHandoffAccountOmittedAgentResolvesUnderLock(t *testing.T) {
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	inst.Account = "work"
	inst.ClearLimitReached()
	backend.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
	previous := testHookHandoffAccountBeforeTargetLock
	defer func() { testHookHandoffAccountBeforeTargetLock = previous }()
	testHookHandoffAccountBeforeTargetLock = func() {
		unlock := m.lockTarget(repo, inst.Title)
		defer unlock()
		// Another handoff completes before this request acquires the target lock.
		_, err := inst.SwapAgentProgram("claude", session.HandoffReasonManual, "tip", false)
		require.NoError(t, err)
		inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "claude"))
	}
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err)
	require.Equal(t, "claude", resp.From)
	require.Equal(t, "claude", resp.To)
	require.Equal(t, "personal", inst.Account)
}

func TestHandoffAccountHealthyPendingSwapRetainsResumeBackoff(t *testing.T) {
	advance := withFrozenClock(t)
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", nowFunc().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	m.cfg.LimitAutoResume = false
	require.NoError(t, inst.BeginManualAccountSwap())
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	inst.EndLimitResume()
	// A replacement pane exists, but preflight cannot repair the incomplete pane set.
	inst.Program = "unrecognized-wrapper"
	key := stableSessionKey(repo, inst)
	m.ResumeLimitedSessions()
	require.Equal(t, session.LiveRunning, inst.GetLiveness())
	require.Equal(t, 1, m.limitResumeStates[key].attempts)
	advance(limitResumeBackoffBase)
	m.ResumeLimitedSessions()
	require.Equal(t, 2, m.limitResumeStates[key].attempts, "pending healthy transactions must retain their retry episode")
	require.Equal(t, nowFunc().Add(2*limitResumeBackoffBase), m.limitResumeStates[key].nextAttempt)
	advance(limitResumeBackoffBase)
	m.ResumeLimitedSessions()
	require.Equal(t, 2, m.limitResumeStates[key].attempts, "the doubled backoff must suppress an early retry")
}

func TestHandoffAccountCommittedCustomCommandUsesAgentNamespace(t *testing.T) {
	_, _, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	inst.Account = "work"
	inst.Program = "claude --model opus"
	inst.ClearLimitReached()
	require.NoError(t, inst.BeginManualAccountSwap())
	_, err := inst.SelectAccountForHandoff("work", "personal", "claude", session.HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	inst.EndLimitResume()
	swap := committedAccountSwap(inst)
	require.NotNil(t, swap)
	require.Equal(t, "claude", swap.agent, "the account namespace must not include command arguments")
	require.Equal(t, "claude --model opus", inst.AgentProgram())
}
