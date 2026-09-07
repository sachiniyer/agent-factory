package tmux

import (
	"testing"
	"time"
)

// awaitCloseReap installs a per-session completion event before Close. Completion
// means both signalling and logging finished; process death alone proves neither.
func awaitCloseReap(t *testing.T, session *TmuxSession) func() {
	t.Helper()
	done := make(chan struct{})
	session.closeReapDone = func() { close(done) }
	// Include list-panes, kill-session and a possible has-session probe, the
	// shrunk grace/TERM waits, and KillEscalating's final one-second KILL wait.
	// This is only a failure bound: successful tests return on the event.
	budget := reapTestSlack * (3*tmuxCommandTimeout + reapGraceWait + reapTermWait + time.Second)
	return func() {
		t.Helper()
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			t.Fatalf("Close's asynchronous reap did not finish within %s", budget)
		}
	}
}
