package app

import (
	"sync"
	"testing"
)

// TestRunInteractiveLease_DropsASupersededIntent is #3028 review: the interactive
// path's pause/resume RPCs are dispatched through tea.Batch, which gives no
// ordering. With a slow pause the four calls of a lifecycle swap can execute as old
// resume, new pause, old pause, new resume — and the late OLD pause would stay in
// the daemon's map, keeping the poll and automated delivery suppressed for the rest
// of its lease after interactive mode has ended.
//
// The intents are ordered correctly on the event loop, so they carry a sequence
// number and the executor applies only the newest it has seen.
func TestRunInteractiveLease_DropsASupersededIntent(t *testing.T) {
	restore := interactiveLeaseApplied
	defer func() { interactiveLeaseApplied = restore }()
	interactiveLeaseApplied = 0

	var applied []int
	runInteractiveLease(2, func() { applied = append(applied, 2) })
	runInteractiveLease(1, func() { applied = append(applied, 1) }) // the late old intent
	runInteractiveLease(3, func() { applied = append(applied, 3) })

	if len(applied) != 2 || applied[0] != 2 || applied[1] != 3 {
		t.Fatalf("applied = %v, want [2 3] — intent 1 describes a state the user already left", applied)
	}
}

// Re-applying the same sequence number must not run twice: a retry of one intent is
// not a new intent, and running a pause twice would extend a lease its lifecycle no
// longer owns.
func TestRunInteractiveLease_IsIdempotentForOneSequence(t *testing.T) {
	restore := interactiveLeaseApplied
	defer func() { interactiveLeaseApplied = restore }()
	interactiveLeaseApplied = 0

	runs := 0
	runInteractiveLease(1, func() { runs++ })
	runInteractiveLease(1, func() { runs++ })
	if runs != 1 {
		t.Fatalf("ran %d times, want 1", runs)
	}
}

// The executor serialises: two intents must never interleave, or a resume could land
// between a pause's check and its RPC.
func TestRunInteractiveLease_SerialisesConcurrentIntents(t *testing.T) {
	restore := interactiveLeaseApplied
	defer func() { interactiveLeaseApplied = restore }()
	interactiveLeaseApplied = 0

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			runInteractiveLease(seq, func() {
				mu.Lock()
				inFlight++
				if inFlight > maxInFlight {
					maxInFlight = inFlight
				}
				mu.Unlock()
				mu.Lock()
				inFlight--
				mu.Unlock()
			})
		}(uint64(i))
	}
	wg.Wait()
	if maxInFlight > 1 {
		t.Fatalf("max concurrent intents = %d, want 1", maxInFlight)
	}
}
