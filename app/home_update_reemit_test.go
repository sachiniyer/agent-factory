package app

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// TestReemitReordersCoalescedKeys guards the re-emit race in
// handleMenuHighlighting against regressing.
//
// A mapped keypress is split into two event-loop passes: pass-1 paints the
// menu highlight and re-emits the key through bubbletea's command pipeline;
// pass-2 runs the action (which may open a modal). The re-emit traverses the
// command pipeline (goroutine spawns + unbuffered-channel hops) to reach
// p.msgs, but the next key from the terminal/test reaches p.msgs in a single
// send, so it can win the event loop's select while the first key's re-emit is
// still in flight.
//
// "/" opens the search overlay on pass-2; "p" is the first search-query
// character. Sent back-to-back, "p" parks on p.msgs before "/"'s re-emit
// resolves. The guard must defer "p" until after "/" opens search, so "p"
// becomes the query (matching only "apple") rather than being swallowed as an
// unmapped no-op in stateDefault (which would leave the query empty and match
// all instances). Uses teatest — the real tea.Program event loop — so the race
// is the same one production sees.
func TestReemitReordersCoalescedKeys(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("apple")  // title "apple" contains "p"
	eh.addStartedInstance("cherry") // title "cherry" does not
	eh.start()

	// Let Init's 100ms previewTick settle.
	time.Sleep(300 * time.Millisecond)

	// Send "/" (search opener) then "p" (first query char) as fast as
	// possible. Both go through p.Send -> p.msgs. The second is delivered
	// before the first's re-emit resolves through the command pipeline.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})

	// Wait for search state to be reached (re-emit pipeline resolves and
	// opens the overlay). The handler closes done after fn runs, so fn
	// must not close it (see home_update.go:158-163).
	var searchReached bool
	eh.waitUntil(5*time.Second, "search opens", func() bool {
		done := make(chan struct{})
		eh.tm.Send(runOnEventLoopMsg{fn: func(h *home) {
			searchReached = h.state == stateSearch
		}, done: done})
		select {
		case <-done:
		case <-time.After(time.Second):
			return false
		}
		return searchReached
	})

	// Inspect the search overlay's result count.
	var resultCount int
	eh.query(func(h *home) {
		if h.searchOverlay != nil {
			resultCount = len(h.searchOverlay.ResultInstances())
		}
	})

	// "p" typed into the open search -> query "p" -> only "apple" matches.
	// If the re-emit race regresses, "p" is swallowed in stateDefault before
	// search opens and the query stays empty, matching all instances.
	require.Equal(t, 1, resultCount,
		"re-emit race swallowed 'p' before search opened; expected 1 result (apple only), got 2 (empty query matches all)")
}
