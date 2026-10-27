package tmux

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Bound-generation coverage for close()'s branches (#4473 review).
//
// close() gained an early return at the top: when the identity probe that sets
// the teardown mark gets no answer, the run is reported unknown WITHOUT
// spending a second and third command budget on list-panes and kill-session.
// That branch is reachable only for a BOUND monitor — markTeardownInitiated
// runs confirmedGeneration only when the current monitor carries a generation
// with an id — so every close path now forks on whether a monitor is bound.
//
// The existing branch tests (close_unknown_test.go, idempotent_close_test.go,
// kill_wedge_test.go) bind no monitor, so they all run the UNBOUND fast path,
// while production binds monitors through RestoreWithResult. These are the
// bound counterparts: each asserts the branch's own contract AND that the
// identity probe actually ran, so a future change that makes the early return
// swallow these paths fails here instead of passing on a path production does
// not take.

// boundCloseSession is a live, id-bound session whose name resolves to the
// generation the monitor polls — the shape RestoreWithResult leaves behind.
func boundCloseSession(t *testing.T) (*TmuxSession, *teardownMarkTmux, *tmuxGeneration) {
	t.Helper()
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.nameGen.Store("$5 111 222")
	return session, m, gen
}

// TestCloseBoundGenerationSurvivedKillStillErrors is the bound counterpart of
// TestClose_SessionSurvivesKill_StillErrors (idempotent_close_test.go). A
// session that refuses to die is a real failure, and binding a generation must
// not turn it into one of the new quiet paths: the kill still has to be
// attempted and the failure still has to reach the caller.
func TestCloseBoundGenerationSurvivedKillStillErrors(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m, _ := boundCloseSession(t)
	m.killFails.Store(true) // tmux refuses; alive stays true

	state, err := session.Close()

	require.Positive(t, m.nameProbeCalls.Load(),
		"this must exercise the BOUND path — an unbound close never resolves an identity")
	require.Equal(t, int32(1), m.killCalls.Load(),
		"an answered identity probe must not skip the kill: the early return is for UNANSWERED probes only")
	require.Error(t, err, "a session that survives kill-session is a real failure")
	require.Contains(t, err.Error(), "error killing tmux session")
	require.ErrorIs(t, err, ErrSessionStillAlive)
	require.Equal(t, PaneStateKnown, state,
		"tmux ANSWERED that the session is alive, so the state is known")

	// And the mark: the live name resolves to the polled generation, so the
	// refused kill discharges af's request — a later vanish is unrequested.
	require.False(t, session.teardownInitiated(),
		"the bound generation answered live after the kill was refused, so no af request describes its next death")
}

// TestCloseBoundGenerationUnansweredProbeSkipsKillAndCapture is the branch that
// has no unbound counterpart at all: it is unreachable without a bound
// monitor. A wedged server that never answers the identity probe has already
// cost a full tmuxCommandTimeout, so close() reports the run unknown instead of
// paying the same deadline twice more for the same non-answer. Skipping those
// two commands IS the branch, so counting them is the only assertion that can
// tell it from an ordinary unknown.
func TestCloseBoundGenerationUnansweredProbeSkipsKillAndCapture(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m, _ := boundCloseSession(t)
	m.nameWedged.Store(true) // the identity probe never answers

	state, err := session.Close()

	require.Equal(t, int32(1), m.nameProbeCalls.Load(),
		"the identity probe runs exactly once, and its non-answer ends the run")
	require.Equal(t, PaneStateUnknown, state,
		"a server that never answered has told close nothing about the session's fate")
	require.ErrorIs(t, err, ErrTmuxTimeout,
		"the timeout must stay reachable through errors.Is for callers that gate on it")
	require.Zero(t, m.killCalls.Load(),
		"kill-session against a server that just failed to answer pays the same deadline for the same non-answer")
	require.Zero(t, m.listPanesCalls.Load(),
		"the process-tree capture is skipped for the same reason — three deadlines, one wedged server")

	// No kill-session was ever sent, so no af request exists to attribute a
	// later disappearance to: the mark must NOT have landed.
	require.False(t, session.teardownInitiated(),
		"a teardown that was never sent cannot quiet a later vanish")
}

// TestCloseBoundGenerationHasSessionProbeTimeoutReportsUnknown is the bound
// counterpart of TestClose_HasSessionProbeTimeout_ReportsUnknown
// (close_unknown_test.go): kill-session fails FAST, so close probes has-session
// to tell "already gone" from "refused to die" — and that probe times out. The
// #1917 contract is that the caller learns the fate is UNKNOWN rather than
// receiving an ordinary kill error and deleting the workspace on it. Binding a
// generation adds a probe before all of this; it must not change the answer.
func TestCloseBoundGenerationHasSessionProbeTimeoutReportsUnknown(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m, _ := boundCloseSession(t)
	m.killFails.Store(true)
	m.probeWedged.Store(true) // the post-kill has-session probe stalls

	state, err := session.Close()

	require.Positive(t, m.nameProbeCalls.Load(), "this must exercise the BOUND path")
	require.Equal(t, int32(1), m.killCalls.Load(),
		"an answered identity probe means the kill is still attempted")
	require.Equal(t, PaneStateUnknown, state,
		"a timed-out has-session probe leaves the session's liveness unestablished; the caller must not "+
			"clean up the worktree on it (#1917)")
	require.ErrorIs(t, err, ErrTmuxTimeout,
		"the probe's timeout must reach the caller as a timeout")

	// The kill was sent and nothing answered that it failed, so af's request
	// still stands and the mark survives to attribute a later vanish.
	require.True(t, session.teardownInitiated(),
		"af asked, and no answer says the request failed — the mark must survive an unanswered probe")
}

// TestCloseBoundGenerationAlreadyDeadReturnsNil is the bound counterpart of
// TestClose_AlreadyDeadSession_ReturnsNil (idempotent_close_test.go): the
// generation answered the identity probe and then exited before kill-session
// reached it. A dead session is Close's goal, so the idempotency shortcut must
// still apply on the bound path.
func TestCloseBoundGenerationAlreadyDeadReturnsNil(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m, _ := boundCloseSession(t)
	m.killFails.Store(true) // tmux exits 1: the target is already gone
	m.alive.Store(false)    // ...and has-session confirms it

	state, err := session.Close()

	require.Positive(t, m.nameProbeCalls.Load(), "this must exercise the BOUND path")
	require.Equal(t, int32(1), m.killCalls.Load(), "Close must still attempt the kill-session")
	require.NoError(t, err,
		"a kill-session on an already-dead session is success, not an error (#967)")
	require.Equal(t, PaneStateKnown, state,
		"the has-session probe ANSWERED, so the session's fate is established")

	// af asked for this teardown and tmux confirmed the session is gone, so the
	// mark stands: the status monitor's next poll reports the disappearance at
	// INFO, which is the whole point of #4472.
	require.True(t, session.teardownInitiated())
	m.captureOK.Store(false)
	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Contains(t, infos.String(), "going silent")
	require.NotContains(t, errs.String(), "going silent")
}

// TestCloseBoundGenerationThatLostTheNameStillKillsTheName is the bound
// branch's other fork: the identity probe ANSWERS, but with a different
// generation than the monitor polls. No mark may land — kill-session targets
// the name, so it will reach the replacement, not the bound generation — but
// the teardown itself must still proceed. Reading "answered with someone else"
// as the unanswered early return would silently stop killing sessions af was
// asked to kill.
func TestCloseBoundGenerationThatLostTheNameStillKillsTheName(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m, _ := boundCloseSession(t)
	m.nameGen.Store("$9 999 888") // a replacement owns the name now

	state, err := session.Close()

	require.Equal(t, int32(1), m.killCalls.Load(),
		"a resolved-but-different generation is an answer: the kill must still be sent")
	require.Positive(t, m.listPanesCalls.Load(),
		"and the process-tree capture must still run before it")
	require.NoError(t, err)
	require.Equal(t, PaneStateKnown, state)
	require.False(t, session.teardownInitiated(),
		"the kill targets the name's owner, which is not the bound generation — its mark must not land")
}

// TestCloseUnboundMonitorSkipsTheIdentityProbe is the control for all of the
// above: it pins that the unbound path the OLD branch tests exercise really is
// a different path — no identity probe, straight to the kill. Without this, a
// regression that bound every monitor would leave the old tests passing while
// quietly changing which code they cover.
func TestCloseUnboundMonitorSkipsTheIdentityProbe(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m := newMarkedTeardownSession(t) // monitor with no generation
	m.killFails.Store(true)

	_, err := session.Close()

	require.Error(t, err)
	require.Zero(t, m.nameProbeCalls.Load(),
		"an unbound monitor has no id to resolve, so close() spends no command budget resolving one")
	require.Equal(t, int32(1), m.killCalls.Load())
	require.True(t, errors.Is(err, ErrSessionStillAlive) || err != nil)
}
