package daemon

import (
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A watch script can start a process that leaves the leader's process group
// (setsid, setpgid, Python's start_new_session). The post-exit group kill does not
// reach it, it keeps the inherited pipe write end open, and the reader never sees
// EOF. awaitWatchReaders used to bound its wait only when the kill had been
// SKIPPED, so a normal (groupKilled) run wedged stop/reload/restart on the
// escaped process for as long as it lived (#4321).
func TestAwaitWatchReadersBoundsAnEscapedWriterAfterGroupKill(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() // the escaped process: the write end stays open for the whole test

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.Close()
		_, _ = io.Copy(io.Discard, r) // blocks until EOF or the read end is closed
	}()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitWatchReaders("t4321", true, 100*time.Millisecond, []*os.File{r}, nil, done)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitWatchReaders blocked on a reader held open by a process that escaped the group kill")
	}
}

// A reader parked on usage-limit backpressure is deliberately not reading (the
// pipe is the lossless buffer, #4226), so the bound must NOT close its pipe while
// the park holds: the wait continues until the park ends.
func TestAwaitWatchReadersKeepsWaitingForAParkedReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()

	done := make(chan struct{})
	var parked atomic.Bool
	parked.Store(true)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitWatchReaders("t4321p", true, 50*time.Millisecond, []*os.File{r}, parked.Load, done)
	}()
	select {
	case <-returned:
		t.Fatal("awaitWatchReaders gave up on a reader that is parked on backpressure")
	case <-time.After(400 * time.Millisecond):
	}
	if _, err := r.Stat(); err != nil {
		t.Fatalf("the parked reader's pipe was closed under it: %v", err)
	}
	parked.Store(false)
	close(done) // the reader finishes once the park ends
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitWatchReaders did not return after the park ended and the reader finished")
	}
}
