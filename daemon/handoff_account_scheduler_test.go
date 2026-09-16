package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

func parkManualHandoffOnIncomingLimit(
	t *testing.T, resetAfter time.Duration,
) (*Manager, string, *session.Instance, *accountReadinessBackend, func(time.Duration)) {
	t.Helper()
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	advance := withFrozenClock(t)
	m, repo, inst, base := newAutoResumeManager(t, "", true, "continue", nowFunc().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.ClearLimitReached()
	b := &accountReadinessBackend{
		limitResumeBackend: base,
		previewed:          make(chan struct{}),
		release:            make(chan struct{}),
		limited:            true,
	}
	close(b.release)
	inst.SetBackend(b)

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.Error(t, err)
	require.True(t, isMutationCommitted(err))
	require.Equal(t, "personal", resp.ToAccount)
	require.True(t, inst.LimitReached())
	limitedAccount, limited := inst.LimitAccount()
	require.True(t, limited)
	require.Equal(t, "personal", limitedAccount)
	resetAt := time.Time{}
	if resetAfter > 0 {
		resetAt = nowFunc().Add(resetAfter)
	}
	inst.SetLimitResetAt(resetAt)
	m.cfg.LimitAutoResume = false
	return m, repo, inst, b, advance
}

func TestResumeLimitedSessions_ParkedManualIncomingResetWaits(t *testing.T) {
	m, repo, inst, backend, _ := parkManualHandoffOnIncomingLimit(t, time.Hour)
	stateKey := stableSessionKey(repo, inst)
	_, beforeRespawns, beforePrompts := backend.snapshot()

	m.ResumeLimitedSessions()

	_, afterRespawns, afterPrompts := backend.snapshot()
	require.Equal(t, beforeRespawns, afterRespawns)
	require.Equal(t, beforePrompts, afterPrompts)
	require.Zero(t, m.limitResumeStates[stateKey].attempts,
		"the scheduler must wait for the incoming account's reset plus grace")
}

func TestResumeLimitedSessions_ParkedManualIncomingResetFiresAfterGrace(t *testing.T) {
	m, repo, inst, _, advance := parkManualHandoffOnIncomingLimit(t, time.Hour)
	stateKey := stableSessionKey(repo, inst)
	advance(time.Hour + limitResumeGrace + time.Second)

	m.ResumeLimitedSessions()

	require.Equal(t, 1, m.limitResumeStates[stateKey].attempts)
}

func TestResumeLimitedSessions_PendingSwapWithoutResetRetriesImmediately(t *testing.T) {
	m, repo, inst, _, _ := parkManualHandoffOnIncomingLimit(t, 0)
	stateKey := stableSessionKey(repo, inst)

	m.ResumeLimitedSessions()

	require.Equal(t, 1, m.limitResumeStates[stateKey].attempts,
		"a pending swap without an incoming reset must retain immediate candidate retry")
}

func TestResumeFromLimit_ParkedManualIncomingResetAllowsExplicitRetry(t *testing.T) {
	m, repo, inst, backend, _ := parkManualHandoffOnIncomingLimit(t, time.Hour)
	backend.limited = false

	require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
	_, _, prompts := backend.snapshot()
	require.Len(t, prompts, 1)
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending)
}

// #4404 review: an AUTOMATIC replacement whose readiness wait meets the
// incoming account's own wall must be charged to that account, as the manual
// arm above already is. The plain re-park it used kept limit_account on the
// outgoing identity and recorded nothing for the incoming one, so the swap
// scheduler and the create-time router both read the credential readiness had
// just proven walled as healthy.
func TestResumeLimitedSessions_AutomaticReplacementWallChargesTheIncomingAccount(t *testing.T) {
	t.Cleanup(task.SetTrustPromptTimingForTest(time.Millisecond))
	advance := withFrozenClock(t)
	m, repo, inst, base := newAutoResumeManager(t, "", true, "finish the migration", nowFunc().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "work")
	b := &accountReadinessBackend{
		limitResumeBackend: base,
		previewed:          make(chan struct{}),
		release:            make(chan struct{}),
		limited:            true,
	}
	close(b.release)
	inst.SetBackend(b)

	advance(time.Second)
	m.ResumeLimitedSessions()

	account, automatic := inst.AccountSelection()
	require.Equal(t, "work", account, "the scheduler committed the replacement identity")
	require.True(t, automatic)
	require.True(t, inst.LimitReached(), "the replacement is parked at the wall readiness met")
	limitedAccount, limited := inst.LimitAccount()
	require.True(t, limited)
	require.Equal(t, "work", limitedAccount, "the wall belongs to the incoming identity, not the outgoing one")
	var charged bool
	for _, observation := range inst.AccountLimitObservations() {
		charged = charged || (observation.Agent == "claude" && observation.Account == "work")
	}
	require.True(t, charged, "the incoming identity carries durable evidence the router and scheduler read")
	require.Equal(t, "work", persistedInstanceByTitle(t, repo, inst.Title).LimitAccount,
		"and the settled row a restart or a delete reads agrees")
}
