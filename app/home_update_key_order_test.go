package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// Keys typed right behind a key that opens a modal belong to that modal (#4828).
// A mapped key used to take two Update passes — paint its menu highlight, then
// re-emit itself and act on the replay — and the next physical key could beat
// the replay onto the event loop and run in the pre-action state. The action
// now runs in the same Update as the highlight, so these pin the user-visible
// property rather than any mechanism.

// TestKeyAfterOpenerLandsInTheOpenedModal: "m" opens the task manager and "n"
// right behind it enters task-create mode there. Run in the pre-action state,
// "n" would open the new-session form instead.
func TestKeyAfterOpenerLandsInTheOpenedModal(t *testing.T) {
	h := newTestHome(t)

	_, _ = h.handleKeyPress(runeKey('m'))
	require.Equal(t, stateTasks, h.state, "m opens the task manager in its own keypress")
	_, _ = h.handleKeyPress(runeKey('n'))
	require.Equal(t, stateTasks, h.state, "n must not open the new-session form")
	require.True(t, h.automations.TaskPane().IsCreating(), "n reaches the task manager's create mode")
}

// TestCoalescedKeysLandInTheOpenedModalInOrder drives the same property through
// the real tea.Program event loop (teatest), where the race lived: "/" opens
// search, and "p", "q" sent back-to-back behind it must become the query "pq".
//
// Fixtures are chosen so the result COUNT uniquely identifies the query:
//
//	"pq" -> 1 (only "pqrs" has p then q); "p" -> 2 ("pqrs"+"aple"); "q" -> 2
//	("pqrs"+"quux"); "qp" -> 0; "" -> 4.
func TestCoalescedKeysLandInTheOpenedModalInOrder(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("pqrs")   // fuzzy "pq" matches (p then q)
	eh.addStartedInstance("aple")   // has p, no q — matches "p" but not "pq"
	eh.addStartedInstance("quux")   // has q, no p — matches "q" but not "pq"
	eh.addStartedInstance("cherry") // neither p nor q — only matches ""
	eh.start()

	// Let Init's 100ms previewTick settle.
	time.Sleep(300 * time.Millisecond)

	// Send "/" (search opener) then "p", "q" (first query chars) as fast as
	// possible, all through p.Send -> p.msgs.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	// Poll the overlay's result count on the tea goroutine until it settles.
	// The runOnEventLoopMsg handler closes done after fn runs, so fn must not
	// close it (see the runOnEventLoopMsg case in Update).
	var resultCount int
	eh.waitUntil(5*time.Second, "search query becomes \"pq\" in order (1 result)", func() bool {
		done := make(chan struct{})
		eh.tm.Send(runOnEventLoopMsg{fn: func(h *home) {
			if h.state == stateSearch && h.searchOverlay != nil {
				resultCount = len(h.searchOverlay.ResultInstances())
			}
		}, done: done})
		select {
		case <-done:
		case <-time.After(time.Second):
			return false
		}
		return resultCount == 1
	})

	// Query "pq" -> only "pqrs" matches (fuzzy: p then q). count==1 rules out
	// every regression: "qp" (0), a swallowed char leaving "p"/"q" (2), or an
	// empty query (4).
	require.Equal(t, 1, resultCount,
		"racing query chars landed out of order or were swallowed; expected query \"pq\" (1 result: pqrs), got %d", resultCount)
}
