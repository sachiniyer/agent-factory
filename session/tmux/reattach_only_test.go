package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ReattachOnly takes its caller's existence answer instead of probing again
// (#4479 over #4473). These pin that the answer reaches the monitor binding the
// same way RestoreWithResult's own has-session answer does. If it were dropped
// either way, the other of the two would fail.

// An answered probe runs the generation lookup and binds the fresh monitor to
// the generation the name resolves to, not to the outgoing one.
func TestReattachOnlyAnsweredBindsTheResolvedGeneration(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	gen := &tmuxGeneration{sessionID: "$7", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.nameGen.Store("$9 999 888")

	require.NoError(t, session.ReattachOnly(t.TempDir(), true))
	require.Positive(t, m.nameProbeCalls.Load(), "an answered probe must resolve the generation")
	session.monitorMu.Lock()
	bound := session.monitor.generation
	session.monitorMu.Unlock()
	require.NotNil(t, bound)
	require.NotSame(t, gen, bound, "a resolved different generation must not inherit the outgoing one")
	require.Equal(t, "$9", bound.sessionID)
}

// An unanswered probe skips the generation lookup, since it would pay the same
// deadline again, and carries the outgoing monitor's generation. That is the
// wedge semantics RestoreWithResult already has.
func TestReattachOnlyUnansweredCarriesTheOutgoingGeneration(t *testing.T) {
	shortTmuxTimeout(t, markTestTimeout)
	gen := &tmuxGeneration{sessionID: "$7", serverPID: "111", created: "222"}
	session, m := boundSession(t, gen)
	m.nameWedged.Store(true) // a probe that ran here would stall out

	require.NoError(t, session.ReattachOnly(t.TempDir(), false))
	require.Zero(t, m.nameProbeCalls.Load(),
		"an unanswered caller probe must not open a second full-timeout generation probe")
	session.monitorMu.Lock()
	carried := session.monitor.generation
	session.monitorMu.Unlock()
	require.Same(t, gen, carried, "an unanswered rebind carries the outgoing monitor's generation")
}
