package tmux

import (
	"bytes"
	"context"
	"log"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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

// TestDetach_SIGKILLsAttachOnSlowWgWait is the regression guard for the
// follow-up fix from issue #598: when wg.Wait blocks longer than
// wgWaitSigkillDeadline, Detach() must SIGKILL the tmux attach-session
// child so the io.Copy goroutine's PTY-master Read returns EOF. Without
// this, the user's sidebar is held hostage by whichever process is starving
// the tmux server (in the original incident: the daemon's capture-pane
// poll, which lives in a separate process the in-app pause-while-attached
// gate from #600 cannot reach). The test verifies the kill is invoked AND
// that Detach returns even though the simulated io.Copy goroutine was
// hanging — the whole point of the fix is "Detach returns within ~1s".
func TestDetach_SIGKILLsAttachOnSlowWgWait(t *testing.T) {
	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 50 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	// Replace WarningLog with a buffer so we can assert the SIGKILL log
	// landed with the recorded pid (the diagnostic that lets us tie a
	// future hang back to which client got killed).
	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("sigkill-on-slow-wg", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	// Swap the real (mock-Process-nil) killAttach for one that records the
	// call and signals the simulated io.Copy goroutine to exit. This is
	// what would happen for real: SIGKILL → slave closes → master Read
	// returns EOF → io.Copy returns → wg.Done. Here we short-circuit
	// directly to the goroutine.
	var killCalls atomic.Int32
	killed := make(chan struct{})
	session.killAttach = func() (int, error) {
		killCalls.Add(1)
		close(killed)
		return 12345, nil
	}

	session.wg.Add(1)
	go func() {
		defer session.wg.Done()
		select {
		case <-killed:
			// SIGKILL arrived — simulate io.Copy returning immediately.
		case <-time.After(2 * time.Second):
			// Safety bound: if Detach never invokes killAttach the
			// goroutine still drains so the test doesn't leak. The
			// killCalls assertion below will catch the missing call.
		}
	}()

	detachDone := make(chan struct{})
	go func() {
		session.Detach()
		close(detachDone)
	}()

	select {
	case <-detachDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Detach did not return within 3s — the SIGKILL fallback is not unblocking wg.Wait")
	}

	if got := killCalls.Load(); got != 1 {
		t.Fatalf("expected killAttach to be invoked exactly once after wgWaitSigkillDeadline; got %d calls", got)
	}
	if !strings.Contains(warnBuf.String(), "SIGKILLing tmux attach-session pid=12345") {
		t.Fatalf("expected WARN log naming the SIGKILLed pid; got %q", warnBuf.String())
	}
}

// TestDetach_DoesNotSIGKILLOnFastWgWait covers the inverse: when wg.Wait
// returns inside the deadline (the overwhelming common case), killAttach
// must not be invoked. SIGKILLing the attach client unnecessarily would
// race with the normal io.Copy drain and could surface as spurious
// "process killed" log noise even though the detach worked correctly.
func TestDetach_DoesNotSIGKILLOnFastWgWait(t *testing.T) {
	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 500 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("no-sigkill-on-fast-wg", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	var killCalls atomic.Int32
	session.killAttach = func() (int, error) {
		killCalls.Add(1)
		return 0, nil
	}
	// No goroutine added: wg.Wait returns immediately.

	session.Detach()

	if got := killCalls.Load(); got != 0 {
		t.Fatalf("expected killAttach to NOT be invoked on a fast wg.Wait; got %d calls", got)
	}
}

// TestDetach_DoesNotPanicWhenKillAttachNil guards the should-not-happen
// path: if somehow Detach() runs without Restore() having set killAttach
// (a bug elsewhere, or future refactor that loses the wiring), the SIGKILL
// fallback must degrade to a logged warning rather than panicking on a nil
// function pointer. We don't want a defensive bound to itself become a
// crash.
func TestDetach_DoesNotPanicWhenKillAttachNil(t *testing.T) {
	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 50 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("nil-killattach", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	// Force the should-not-happen state by clearing what Restore set.
	session.killAttach = nil

	session.wg.Add(1)
	go func() {
		defer session.wg.Done()
		// Sleep past the deadline so the SIGKILL branch runs, then drain
		// so the test doesn't hang forever.
		time.Sleep(150 * time.Millisecond)
	}()

	// Must not panic.
	session.Detach()

	if !strings.Contains(warnBuf.String(), "no attach process recorded") {
		t.Fatalf("expected WARN log noting missing attach process; got %q", warnBuf.String())
	}
}
