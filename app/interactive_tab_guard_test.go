package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// interactiveTabGuard is the #7944 fence: a SHELL tab whose tmux session is
// provably gone must refuse interactive entry with a one-line hint naming the
// EXISTING respawn affordance — close the tab (w), open a new one (t). Before
// it, Enter put the dead pane in interactive mode and every keystroke vanished
// into a blank pane. Process tabs keep master's behaviour byte-identical: they
// are not fenced here — a stamped process exit still binds, surfacing the
// process tab's own dead state exactly as before.
func TestInteractiveTabGuard(t *testing.T) {
	deadShell := &session.Instance{Tabs: []*session.Tab{
		{Kind: session.TabKindAgent, Name: "Agent"},
		{Kind: session.TabKindShell, Name: "terminal",
			Exit: &session.TabExit{StatusKnown: true}},
	}}

	err := interactiveTabGuard(deadShell, 1)
	require.Error(t, err, "a dead shell tab must refuse interactive entry")
	assert.Contains(t, err.Error(), "is dead", "the hint says the tab is dead")
	assert.Contains(t, err.Error(), "press w to close it, then t for a new one",
		"the hint names the existing close-then-recreate affordance")
	assert.Contains(t, err.Error(), "'terminal'", "the hint names the tab")

	// The agent tab's liveness is interactiveGuard's session-level fence — the
	// tab guard must not double-fence it, and an out-of-range index is not a
	// tab at all.
	assert.NoError(t, interactiveTabGuard(deadShell, 0), "the agent tab is not this guard's case")
	assert.NoError(t, interactiveTabGuard(deadShell, -1), "out of range low is not a tab")
	assert.NoError(t, interactiveTabGuard(deadShell, 9), "out of range high is not a tab")
	assert.NoError(t, interactiveTabGuard(nil, 0), "a nil instance is not this guard's case")

	// Every other kind is left exactly as master had it — including a process
	// tab whose exit is stamped: its dead state is its own surfacing, not a
	// refused bind (R5).
	otherKinds := &session.Instance{Tabs: []*session.Tab{
		{Kind: session.TabKindAgent, Name: "Agent"},
		{Kind: session.TabKindProcess, Name: "serve",
			Exit: &session.TabExit{StatusKnown: true, Status: 0}},
		{Kind: session.TabKindWeb, Name: "preview"},
	}}
	assert.NoError(t, interactiveTabGuard(otherKinds, 1), "a dead process tab binds as on master")
	assert.NoError(t, interactiveTabGuard(otherKinds, 2), "non-shell kinds are not fenced")
}
