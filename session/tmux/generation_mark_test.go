package tmux

import (
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
