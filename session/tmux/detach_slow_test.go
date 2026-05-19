package tmux

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// TestDetach_LogsErrorWhenWgWaitExceedsThreshold is the regression guard for
// the defense-in-depth ERROR log added in fix-598. If the
// pause-while-attached gate in app/app.go ever regresses (or a new code
// path starts contending with the tmux server while the user is attached),
// Detach() should surface that loudly so we don't have to wait for a user
// to report another multi-second hang. The brief: wg.Wait > 5s → ERROR log
// naming the elapsed and pointing at the likely cause.
//
// We exercise the timing by:
//  1. Lowering slowDetachWgWaitThreshold to 50ms so the test runs quickly.
//  2. Adding a goroutine to t.wg that sleeps longer than the threshold —
//     simulating an io.Copy on a PTY whose tmux client is stuck waiting on
//     a contended tmux server.
//  3. Swapping out ErrorLog with a buffer-backed logger so we can assert
//     on what was emitted.
func TestDetach_LogsErrorWhenWgWaitExceedsThreshold(t *testing.T) {
	prevThreshold := slowDetachWgWaitThreshold
	slowDetachWgWaitThreshold = 50 * time.Millisecond
	t.Cleanup(func() { slowDetachWgWaitThreshold = prevThreshold })

	// Replace ErrorLog with a buffer-backed logger for assertion.
	prevErrorLog := aflog.ErrorLog
	var buf bytes.Buffer
	aflog.ErrorLog = log.New(&buf, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = prevErrorLog })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("slow-wg-wait", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	// Mimic Attach() bookkeeping. Add a goroutine to wg that sleeps past the
	// lowered threshold — this is the part Detach's wg.Wait blocks on.
	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	session.wg.Add(1)
	go func() {
		defer session.wg.Done()
		// Sleep long enough that the test deterministically crosses the
		// threshold even on a busy CI runner — 4x the threshold gives
		// plenty of room without dragging the test out.
		time.Sleep(4 * slowDetachWgWaitThreshold)
	}()

	session.Detach()

	got := buf.String()
	if !strings.Contains(got, "tmux.Detach: wg.Wait took") {
		t.Fatalf("expected ERROR log naming wg.Wait elapsed; got %q", got)
	}
	if !strings.Contains(got, "Sessions paused while attached should have prevented this") {
		t.Fatalf("expected ERROR log to reference the pause-while-attached fix; got %q", got)
	}
}

// TestDetach_DoesNotLogErrorOnFastWgWait is the inverted case: on a normal
// detach where wg.Wait finishes quickly (the io.Copy goroutine drains in a
// few ms), no ERROR should be emitted. Otherwise every benign detach would
// log spam and dull our reaction to a real regression.
func TestDetach_DoesNotLogErrorOnFastWgWait(t *testing.T) {
	prevThreshold := slowDetachWgWaitThreshold
	slowDetachWgWaitThreshold = 200 * time.Millisecond
	t.Cleanup(func() { slowDetachWgWaitThreshold = prevThreshold })

	prevErrorLog := aflog.ErrorLog
	var buf bytes.Buffer
	aflog.ErrorLog = log.New(&buf, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = prevErrorLog })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("fast-wg-wait", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	// No goroutine added: wg.Wait returns immediately.

	session.Detach()

	if strings.Contains(buf.String(), "tmux.Detach: wg.Wait took") {
		t.Fatalf("did not expect slow-detach ERROR on a fast wg.Wait; got %q", buf.String())
	}
}
