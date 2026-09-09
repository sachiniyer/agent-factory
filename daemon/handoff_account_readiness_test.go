package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

type accountReadinessBackend struct {
	*limitResumeBackend
	previewed chan struct{}
	release   chan struct{}
	once      sync.Once
	trust     bool
	limited   bool
}

type accountReadinessGoneBackend struct{ *limitResumeBackend }

type accountDeliveryInspectBackend struct {
	*accountReadinessBackend
	beforeSend func()
}

func (b *accountDeliveryInspectBackend) SendPromptCommandWithStatus(
	i *session.Instance, prompt string,
) (session.PromptDeliveryStatus, error) {
	if b.beforeSend != nil {
		b.beforeSend()
	}
	return b.limitResumeBackend.SendPromptCommandWithStatus(i, prompt)
}

func (b *accountReadinessGoneBackend) Preview(*session.Instance) (string, error) {
	return "", tmux.ErrSessionGone
}

func TestHandoffAccountReadinessFailureBecomesInert(t *testing.T) {
	m, repo, inst, base := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	backend := &accountReadinessGoneBackend{limitResumeBackend: base}
	inst.SetBackend(backend)
	inst.ClearLimitReached()
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorIs(t, err, task.ErrAgentReadiness)
	require.True(t, isMutationCommitted(err))
	require.Equal(t, "personal", resp.ToAccount)
	require.True(t, inst.StartupStateUnknown())
	require.NotNil(t, persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap)
	_, beforeRespawns, _ := backend.snapshot()
	m.ResumeLimitedSessions()
	_, afterRespawns, _ := backend.snapshot()
	require.Equal(t, beforeRespawns, afterRespawns, "an unconfirmed replacement must not be respawned")
}

func TestHandoffAccountReadinessFailureRetriesInertSettlement(t *testing.T) {
	m, repo, inst, base := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	backend := &accountReadinessGoneBackend{limitResumeBackend: base}
	inst.SetBackend(backend)
	inst.ClearLimitReached()
	previous := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = previous })
	fail := true
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		actualStartupUnknown := data.PendingAccountSwap != nil &&
			data.PendingAccountSwap.OriginalStartupStateUnknown != nil &&
			*data.PendingAccountSwap.OriginalStartupStateUnknown
		if fail && data.Title == inst.Title && actualStartupUnknown {
			return errors.New("startup-unknown disk unavailable")
		}
		return nil
	}

	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorIs(t, err, task.ErrAgentReadiness)
	require.ErrorContains(t, err, "startup-unknown disk unavailable")
	require.True(t, inst.StartupStateUnknown())
	require.Contains(t, m.settleOwed, stableSessionKey(repo, inst))
	beforeRetry := persistedInstanceByTitle(t, repo, inst.Title).RestoreAccountSwapRollbackFence()
	require.False(t, beforeRetry.StartupStateUnknown)

	fail = false
	m.FlushOwedSettlements()
	saved := persistedInstanceByTitle(t, repo, inst.Title).RestoreAccountSwapRollbackFence()
	require.True(t, saved.StartupStateUnknown)
	require.False(t, inst.Started())
	_, beforeRespawns, _ := backend.snapshot()
	m.ResumeLimitedSessions()
	_, afterRespawns, _ := backend.snapshot()
	require.Equal(t, beforeRespawns, afterRespawns,
		"the restart scheduler must not retry a replacement whose startup state is unknown")
}

func (b *accountReadinessBackend) Preview(*session.Instance) (string, error) {
	b.once.Do(func() { close(b.previewed) })
	<-b.release
	if b.limited {
		return "Claude usage limit reached. Your limit will reset at 2pm (UTC)", nil
	}
	return "ready\n❯\n›\n> \n╰", nil
}
func (b *accountReadinessBackend) CheckAndHandleTrustPrompt(*session.Instance) bool {
	if b.trust {
		b.trust = false
		return true
	}
	return false
}

func TestHandoffAccountWaitsForReadinessAndTrust(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	m, repo, inst, base := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	prepareHandoffTargetPreflight(t, inst)
	inst.ClearLimitReached()
	inst.Program = "codex"
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "codex"))
	base.onRespawn = func(i *session.Instance) { i.SetTmuxSession(tmux.NewTmuxSession(i.Title, i.AgentProgram())) }
	b := &accountReadinessBackend{limitResumeBackend: base, previewed: make(chan struct{}), release: make(chan struct{}), trust: true}
	inst.SetBackend(b)
	done := make(chan error, 1)
	go func() {
		_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, To: "claude", Account: "personal"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("handoff returned before readiness: %v", err)
	case <-b.previewed:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement never probed for readiness")
	}
	_, _, prompts := base.snapshot()
	require.Empty(t, prompts)
	require.NotNil(t, persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap)
	close(b.release)
	require.NoError(t, <-done)
	require.False(t, b.trust, "trust dialog must be handled before delivery")
	_, _, prompts = base.snapshot()
	require.Len(t, prompts, 1)
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
}

func TestHandoffAccountReadinessLimitRetainsMission(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	m, repo, inst, base := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	b := &accountReadinessBackend{limitResumeBackend: base, previewed: make(chan struct{}), release: make(chan struct{}), limited: true}
	close(b.release)
	inst.SetBackend(b)
	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.Error(t, err)
	require.True(t, isMutationCommitted(err))
	require.Equal(t, "personal", resp.ToAccount)
	_, _, prompts := base.snapshot()
	require.Empty(t, prompts)
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.NotNil(t, saved.PendingAccountSwap)
	require.Equal(t, session.LiveLimitReached, saved.Liveness)
	_, observations := session.AccountLimitEvidenceFromData(saved)
	require.NotEmpty(t, observations)
	require.Equal(t, "personal", observations[len(observations)-1].Account)
	b.limited = false
	// Explicit retry preserves the same transaction and delivers exactly once.
	err = m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
	_, _, prompts = base.snapshot()
	require.Len(t, prompts, 1)
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
}

func TestManualAccountHandoffRetryFencesMissionBeforeSubmission(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	m, repo, inst, base := newAutoResumeManager(t, "", true, "finish migration", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	readiness := &accountReadinessBackend{
		limitResumeBackend: base,
		previewed:          make(chan struct{}),
		release:            make(chan struct{}),
		limited:            true,
	}
	close(readiness.release)
	backend := &accountDeliveryInspectBackend{accountReadinessBackend: readiness}
	inst.SetBackend(backend)

	_, err := m.HandoffSession(HandoffSessionRequest{
		Title: inst.Title, RepoID: repo, Account: "personal",
	})
	require.Error(t, err)
	require.Equal(t, session.PromptNotDelivered,
		persistedInstanceByTitle(t, repo, inst.Title).PendingAccountSwap.MissionDeliveryStatus)

	readiness.limited = false
	var statusAtSubmission session.PromptDeliveryStatus
	backend.beforeSend = func() {
		statusAtSubmission = persistedInstanceByTitle(t, repo, inst.Title).
			RestoreAccountSwapRollbackFence().PendingAccountSwap.MissionDeliveryStatus
	}
	require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
	require.Equal(t, session.PromptCouldNotConfirm, statusAtSubmission,
		"durable positive non-delivery evidence must be fenced before the composer is touched")
}
