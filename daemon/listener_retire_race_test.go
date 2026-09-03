package daemon

import (
	"net/http"
	"runtime"
	"testing"
	"time"
)

// TestRetireCapturesTheGraceBeforeItsGoroutineStarts pins #3772.
//
// retire() spawned `go h.drain()`, and drain() read listenerRetireGrace INSIDE
// that goroutine — twice, once for the deadline and once for the warning. A
// drain outlives the call that spawned it; that is what a drain IS. So a test
// that shortens the grace races a drain still running from an earlier one, and
// the var's own doc comment ("so the deadline test can shorten it") is the
// invitation.
//
// Not hypothetical: it took `Test (macOS)` red on two of three runs of an
// unrelated PR, between TestDisablingAListenerRetiresIt's drain and
// TestRetiredListenerClosesAClientThatNeverDrains's swap.
//
// The pairing cannot be arranged with a channel — any signal out of the drain
// goroutine would order the write AFTER the read, leaving nothing to detect. So
// the shape two neighbouring tests produce is repeated instead: retire a
// handle, then swap the grace, with nothing ordering the two.
//
// A handle needs no listener for this. retire() and drain() both only call
// Shutdown, which returns immediately on a server that never served.
func TestRetireCapturesTheGraceBeforeItsGoroutineStarts(t *testing.T) {
	previous := listenerRetireGrace
	t.Cleanup(func() { listenerRetireGrace = previous })

	for i := 0; i < 64; i++ {
		h := &tcpListenerHandle{srv: &http.Server{}, addr: "127.0.0.1:0"}
		h.retire()
		// The write withRetireGrace performs, with nothing ordering it against
		// the drain the line above just started.
		listenerRetireGrace = time.Duration(i+1) * time.Millisecond
		// Give that drain a chance to run while this write is still the last
		// one recorded for the address.
		runtime.Gosched()
	}
}
