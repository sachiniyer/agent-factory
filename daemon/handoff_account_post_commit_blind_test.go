package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// wedgedCommittedSwapFixture commits a manual account swap and then simulates the
// post-commit blind-teardown wedge (ErrAccountSwapAgentTeardownBlind): the swap's
// identity checkpoint has landed, but the post-commit respawn's cleanup encountered
// a blind agent teardown. The daemon handles that error by calling
// instance.MarkStartupStateUnknown() while preserving the committed pending swap —
// producing a row that is StartupStateUnknown, not Started, with a surviving
// committed swap whose current == to. This helper returns the fixture in that
// wedged state, with delivery restored so a retry can finish the transaction.
//
// alive selects the probe arm the recovery will take: false forces probeAbsent
// (the agent pane is gone, the most common post-blind state); true exercises the
// live-pane repair arm.
func wedgedCommittedSwapFixture(t *testing.T, alive bool) (*Manager, string, *session.Instance, *limitResumeBackend) {
	t.Helper()
	m, repo, inst, backend := newAutoResumeManager(t, "", alive, "continue", time.Now().Add(time.Hour))
	configureLimitAccountCandidate(t, m, "personal")
	inst.Account = "work"
	inst.ClearLimitReached()
	prepareHandoffTargetPreflight(t, inst)

	// Commit the manual swap, stranding it at delivery so the pending swap survives.
	backend.sendPromptErr = errors.New("delivery interrupted")
	_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.ErrorContains(t, err, "delivery interrupted")
	require.True(t, isMutationCommitted(err), "the handoff must have committed the identity checkpoint")
	_, _, pending := inst.PendingAccountSwap()
	require.True(t, pending, "the committed swap must survive the failed delivery")
	require.False(t, inst.StartupStateUnknown(), "a healthy committed swap is not startup-unknown yet")
	require.NotNil(t, committedAccountSwap(inst), "committedAccountSwap must recognize the committed transaction")

	// The blind-teardown error path (limit.go:696-704): preserve the committed swap
	// while marking the row inert.
	inst.MarkStartupStateUnknown()
	require.True(t, inst.StartupStateUnknown(), "the wedge must set StartupStateUnknown")
	require.False(t, inst.Started(), "the wedge must clear Started")
	require.NotNil(t, committedAccountSwap(inst), "the committed swap must survive the wedge")

	// Restore delivery so a retry can finish the transaction.
	backend.mu.Lock()
	backend.sendPromptErr = nil
	backend.mu.Unlock()
	return m, repo, inst, backend
}

// TestResumeFromLimit_CommittedSwapBlindWedgeRecovers is the core regression test
// for the post-commit blind-teardown wedge. Before the fix,
// accountSwapResumeEligible gated on StartupStateUnknown first, so every
// ResumeFromLimit retry returned "session %q is not blocked on a usage limit"
// before the alreadySet recovery branches of resumeFromLimitLockedOutcome could
// run. A committed swap that wedged on a blind teardown was unrecoverable via
// ResumeFromLimit (all client surfaces). After the fix, the committed-swap
// check short-circuits the gate, the alreadySet branches re-establish the
// runtime, and the success path resolves the startup-unknown fence.
func TestResumeFromLimit_CommittedSwapBlindWedgeRecovers(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, false)

	outcome, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err, "a committed-swap blind wedge must be recoverable via ResumeFromLimit, "+
		"not refused with 'not blocked on a usage limit'")
	require.Equal(t, resumePerformed, outcome)

	// The transaction completed: the pending swap retired and the prompt landed.
	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending, "the recovered committed swap must retire its pending marker")
	recoverCalls, _, prompts := backend.snapshot()
	require.Zero(t, recoverCalls, "the committed-swap recovery must not route through Recover")
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], `claude account "personal"`,
		"the recovery must deliver the committed swap's notice")

	// The success path resolved the startup-unknown fence the wedge set: the row
	// is no longer inert and is started, so every later recovery door works.
	require.False(t, inst.StartupStateUnknown(), "successful recovery must clear StartupStateUnknown")
	require.True(t, inst.Started(), "successful recovery must restore Started")
	require.False(t, inst.LimitReached(), "successful recovery must clear the limit block")

	// The persisted record agrees: a restart must not reload an inert row.
	saved := persistedInstanceByTitle(t, repo, inst.Title)
	require.False(t, saved.StartupStateUnknown, "the durable record must not carry the stale fence")
	require.Nil(t, saved.PendingAccountSwap, "the durable record must retire the committed swap")
}

// TestResumeFromLimit_CommittedSwapBlindWedgeLivePaneRepairRecovers exercises
// the probeAlive arm of the alreadySet recovery: the replacement pane is live but
// its pane set is incomplete (ReplacementPanesStarted=false, as the real
// blind-teardown error path leaves it), so the recovery must repair the pane set
// and respawn before delivering the notice.
func TestResumeFromLimit_CommittedSwapBlindWedgeLivePaneRepairRecovers(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, true)

	outcome, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)

	_, _, pendingAfter := inst.PendingAccountSwap()
	require.False(t, pendingAfter, "the live-pane repair must finish the committed swap")
	_, respawnCalls, prompts := backend.snapshot()
	require.True(t, respawnCalls >= 1, "the incomplete pane set must force at least one respawn")
	require.Len(t, prompts, 1)
	require.Contains(t, prompts[0], `claude account "personal"`)
	require.False(t, inst.StartupStateUnknown())
	require.True(t, inst.Started())
}

// TestHandoffSession_CommittedSwapBlindWedgeRecovers verifies the HandoffSession
// corridor recovers the wedge too. The bug report identified HandoffSession as the
// "only surviving recovery door," but it shared the same ValidateRuntimeAction /
// BeginManualAccountSwap StartupStateUnknown barriers; the fix lifts those for a
// committed-swap retry naming the committed account.
func TestHandoffSession_CommittedSwapBlindWedgeRecovers(t *testing.T) {
	m, repo, inst, _ := wedgedCommittedSwapFixture(t, false)

	resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
	require.NoError(t, err, "HandoffSession naming the committed account must recover the wedge")
	require.True(t, resp.OK)
	require.Equal(t, "personal", resp.ToAccount)

	_, _, pending := inst.PendingAccountSwap()
	require.False(t, pending, "the recovered committed swap must retire its pending marker")
	require.False(t, inst.StartupStateUnknown(), "successful recovery must clear StartupStateUnknown")
	require.True(t, inst.Started(), "successful recovery must restore Started")
}

// TestCommittedSwapBlindWedgeSchedulerStaysSuppressed verifies the auto-resume
// scheduler does NOT auto-recover the wedged row. The fix deliberately bypasses
// the StartupStateUnknown gate only at the explicit ResumeFromLimit / HandoffSession
// call sites — not inside the shared accountSwapResumeEligible predicate — so the
// "suppress automatic retries until inspection" intent of the wedge is preserved.
// The row remains inert until the operator explicitly retries.
func TestCommittedSwapBlindWedgeSchedulerStaysSuppressed(t *testing.T) {
	m, repo, inst, backend := wedgedCommittedSwapFixture(t, false)
	_ = repo

	// The scheduler must skip the wedged row: it is not Started and not
	// accountSwapScheduledResumeEligible (StartupStateUnknown gates the shared
	// predicate, which the scheduler uses and the fix does not relax).
	m.ResumeLimitedSessions()

	_, _, prompts := backend.snapshot()
	require.Empty(t, prompts, "the auto-scheduler must not auto-recover a blind wedge; "+
		"only the explicit RPC is the inspection the marker waits for")
	_, _, pending := inst.PendingAccountSwap()
	require.True(t, pending, "the scheduler must leave the committed swap pending")
	require.True(t, inst.StartupStateUnknown(), "the scheduler must keep the row inert")
}

// TestResumeFromLimit_NonCommittedStartupUnknownStillRefused verifies the fix
// does not open the door for a NON-committed-swap startup-unknown row. A plain
// startup-unknown session with no committed swap must still be refused by
// ResumeFromLimit, because the committed-swap bypass only fires when
// committedAccountSwap detects a surviving transaction. This guards against
// over-broadening the recovery corridor.
func TestResumeFromLimit_NonCommittedStartupUnknownStillRefused(t *testing.T) {
	m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
	inst.ClearLimitReached()
	inst.MarkStartupStateUnknown()
	require.True(t, inst.StartupStateUnknown())
	require.Nil(t, committedAccountSwap(inst), "a row with no committed swap must not be eligible for the bypass")

	_, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not blocked on a usage limit")
	require.True(t, inst.StartupStateUnknown(), "a non-committed startup-unknown row must stay inert")
}
