package api

import (
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// The shield must hold SIGHUP non-terminating for exactly the wrapped call and
// restore the caller's own disposition afterward — an inherited SIG_IGN
// (nohup) is handed back, not flattened to SIG_DFL.
func TestIgnoreTeardownHangup(t *testing.T) {
	signal.Reset(syscall.SIGHUP)
	undo := ignoreTeardownHangup()
	// A SIGHUP delivered mid-call must not terminate the process — a
	// regression that loses the shield kills this test binary outright, which
	// is the fail-first signal this test exists to provide.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("self-SIGHUP: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	undo()
	if signal.Ignored(syscall.SIGHUP) {
		t.Fatal("SIGHUP was left ignored after the call; the default disposition must be restored")
	}
}

func TestIgnoreTeardownHangupPreservesInheritedIgnore(t *testing.T) {
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	undo := ignoreTeardownHangup()
	undo()
	if !signal.Ignored(syscall.SIGHUP) {
		t.Fatal("an inherited SIG_IGN must be handed back, not reset to SIG_DFL")
	}
}
