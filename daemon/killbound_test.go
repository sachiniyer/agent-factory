package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestKillWatchdogTabCount_SurvivesAGhostInstance pins the nil case, which is a
// real path rather than defensive coding: a title-based kill can resolve a
// persisted session that could not be reconstructed, and resolveActionSession
// deliberately returns a nil instance with non-nil data so the ghost branch can
// clean the record up. Reading the tab count off the instance there would panic the
// daemon on the very path that exists to tidy up after a dead session.
func TestKillWatchdogTabCount_SurvivesAGhostInstance(t *testing.T) {
	require.NotPanics(t, func() {
		require.Equal(t, 0, killWatchdogTabCount(nil, nil),
			"neither record survives — zero, and the delay floor still applies")
	})
	require.NotPanics(t, func() {
		data := &session.InstanceData{Tabs: []session.TabData{
			{Name: "agent", TmuxName: "af_x"},
			{Name: "shell-2", TmuxName: "af_x__shell"},
			// A web tab owns no tmux session, so it costs no per-tab teardown and
			// must not buy the watchdog extra time (#3023 review).
			{Name: "docs", Kind: session.TabKindWeb},
		}}
		require.Equal(t, 2, killWatchdogTabCount(nil, data),
			"only tmux-bearing tabs are torn down per-tab; a ghost names them by TmuxName")

		// Pending handles are real teardown work on the ghost path: they can still
		// own a live process in this worktree, so each distinct handle must buy the
		// same bounded-kill budget as a live tab.
		data.PendingTabCleanup = []session.TabCleanupData{{TabID: "t9", TmuxName: "af_x__stuck"}}
		require.Equal(t, 3, killWatchdogTabCount(nil, data),
			"a ghost budgets every tmux session ghostCleanup actually tears down")

		// A post-#953 record stores the agent's tmux name in BOTH data.TmuxName and
		// data.Tabs[0].TmuxName. ghostTmuxNames deduplicates them, so the budget must
		// not count the agent twice — nine real sessions budgeted as ten postpones
		// the wedge diagnostics by ~54s.
		data.TmuxName = "af_x"
		require.Equal(t, 3, killWatchdogTabCount(nil, data),
			"the agent's name appears twice in the record but is ONE tmux session")

		// A genuinely distinct session does add to it.
		data.Tabs = append(data.Tabs, session.TabData{Name: "shell-3", TmuxName: "af_x__shell3"})
		require.Equal(t, 4, killWatchdogTabCount(nil, data))
	})
}

// The delay must never drop below the value it had when tabs were capped at 9, so
// no session that was legal before this change gets a shorter watchdog than it used
// to have — and it must actually grow past that for a roster the old cap forbade.
func TestKillWatchdogDelayFor_FloorsAtTheOldValueAndGrowsBeyondIt(t *testing.T) {
	require.Equal(t, killWatchdogFloor, killWatchdogDelayFor(0))
	// GreaterOrEqual, not Equal: the property that matters is that no session which
	// was legal under the old cap gets a SHORTER watchdog than before. Nine tabs
	// compute to 908s, which clears the 900s floor on its own — asserting equality
	// pinned an arithmetic coincidence rather than the guarantee, and it failed.
	require.GreaterOrEqual(t, killWatchdogDelayFor(9), killWatchdogFloor,
		"a session legal under the old cap must never wait less than it used to")
	require.Greater(t, killWatchdogDelayFor(40), killWatchdogFloor,
		"a roster the old cap forbade needs more than the flat budget, or the watchdog cries wolf")
}
