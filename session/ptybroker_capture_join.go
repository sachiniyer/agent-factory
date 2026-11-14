package session

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// How long a capture teardown waits on its read loop (#7302). Every teardown
// joins the loop while holding captureMu, so these bound how long a stop can keep
// a reconnect, a recovery or a shutdown waiting.
const (
	// defaultCaptureDrainGrace is how long a stopped capture may take to reach
	// its own end of stream before the teardown aborts the reader. A successful
	// local stop drains to kernel FIFO EOF so the pane's last bytes reach the
	// ring (#2300), and an abort would discard them, so the abort waits. A
	// healthy drain finishes in milliseconds.
	defaultCaptureDrainGrace = time.Second
	// defaultCaptureJoinTimeout is how long the teardown then waits for the
	// aborted reader to return before abandoning it.
	defaultCaptureJoinTimeout = 3 * time.Second
)

// abandonedCaptureReaders counts, process-wide, the read loops a capture teardown
// gave up joining. It only feeds the log line, so a host that keeps hitting the
// macOS wedge shows a climbing number rather than a string of one-offs.
var abandonedCaptureReaders atomic.Int64

// captureRun is one installed capture's read loop, as its teardown sees it.
type captureRun struct {
	// done is closed when the read loop has exited.
	done chan struct{}
	// exited and abandoned are guarded by the broker's mu, and at most one of
	// them is ever set: the loop sets exited on its way out unless the teardown
	// has already set abandoned, in which case the loop only leaves.
	exited    bool
	abandoned bool
	// abandonedAt is when the teardown gave up on the loop, for the log line
	// if the loop ever returns.
	abandonedAt time.Time
}

// joinCapture waits for a stopped capture's read loop to exit, and never waits
// forever (#7302). It runs inside a teardown that holds captureMu, after
// StopCapture.
//
//  1. It gives the stop its drain grace to end the stream on its own. A
//     successful local stop drains to kernel EOF, and aborting first would
//     discard the pane's final bytes (#2300).
//  2. It closes the reader. That is an abort, and an abort wakes even a reader
//     whose drain will never see EOF (captureReader, #7302), so this step
//     normally ends the loop.
//  3. It abandons a loop that is still running after the join timeout: one that
//     ignored even the wake. The teardown returns anyway, so captureMu is
//     released and a reconnect can bring up a fresh capture. The abandoned loop
//     is fenced (see readLoop): if it ever returns, nothing it read reaches the
//     ring, and its exit changes nothing for whatever capture replaced it.
//
// The reader is closed even when the loop exits within the grace. The broker has
// always closed it on every stop, and after the loop's own end the close is a
// formality.
func (b *ptyBroker) joinCapture(run *captureRun, r io.Closer) {
	if waitClosed(run.done, b.captureDrainGrace) {
		_ = r.Close()
		return
	}
	_ = r.Close()
	if waitClosed(run.done, b.captureJoinTimeout) {
		return
	}

	b.mu.Lock()
	if run.exited {
		// The loop finished its exit bookkeeping while the timer fired; only
		// close(done) was still to come.
		b.mu.Unlock()
		return
	}
	run.abandoned = true
	run.abandonedAt = time.Now()
	b.abandonedReaders++
	b.mu.Unlock()
	n := abandonedCaptureReaders.Add(1)
	log.WarningLog.Printf("pty broker: abandoned a capture read loop that did not return within %v of its stop, the last %v after an abort (%d abandoned in this process); its pane bytes are dropped from here on, and the next subscribe starts a fresh capture",
		b.captureDrainGrace+b.captureJoinTimeout, b.captureJoinTimeout, n)
}

// waitClosed reports whether ch closes within d.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}
