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
// "/" opens the search overlay on pass-2; "p" and "q" are the first two
// search-query characters. Sent "/" then "p" then "q" back-to-back, both query
// chars park on p.msgs before "/"'s re-emit resolves. The guard buffers racing
// keys in arrival order and drains them through the normal path once "/"
// opens search (synchronously, in the same Update as the replay — not through
// per-key goroutines), so the query becomes "pq" and matches only "pqrs". The per-key re-emit the guard replaced
// had no ordering guarantee — two goroutines could deliver q before p, giving
// "qp" (which matches nothing) — so this asserts ORDER among two distinct
// racing keys, not merely that a single key survives. Uses teatest — the real
// tea.Program event loop — so the race is the same one production sees.
//
// Fixtures are chosen so the result COUNT uniquely identifies the query:
//
//	"pq" -> 1 (only "pqrs" has p then q); "p" -> 2 ("pqrs"+"aple"); "q" -> 2
//	("pqrs"+"quux"); "qp" -> 0; "" -> 4. So count==1 means both chars landed in
//
// order, untainted by a timing race between the search-open and the drain.
func TestReemitReordersCoalescedKeys(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstance("pqrs")   // fuzzy "pq" matches (p then q)
	eh.addStartedInstance("aple")   // has p, no q — matches "p" but not "pq"
	eh.addStartedInstance("quux")   // has q, no p — matches "q" but not "pq"
	eh.addStartedInstance("cherry") // neither p nor q — only matches ""
	eh.start()

	// Let Init's 100ms previewTick settle.
	time.Sleep(300 * time.Millisecond)

	// Send "/" (search opener) then "p", "q" (first query chars) as fast as
	// possible. All go through p.Send -> p.msgs. The query chars are
	// delivered before "/"'s re-emit resolves through the command pipeline.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	// Poll the overlay's result count on the tea goroutine until it settles.
	// Search opens on "/"'s pass-2 and the buffered "p"/"q" drain in that same
	// Update, but polling result count rather than state==stateSearch keeps the
	// assertion about ORDER, not about the overlay existing; count==1 is the proof
	// both chars landed — in order — since only "pq" yields 1. The
	// runOnEventLoopMsg handler closes done after fn runs, so fn must not
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

	// "p","q" drained in order into the open search -> query "pq" -> only
	// "pqrs" matches (fuzzy: p then q). count==1 rules out every regression:
	// "qp" (0), a swallowed char leaving "p"/"q" (2), or an empty query (4).
	require.Equal(t, 1, resultCount,
		"racing query chars landed out of order or were swallowed; expected query \"pq\" (1 result: pqrs), got %d", resultCount)
}
