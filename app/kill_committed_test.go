package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/session"
)

// committedKillWarningTestError is a test stub for apiproto.MutationCommittedError
// — the marker the apiclient reconstitutes from daemon.KillSessionResponse when the
// durable kill tombstone committed but a post-commit teardown/storage follow-up
// failed. Mirrors committedArchiveWarningTestError so the same IsMutationCommitted
// classification the production transport raises is driven through
// handleInstanceKilled deterministically, without standing up a real daemon mid-kill
// (which would yield a transport error classified as a hard failure, not committed).
type committedKillWarningTestError struct{ msg string }

func (e committedKillWarningTestError) Error() string         { return e.msg }
func (committedKillWarningTestError) MutationCommitted() bool { return true }

var _ apiproto.MutationCommittedError = committedKillWarningTestError{}

// killCommittedWarning is the realistic daemon text: the durable tombstone landed
// and will be reaped automatically; only a follow-up step failed. The recovery
// screen's "The session is retained." heading would directly contradict this.
const killCommittedWarning = `kill of session 'kill-committed' could not update its record because another agent-factory process is holding the repo's instances lock; the session is already marked killed and will be reaped automatically — retry if it lingers`

// runKillFinalizer is the shared harness: a Ready row with the optimistic OpKilling
// overlay raised (killInstanceCmd's confirmation runs BeginKill before the RPC), so
// handleInstanceKilled sees the same in-flight op the production event loop sees.
func runKillFinalizer(t *testing.T, title string, err error) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	inst := newKillableInstance(t, title)
	require.NoError(t, inst.Transition(session.BeginKill()))
	require.Equal(t, session.OpKilling, inst.GetInFlightOp(), "precondition: optimistic kill op is raised")
	h.store.AddInstance(inst)
	_, _ = h.handleInstanceKilled(instanceKilledMsg{target: captureSessionActionTarget(inst, h.repoID), err: err})
	return h, inst
}

// TestInstanceKilled_CommittedWarningIsNotAFailure pins the #3252 fix: a kill whose
// durable tombstone committed but whose post-commit follow-up failed is reported as
// success-with-warning — matching handleInstanceArchived, the CLI, and the web — not
// as the hard "Cannot kill session" recovery that invites an immediate retry of a
// kill that already landed.
func TestInstanceKilled_CommittedWarningIsNotAFailure(t *testing.T) {
	require.True(t, apiclient.IsMutationCommitted(committedKillWarningTestError{killCommittedWarning}),
		"precondition: the test stub must classify as mutation-committed")

	h, inst := runKillFinalizer(t, "kill-committed", committedKillWarningTestError{killCommittedWarning})

	// No recovery screen: the older "Cannot kill session" failure path is skipped
	// for a kill that landed.
	assert.Nil(t, h.recovery, "a committed kill must not open the 'Cannot kill session' recovery")

	// The outcome is surfaced as success-with-warning via mutationOutcomeError, not
	// the hard-failure wording.
	assert.Contains(t, h.errBox.FullError(), "went through",
		"a committed kill must be framed as having landed")
	assert.Contains(t, h.errBox.FullError(), "the daemon reported a follow-up problem")
	assert.Contains(t, h.errBox.FullError(), "check the sidebar")
	assert.Contains(t, h.errBox.FullError(), "already marked killed",
		"the daemon's follow-up text (the actual evidence) must remain visible")
	assert.NotContains(t, h.errBox.FullError(), "failed to kill session",
		"a committed kill must not be mislabeled as a hard failure")
	assert.NotContains(t, h.errBox.FullError(), "could not be confirmed",
		"a committed kill is known to have landed, not an unknown outcome")
	assert.NotContains(t, h.errBox.FullError(), "The session is retained",
		"the recovery detail must not appear for a kill that landed")

	// The optimistic OpKilling fence is NOT reverted: keeping it makes the row read
	// as Deleting, so IsTearingDown fences a second kill off as "already being
	// deleted" and CanKill hides the kill key — the bug invited an immediate retry
	// of a kill that already landed.
	assert.Equal(t, session.OpKilling, inst.GetInFlightOp(),
		"a committed kill must not revert OpKilling toward a retry invitation")
	assert.True(t, inst.IsTearingDown(),
		"the row stays fenced as tearing down so handleKill reports 'already being deleted'")
	assert.False(t, inst.CanKill(),
		"the kill key is not re-advertised for a kill that already landed")

	// The row is NOT removed locally: the daemon record may persist as a tombstone,
	// and the snapshot/reconcile reaps it once the daemon's finishUserKill retry
	// completes or adopts the tombstone. Removing it ourselves would hide a row the
	// daemon still tracks.
	assert.Contains(t, collectTitles(h.store.GetInstances()), "kill-committed",
		"the row is left for the snapshot/reconcile to reap or tombstone")
}

// TestInstanceKilled_PlainRefusalStillShowsRecovery is the regression guard: a kill
// the daemon genuinely refused (nothing committed) must still flip the row back to a
// retryable Ready and show the recovery screen, exactly as before the fix. The
// committed branch must not swallow the plain-failure path.
func TestInstanceKilled_PlainRefusalStillShowsRecovery(t *testing.T) {
	h, inst := runKillFinalizer(t, "kill-refused", errors.New("The daemon refused this kill."))

	require.NotNil(t, h.recovery, "a genuinely refused kill must still show the recovery screen")
	assert.Contains(t, h.recovery.condition, "Cannot kill session")
	assert.Contains(t, h.recovery.detail, "The session is retained.")
	assert.Contains(t, h.errBox.FullError(), "failed to kill session 'kill-refused'",
		"a refused kill keeps its hard-failure wording")

	// The fence reverts so the user can retry: the row is an ordinary Ready row
	// again, advertising the kill key.
	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"a refused kill reverts the optimistic op so the user can retry")
	assert.False(t, inst.IsTearingDown())
	assert.True(t, inst.CanKill(), "a refused kill re-advertises the kill key for retry")
	assert.Contains(t, collectTitles(h.store.GetInstances()), "kill-refused",
		"the retained row stays visible for the retry")
}

// TestInstanceKilled_CommittedWarningLeavesSuccessRemovalIntact isolates the clean
// success path: with no error the row is still removed from the sidebar (the daemon
// already deleted the disk record). The committed branch must not have shifted the
// success-path removal.
func TestInstanceKilled_CommittedWarningLeavesSuccessRemovalIntact(t *testing.T) {
	h, inst := runKillFinalizer(t, "kill-clean", nil)

	assert.NotContains(t, collectTitles(h.store.GetInstances()), "kill-clean",
		"a clean kill still removes the row from the sidebar")
	_ = inst
}
