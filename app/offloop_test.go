package app

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// blockingSeam is the fake for a blocking seam that a key action moved off the
// event loop (#4848). enter blocks until the test releases it, so an Update that
// still reached the seam synchronously would hang, and updateOffLoop turns that
// hang into a failure instead of a stuck test.
type blockingSeam struct {
	calls   atomic.Int32
	release chan struct{}
	once    sync.Once
}

func newBlockingSeam(t *testing.T) *blockingSeam {
	t.Helper()
	b := &blockingSeam{release: make(chan struct{})}
	// Never strand a cmd goroutine parked in enter past the test.
	t.Cleanup(b.unblock)
	return b
}

// enter records the call and blocks until unblock. Every fake seam calls it
// first.
func (b *blockingSeam) enter() {
	b.calls.Add(1)
	<-b.release
}

// unblock lets every current and future enter return.
func (b *blockingSeam) unblock() {
	b.once.Do(func() { close(b.release) })
}

// offLoopDeadline is how long Update may take before the harness calls it
// blocked. Update does in-memory work only, so this is generous; it trips only
// when Update is parked inside the seam, which never returns until unblock.
const offLoopDeadline = 5 * time.Second

// updateOffLoop delivers msg through h.Update — the event-loop entry point —
// and fails the test if Update entered the blocking seam: either it did not
// return (it is parked in enter), or it returned having called it. It returns
// the cmd Update produced, which is where the blocking work must now live.
func updateOffLoop(t *testing.T, h *home, msg tea.Msg, seam *blockingSeam) tea.Cmd {
	t.Helper()
	done := make(chan tea.Cmd, 1)
	go func() {
		_, cmd := h.Update(msg)
		done <- cmd
	}()
	select {
	case cmd := <-done:
		if n := seam.calls.Load(); n != 0 {
			t.Fatalf("Update called the blocking seam %d time(s) on the event loop; it must only run inside the returned tea.Cmd (#4848)", n)
		}
		return cmd
	case <-time.After(offLoopDeadline):
		t.Fatalf("Update did not return within %s: it is blocked inside the seam on the event loop (#4848)", offLoopDeadline)
		return nil
	}
}
