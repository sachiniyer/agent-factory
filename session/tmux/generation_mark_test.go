package tmux

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Generation binding (#4473 review): the teardown mark lives on a
// tmuxGeneration — (session id, server pid, created) — not on the monitor
// object, because a fresh monitor does not necessarily mean a fresh session.
// These tests pin the four consequences: same-generation monitors share the
// mark, a reissued id is the bound generation's death, a resolved id is
// affirmative liveness, and a live name does not clear a bound mark.

// boundSession returns a session whose current monitor is bound to gen.
func boundSession(t *testing.T, gen *tmuxGeneration) (*TmuxSession, *teardownMarkTmux) {
	t.Helper()
	m := &teardownMarkTmux{}
	m.alive.Store(true)
	m.captureOK.Store(true)
	session := newTmuxSession(toTmuxName("generation-mark", ""), "claude", NewMockPtyFactory(t), m.exec())
	session.monitor = &statusMonitor{generation: gen}
	return session, m
}

func monitorMark(t *testing.T, session *TmuxSession) bool {
	t.Helper()
	session.monitorMu.Lock()
	defer session.monitorMu.Unlock()
	return session.monitor != nil && session.monitor.generation != nil &&
		session.monitor.generation.teardownInitiated
}

// TestSameGenerationRebindSharesTeardownMark is the first finding: a pure
// rebind that confirms the SAME live generation installs a fresh monitor, and
// a close() marking the current monitor must still reach a poll in flight on
// the swapped-out one — only possible if both monitors share the generation's
// attribution object.
func TestSameGenerationRebindSharesTeardownMark(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, _ := boundSession(t, gen)
	outgoing := session.monitor

	// The rebind resolves the same generation — display-message answered with
	// the identical (id, pid, created) tuple.
	incoming := newStatusMonitor()
	session.setMonitor(incoming, &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}, true)
	require.Same(t, gen, incoming.generation,
		"a rebind confirming the same live generation must share its attribution object")

	// close() marks the CURRENT monitor's generation; the swapped-out
	// monitor's in-flight poll reads the same mark through the shared object.
	session.markTeardownInitiated()
	require.True(t, outgoing.generation.teardownInitiated,
		"a close during an in-flight poll on the old monitor must still classify the shared generation's teardown as af-initiated")
}

// TestBoundMonitorReadsIdReuseAsGenerationGone is the server-lifetime finding:
// tearing down the last session exits the tmux server, and the next server
// reissues $0 — so has-session answering at the id proves nothing. The
// identity probe asks the id's owner for pid+created; a DIFFERENT server pid
// behind the same id is the bound generation's death, not its survival.
func TestBoundMonitorReadsIdReuseAsGenerationGone(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$0", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	session.markTeardownInitiated()

	// A replacement server reissued $0 — same id, same-second creation stamp
	// (measured: two servers both minted $0 with identical session_created),
	// different server pid.
	m.idGen.Store("999 222")

	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.True(t, session.monitor.dead,
		"the bound generation is gone once its id resolves to a different server lifetime")
	require.Contains(t, infos.String(), "going silent",
		"af closed this generation itself; the reuse of its id does not change that attribution")
	require.NotContains(t, errs.String(), "going silent")
}

// TestBoundMonitorUnmarkedIdReuseStaysError is the finding's other half: the
// same reissue against an UNMARKED bound monitor is an unrequested vanish —
// the case ERROR exists for.
func TestBoundMonitorUnmarkedIdReuseStaysError(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$0", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.idGen.Store("999 222")

	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Contains(t, errs.String(), "going silent",
		"a session that vanished without af asking must not be quieted by id reuse")
	require.NotContains(t, infos.String(), "going silent")
}

// TestAnsweredResolutionDoesNotCarryStaleMark is the third finding: when the
// has-session probe timed out but the display-message resolution DID answer,
// the confirmed-live session is not the one af closed — carrying the old
// settled mark onto it would misclassify its death at INFO.
func TestAnsweredResolutionDoesNotCarryStaleMark(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	session, m := newMarkedTeardownSession(t)
	_, err := session.Close()
	require.NoError(t, err)
	require.True(t, session.TeardownInitiated())

	// The existence probe wedges — but the bind probe recovers and answers
	// with a DIFFERENT generation behind the name.
	m.probeWedged.Store(true)
	m.nameGen.Store("$9 999 888")

	_, err = session.RestoreWithResult(t.TempDir())
	require.NoError(t, err)
	require.False(t, session.TeardownInitiated(),
		"a resolved generation is affirmative liveness: the confirmed-live session is not the one af closed")

	// And the fresh monitor's poll of the confirmed generation's death stays
	// at ERROR — it was never af's teardown.
	m.captureOK.Store(false)
	m.idGen.Store("777 666") // $9 now owned by yet another server — $9's owner is gone
	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Contains(t, errs.String(), "going silent")
	require.NotContains(t, infos.String(), "going silent")
}

// TestSameGenerationResolutionRetiresSettledMark is the finding's precise
// carve-out: the resolved generation matching the OUTGOING monitor's
// generation is affirmative liveness for THAT generation — a settled mark on
// it means the teardown demonstrably did not take, so the resolution retires
// it. An in-flight (unsettled) mark survives, because the close may still
// land.
func TestSameGenerationResolutionRetiresSettledMark(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)

	// A settled mark: af asked, close() returned, and the generation is still
	// here to answer — the teardown did not take.
	session.markTeardownInitiated()
	session.monitorMu.Lock()
	gen.teardownSettledAt = time.Now()
	session.monitorMu.Unlock()

	m.probeWedged.Store(true)
	m.nameGen.Store("$5 111 222") // the SAME generation answers the bind probe

	_, err := session.RestoreWithResult(t.TempDir())
	require.NoError(t, err)
	require.Same(t, gen, session.monitor.generation,
		"the same confirmed generation shares its attribution object")
	require.False(t, session.TeardownInitiated(),
		"the settled mark retires: the generation answered live after the close returned")

	// And the unsettled counterpart: an in-flight teardown keeps its mark.
	gen2 := &tmuxGeneration{sessionID: "$7", serverPID: "111", created: "222"}
	session2, m2 := boundSession(t, gen2)
	session2.markTeardownInitiated() // no settle — close() still running

	m2.probeWedged.Store(true)
	m2.nameGen.Store("$7 111 222")
	_, err = session2.RestoreWithResult(t.TempDir())
	require.NoError(t, err)
	require.True(t, monitorMark(t, session2),
		"an in-flight teardown's mark survives the same-generation rebind")
}

// TestStartKeepsBoundMarkWhenNameIsRebound is the fourth finding: Start's
// exists-gate clears the teardown mark because the name is live — but a bound
// monitor's generation is not the name's owner. Clearing it would report af's
// own teardown of the old generation as an unrequested vanish.
func TestStartKeepsBoundMarkWhenNameIsRebound(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	session.markTeardownInitiated()

	// The name is live — but the session answering there is a DIFFERENT
	// generation than the one the monitor polls.
	m.nameGen.Store("$9 999 888")
	require.ErrorIs(t, session.Start(t.TempDir()), ErrSessionNotStarted)
	require.True(t, session.TeardownInitiated(),
		"a live name owned by another generation does not discharge af's teardown of the bound one")

	// The bound generation then reports its death — af's own teardown, at
	// INFO — rather than being read as an unrequested vanish.
	m.captureOK.Store(false)
	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Contains(t, infos.String(), "going silent")
	require.NotContains(t, errs.String(), "going silent")
}

// TestStartClearsBoundMarkWhenGenerationSurvives is the finding's boundary:
// the clear is wrong only when the name's owner differs. When the live
// session IS the bound generation, its survival discharges the mark as
// before.
func TestStartClearsBoundMarkWhenGenerationSurvives(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	session.markTeardownInitiated()

	m.nameGen.Store("$5 111 222") // the bound generation itself is live
	require.ErrorIs(t, session.Start(t.TempDir()), ErrSessionNotStarted)
	require.False(t, session.TeardownInitiated(),
		"the bound generation answering live proves the teardown did not take")
}

// TestCloseDoesNotMarkAGenerationThatLostTheName is the next finding: the
// bound session exited on its own and a replacement already owns the name, so
// close()'s name-targeted kill-session will hit the replacement. Marking the
// bound generation anyway would read its unrequested death as the teardown af
// just asked for.
func TestCloseDoesNotMarkAGenerationThatLostTheName(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.nameGen.Store("$9 999 888") // the name now answers for another generation

	_, err := session.Close()
	require.NoError(t, err)
	require.False(t, session.TeardownInitiated(),
		"the kill targets the name's owner, which is not the bound generation — its mark must not land")

	// The bound generation's earlier, unrequested death then reports as the
	// anomaly it was — not as the request close() just made.
	m.captureOK.Store(false)
	infos := captureInfoLog(t)
	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Contains(t, errs.String(), "going silent")
	require.NotContains(t, infos.String(), "going silent")
}

// TestCloseMarksTheGenerationTheNameStillResolvesTo is the counterpart: while
// the name answers for the polled generation, close() marks it exactly as
// before — the resolution only vetoes a mark that could not describe the
// session kill-session will reach.
func TestCloseMarksTheGenerationTheNameStillResolvesTo(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.nameGen.Store("$5 111 222")

	_, err := session.Close()
	require.NoError(t, err)
	require.True(t, session.TeardownInitiated())
}

// TestWedgedIdentityProbeSpendsOneBudget is the double-timeout finding: a
// bound monitor whose identity probe consumes the whole command deadline must
// end the poll on that error — not follow it with a capture-pane that spends
// a second full budget, which doubles the sequential status loop's worst case
// per wedged session.
func TestWedgedIdentityProbeSpendsOneBudget(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.idWedged.Store(true)

	errs := captureErrorLog(t)
	session.HasUpdated()
	require.Equal(t, int32(1), m.idProbeCalls.Load())
	require.Zero(t, m.captureCalls.Load(),
		"an identity probe that consumed the deadline must not be followed by a capture spending a second one")
	require.False(t, session.monitor.dead, "an unanswered probe is unknown — never a latchable death")
	require.Contains(t, errs.String(), "error capturing pane content")
}

// TestTransientIdentityProbeFailureStaysRetryable is the misclassification
// finding: a probe error tmux never answered — an exec-level failure here —
// is not evidence the generation is gone. The poll must surface it as an
// ordinary transient and keep the monitor retryable, which is what the
// recovery half of this test observes.
func TestTransientIdentityProbeFailureStaysRetryable(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.idErr.Store(errors.New("fork/exec tmux: resource temporarily unavailable"))
	m.idErrOn.Store(true)

	errs := captureErrorLog(t)
	updated, _, _ := session.HasUpdated()
	require.False(t, updated)
	require.False(t, session.monitor.dead,
		"a probe that never produced an answer proves nothing about the generation's fate")
	require.NotContains(t, errs.String(), "going silent")

	m.idErrOn.Store(false)
	m.idGen.Store("111 222") // the bound generation answers again
	updated, _, _ = session.HasUpdated()
	require.True(t, updated, "the retryable poll recovers once the probe can run again")
}

// TestUnclassifiedProbeErrorCorroboratesAgainstTheIdList is the finding's
// middle case: tmux answered the probe with exit 1 but a diagnostic that is
// neither "can't find session" nor a server-death report. Only the server's
// own session-id listing settles it — the id still registered means the
// failure was the probe's, not the generation's.
func TestUnclassifiedProbeErrorCorroboratesAgainstTheIdList(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.idErr.Store(exitErrorWithStderr(t, 1, "no current target"))
	m.idErrOn.Store(true)
	m.idList.Store("$5\n") // the server still knows the id

	session.HasUpdated()
	require.False(t, session.monitor.dead,
		"an unclassified probe error while the id remains registered is a retryable failure, not a death")
}

// TestUnclassifiedProbeErrorIsDeterminateWhenTheIdIsGone is the same failure
// with the corroboration coming back empty: the server answers listings but
// no longer holds the id, so the bound generation is proved gone.
func TestUnclassifiedProbeErrorIsDeterminateWhenTheIdIsGone(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.idErr.Store(exitErrorWithStderr(t, 1, "no current target"))
	m.idErrOn.Store(true)

	errs := captureErrorLog(t)
	session.HasUpdated()
	require.True(t, session.monitor.dead,
		"the server answering with the id unregistered is a proved death")
	require.Contains(t, errs.String(), "going silent")
}

// TestBoundGenerationDeathKeepsDialogContext is the lost-context finding: a
// bound monitor that learns of the generation's death from the identity probe
// must build the error through sessionGoneError like the capture path does —
// a dialog af answered moments before the pane vanished is the difference
// between "the agent exited" and "the agent exited because we told it to".
func TestBoundGenerationDeathKeepsDialogContext(t *testing.T) {
	gen := &tmuxGeneration{sessionID: "$5", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.captureOK.Store(false)
	session.noteDialogKeystroke(codexDirectoryTrustDialogName, codexDirectoryTrustAffirmative, "Enter")

	errs := captureErrorLog(t)
	session.HasUpdated() // idGen unset: the id resolves to nothing
	require.True(t, session.monitor.dead)
	require.Contains(t, errs.String(),
		"after af answered its Codex directory-trust dialog by sending Enter",
		"a generation-mismatch death must carry the same dialog context the capture path reports")
}
