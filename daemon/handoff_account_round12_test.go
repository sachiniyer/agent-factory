package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func TestResumeLimitedSessions_CrossAgentSameLabelDoesNotUseOutgoingReset(t *testing.T) {
	advance := withFrozenClock(t)
	m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue", nowFunc().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "work")
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	_, err = agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)
	prepareHandoffTargetPreflight(t, inst)
	pathEntries := filepath.SplitList(os.Getenv("PATH"))
	require.NotEmpty(t, pathEntries)
	require.NoError(t, os.WriteFile(filepath.Join(pathEntries[0], tmux.ProgramCodex), []byte("#!/bin/sh\nexit 0\n"), 0o700))

	// The persisted wall belongs to the outgoing Claude identity. The pending
	// transaction deliberately reuses its label in Codex's separate namespace.
	inst.ClearLimitReached()
	inst.Account = "work"
	inst.SetLimitReached(nowFunc().Add(time.Hour))
	require.NoError(t, inst.BeginManualAccountSwap())
	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramCodex, true))
	_, err = inst.SelectAccountForHandoff("work", "work", tmux.ProgramCodex, tmux.ProgramCodex, true,
		session.HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	inst.EndLimitResume()
	m.cfg.LimitAutoResume = false
	restored, err := session.FromInstanceData(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	restored.SetBackend(backend)
	m.instances[daemonInstanceKey(repo, inst.Title)] = restored
	inst = restored
	stateKey := stableSessionKey(repo, inst)

	advance(time.Second)
	m.ResumeLimitedSessions()

	require.Equal(t, 1, m.limitResumeStates[stateKey].attempts,
		"an outgoing Claude reset must not delay the pending Codex replacement merely because both labels are work")
}

type unconfirmedAccountDeliveryBackend struct {
	*accountReadinessBackend
	mu       sync.Mutex
	attempts int
}

func (b *unconfirmedAccountDeliveryBackend) SendPromptCommandWithStatus(
	*session.Instance, string,
) (session.PromptDeliveryStatus, error) {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	return session.PromptCouldNotConfirm, errors.New("prompt submission reply lost")
}

func (b *unconfirmedAccountDeliveryBackend) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

type unconfirmedNilAccountDeliveryBackend struct {
	*accountReadinessBackend
}

func (b *unconfirmedNilAccountDeliveryBackend) SendPromptCommandWithStatus(
	*session.Instance, string,
) (session.PromptDeliveryStatus, error) {
	return session.PromptCouldNotConfirm, nil
}

func TestHandoffAccount_UnconfirmedNilDeliveryKeepsPendingTransaction(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	m, repo, inst, base := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	readiness := &accountReadinessBackend{
		limitResumeBackend: base,
		previewed:          make(chan struct{}),
		release:            make(chan struct{}),
	}
	close(readiness.release)
	inst.SetBackend(&unconfirmedNilAccountDeliveryBackend{accountReadinessBackend: readiness})

	resp, err := m.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repo, Account: "personal",
	})
	require.ErrorIs(t, err, task.ErrPromptDelivery)
	require.True(t, isMutationCommitted(err))
	require.Equal(t, "personal", resp.ToAccount)
	require.NotNil(t, inst.ToInstanceData().PendingAccountSwap,
		"a non-delivered verdict must not retire the in-memory transaction")
	require.NotNil(t, persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap,
		"a non-delivered verdict must not retire the durable transaction")
}

func TestHandoffAccount_UnconfirmedDeliveryIsNotAutomaticallyRedelivered(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	m, repo, inst, base := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	readiness := &accountReadinessBackend{
		limitResumeBackend: base,
		previewed:          make(chan struct{}),
		release:            make(chan struct{}),
	}
	close(readiness.release)
	backend := &unconfirmedAccountDeliveryBackend{accountReadinessBackend: readiness}
	inst.SetBackend(backend)
	m.cfg.LimitAutoResume = false

	_, err := m.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repo, Account: "personal",
	})
	require.Error(t, err)
	require.True(t, isMutationCommitted(err))
	require.Equal(t, 1, backend.attemptCount())
	require.NotNil(t, inst.ToInstanceData().PendingAccountSwap)
	// A later, unrelated prompt attempt is session-wide evidence. It must not
	// change the retry verdict for this pending handoff mission.
	require.True(t, inst.RecordPromptAttempt(session.PromptNotDelivered, time.Now().Add(time.Second)))

	m.ResumeLimitedSessions()

	require.Equal(t, 1, backend.attemptCount(),
		"could-not-confirm may have submitted the mission and must never authorize automatic redelivery")

	require.NoError(t, inst.RecordPendingManualAccountSwapMissionDelivery(
		"", "personal", session.PromptNotDelivered,
	))
	m.ResumeLimitedSessions()
	require.Equal(t, 2, backend.attemptCount(),
		"positive non-delivery evidence for the pending mission must authorize recovery")
}

func TestHandoffSession_PinnedAccountRequiresTargetAccount(t *testing.T) {
	t.Run("pinned refuses before swap", func(t *testing.T) {
		m, repo, path := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, m, repo, path, "pinned", backend)
		inst.Account = "work"

		_, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repo, To: tmux.ProgramGemini,
		})
		require.ErrorContains(t, err, "target account")
		require.ErrorContains(t, err, "--account")
		swaps, prompts := backend.snapshot()
		require.Zero(t, swaps)
		require.Empty(t, prompts)
		require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
	})

	t.Run("ambient remains agent-only", func(t *testing.T) {
		m, repo, path := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, m, repo, path, "ambient", backend)

		resp, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repo, To: tmux.ProgramGemini,
		})
		require.NoError(t, err)
		require.Equal(t, tmux.ProgramGemini, resp.To)
		swaps, prompts := backend.snapshot()
		require.Equal(t, 1, swaps)
		require.Len(t, prompts, 1)
	})
}

// A scoped session may change agents — the account question is answered by the
// TARGET's capability, never by reusing the outgoing name (#4428). A target
// with no account namespace drops the scope in the record transaction and the
// response reports the drop on from_account; a target with one refuses without
// --account whether the scope was pinned or af-selected.
func TestHandoffSession_ScopeDecisionByTargetCapability(t *testing.T) {
	t.Run("auto-scoped refuses a scopable target without --account", func(t *testing.T) {
		m, repo, path := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, m, repo, path, "auto-scoped", backend)
		require.True(t, inst.ReconcileAccountHandoffSnapshot("work", "claude", true, nil))

		_, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repo, To: tmux.ProgramGemini,
		})
		require.ErrorContains(t, err, "--account")
		swaps, prompts := backend.snapshot()
		require.Zero(t, swaps)
		require.Empty(t, prompts)
		require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
		if account, auto := inst.AccountSelection(); account != "work" || !auto {
			t.Fatalf("AccountSelection = (%q, %v) after the refusal, want the scope untouched",
				account, auto)
		}
	})

	t.Run("pinned drops the scope for a target with no account support", func(t *testing.T) {
		m, repo, path := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, m, repo, path, "descoped-pinned", backend)
		inst.Account = "work"

		resp, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repo, To: tmux.ProgramAider,
		})
		require.NoError(t, err)
		require.Equal(t, tmux.ProgramAider, resp.To)
		require.Equal(t, "work", resp.FromAccount,
			"the dropped scope is reported so the client can say what happened to it")
		require.Empty(t, resp.ToAccount)
		if account, auto := inst.AccountSelection(); account != "" || auto {
			t.Fatalf("AccountSelection = (%q, %v) after the swap, want the scope dropped — "+
				"aider has no account namespace for the name to resolve in", account, auto)
		}
		handoffs := inst.Handoffs()
		require.Len(t, handoffs, 1)
		require.Equal(t, "work", handoffs[0].FromAccount)
		require.Empty(t, handoffs[0].ToAccount)
		swaps, prompts := backend.snapshot()
		require.Equal(t, 1, swaps)
		require.Len(t, prompts, 1)
	})

	t.Run("auto-scoped drops the scope for a target with no account support", func(t *testing.T) {
		m, repo, path := newStatusTestManager(t)
		backend := &handoffBackend{FakeBackend: session.NewFakeBackend()}
		inst := registerHandoffSubject(t, m, repo, path, "descoped-auto", backend)
		require.True(t, inst.ReconcileAccountHandoffSnapshot("work", "claude", true, nil))

		resp, err := m.HandoffSession(HandoffSessionRequest{
			Title: inst.Title, RepoID: repo, To: tmux.ProgramAider,
		})
		require.NoError(t, err)
		require.Equal(t, "work", resp.FromAccount)
		require.Empty(t, resp.ToAccount)
		if account, auto := inst.AccountSelection(); account != "" || auto {
			t.Fatalf("AccountSelection = (%q, %v) after the swap, want the af-selected scope "+
				"dropped the same way a pinned one is", account, auto)
		}
		swaps, _ := backend.snapshot()
		require.Equal(t, 1, swaps)
	})
}
