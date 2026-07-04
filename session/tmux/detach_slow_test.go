package tmux

import (
	"bytes"
	"context"
	"errors"
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
// crash. Stubs the pgrep/kill hooks so the test doesn't shell out and
// stays deterministic.
func TestDetach_DoesNotPanicWhenKillAttachNil(t *testing.T) {
	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 50 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	prevPgrep := pgrepRunnerVar
	pgrepRunnerVar = func(string) ([]int, error) { return nil, nil }
	t.Cleanup(func() { pgrepRunnerVar = prevPgrep })

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

// TestDetach_BoundsWaitWhenKillAttachNilAndWgHangs is the direct regression
// guard for the 51-second hang observed at 00:05:14 on 2026-05-20: when
// Detach() runs without a working killAttach AND the io.Copy goroutine is
// genuinely stuck (PTY drain never returns), the previous implementation
// fell through to an unconditional <-waitDone after logging "cannot
// SIGKILL", blocking the user's TUI indefinitely.
//
// The new contract: even if the pgrep fallback fails to find anything to
// kill, the secondary wait MUST be bounded by wgWaitAbandonDeadline.
// Returning with a leaked goroutine is correct here — the kernel will
// eventually unstick the PTY and the goroutine will exit on its own. The
// test asserts:
//
//  1. Detach returns within wgWaitSigkillDeadline + wgWaitAbandonDeadline +
//     slack, not the multi-second hang the original incident produced.
//  2. The ERROR log naming the abandoned wait fires so future occurrences
//     are visible in agent-factory.log.
//  3. The pgrep fallback was attempted (stubbed to return nothing).
//
// We deliberately leave the wg goroutine running past Detach's return.
// That IS the bug fix: leaking one goroutine is acceptable when the
// alternative is freezing the TUI for 51 seconds. The goroutine drains
// later via t.Cleanup so the test process exits cleanly.
func TestDetach_BoundsWaitWhenKillAttachNilAndWgHangs(t *testing.T) {
	prevSigkill := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 50 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevSigkill })

	prevAbandon := wgWaitAbandonDeadline
	wgWaitAbandonDeadline = 100 * time.Millisecond
	t.Cleanup(func() { wgWaitAbandonDeadline = prevAbandon })

	// pgrep returns no matches — simulates the real case where the attach
	// client either already exited or never matched our pattern. The
	// secondary timeout must still fire.
	var pgrepCalls atomic.Int32
	prevPgrep := pgrepRunnerVar
	pgrepRunnerVar = func(pattern string) ([]int, error) {
		pgrepCalls.Add(1)
		return nil, nil
	}
	t.Cleanup(func() { pgrepRunnerVar = prevPgrep })

	prevErr := aflog.ErrorLog
	var errBuf bytes.Buffer
	aflog.ErrorLog = log.New(&errBuf, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = prevErr })

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("bounded-wait-no-kill", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	session.killAttach = nil // the trigger condition from the 00:05:14 incident

	// A genuinely hung io.Copy: do not exit until told. This is the
	// "kernel never drains the PTY" simulation.
	releaseHang := make(chan struct{})
	t.Cleanup(func() { close(releaseHang) })
	// Capture wg locally so Done() doesn't race against Detach's defer
	// nilifying t.wg. The leaked-goroutine contract requires the
	// goroutine to survive past Detach's return — that survival can't
	// itself crash the test process.
	wg := session.wg
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-releaseHang
	}()

	detachDone := make(chan struct{})
	go func() {
		session.Detach()
		close(detachDone)
	}()

	// Total ceiling: sigkill (50ms) + abandon (100ms) + slack for Restore
	// + log flush + scheduler. 2s is generous and still proves the
	// guarantee — the original incident took 51s.
	select {
	case <-detachDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Detach blocked past sigkill+abandon deadlines — the secondary bound is not firing")
	}

	if got := pgrepCalls.Load(); got != 1 {
		t.Fatalf("expected pgrep fallback to be attempted exactly once when killAttach is nil; got %d calls", got)
	}
	if !strings.Contains(warnBuf.String(), "attempting pgrep-based fallback") {
		t.Fatalf("expected WARN log noting pgrep fallback attempt; got %q", warnBuf.String())
	}
	if !strings.Contains(errBuf.String(), "abandoning wg.Wait") {
		t.Fatalf("expected ERROR log on the abandon path; got %q", errBuf.String())
	}
}

// TestDetach_ProactiveSIGTERMUnblocksWithoutSIGKILL is the primary #1157
// regression guard: when the attach child exits on a SIGTERM (the healthy
// case — a well-behaved tmux client detaches and closes the slave PTY), the
// detach must complete off the back of that proactive signal WITHOUT ever
// falling to the 1s SIGKILL race that used to hang ~32% of detaches. We keep
// wgWaitSigkillDeadline deliberately high (2s): if the proactive path
// regressed and we fell through to the SIGKILL backstop, the elapsed
// assertion below would blow past its ceiling and fail. The stub termAttach
// stands in for "SIGTERM → client exits → slave closes → io.Copy returns" by
// releasing the simulated io.Copy goroutine.
func TestDetach_ProactiveSIGTERMUnblocksWithoutSIGKILL(t *testing.T) {
	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 2 * time.Second
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	session := newDrainableSession(t, "proactive-sigterm")

	var termCalls, killCalls atomic.Int32
	released := make(chan struct{})
	session.termAttach = func() (int, error) {
		termCalls.Add(1)
		close(released) // SIGTERM lands → child exits → io.Copy returns
		return 4321, nil
	}
	session.killAttach = func() (int, error) {
		killCalls.Add(1)
		return 0, nil
	}

	session.wg.Add(1)
	go func() {
		defer session.wg.Done()
		<-released
	}()

	start := time.Now()
	session.Detach()
	elapsed := time.Since(start)

	if got := termCalls.Load(); got != 1 {
		t.Fatalf("expected the proactive SIGTERM to be sent exactly once; got %d", got)
	}
	if got := killCalls.Load(); got != 0 {
		t.Fatalf("SIGKILL backstop must NOT fire when SIGTERM unblocks io.Copy; got %d kill calls", got)
	}
	// The whole point of the fix: no 1s race. A generous 500ms ceiling — far
	// under the 2s wgWaitSigkillDeadline — proves the detach rode the SIGTERM,
	// not the SIGKILL fallback.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("proactive-SIGTERM detach took %v; expected sub-grace — io.Copy should unblock on SIGTERM, not the ~1s SIGKILL race", elapsed)
	}
}

// TestDetach_FallsBackToSIGKILLWhenSIGTERMIgnored is the paired backstop
// guard required for the most regression-prone file in the repo: the
// proactive SIGTERM is an OPTIMISTIC first step, never a replacement for the
// #601 SIGKILL fallback. Here the stub termAttach reports success but the
// simulated client ignores the signal (never releases the io.Copy goroutine);
// only SIGKILL unsticks it. The detach must still bound itself, escalate to
// SIGKILL after the grace + deadline, and emit the SIGKILL WARN unchanged.
func TestDetach_FallsBackToSIGKILLWhenSIGTERMIgnored(t *testing.T) {
	prevGrace := proactiveGraceDeadline
	proactiveGraceDeadline = 30 * time.Millisecond
	t.Cleanup(func() { proactiveGraceDeadline = prevGrace })

	prevDeadline := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 50 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevDeadline })

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	session := newDrainableSession(t, "sigterm-ignored")

	var termCalls, killCalls atomic.Int32
	killed := make(chan struct{})
	session.termAttach = func() (int, error) {
		termCalls.Add(1)
		return 4321, nil // SIGTERM sent, but the client ignores it — no release
	}
	session.killAttach = func() (int, error) {
		killCalls.Add(1)
		close(killed) // only SIGKILL unsticks the io.Copy goroutine
		return 4321, nil
	}

	session.wg.Add(1)
	go func() {
		defer session.wg.Done()
		select {
		case <-killed:
		case <-time.After(2 * time.Second):
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
		t.Fatal("Detach did not return — the SIGKILL backstop did not fire after SIGTERM was ignored")
	}

	if got := termCalls.Load(); got != 1 {
		t.Fatalf("expected the proactive SIGTERM to be attempted once; got %d", got)
	}
	if got := killCalls.Load(); got != 1 {
		t.Fatalf("expected the SIGKILL backstop to fire exactly once when SIGTERM is ignored; got %d", got)
	}
	if !strings.Contains(warnBuf.String(), "SIGKILLing tmux attach-session pid=4321") {
		t.Fatalf("expected the #601 SIGKILL backstop WARN to still fire; got %q", warnBuf.String())
	}
}

// TestDetach_TermAttachSurvivesNextDetach is the #602 pairing guard extended
// to the new closure: just as killAttach must survive a Detach so the next
// attach lifecycle inherits a working escape hatch, termAttach — set in the
// same Restore and cleared at the same inline site — must be non-nil after
// Detach returns. If a future refactor moved either clear back into the defer
// (which runs AFTER Restore installs the fresh closures), this fails, exactly
// as the 2026-05-20 51s-hang incident would have.
func TestDetach_TermAttachSurvivesNextDetach(t *testing.T) {
	prevSigkill := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 500 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevSigkill })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("termattach-survives", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}
	if session.termAttach == nil {
		t.Fatal("Restore should have set termAttach")
	}
	if session.killAttach == nil {
		t.Fatal("Restore should have set killAttach")
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	session.Detach()

	if session.termAttach == nil {
		t.Fatal("Detach's post-Restore state should leave termAttach non-nil — #602 pairing regression")
	}
	if session.killAttach == nil {
		t.Fatal("Detach's post-Restore state should leave killAttach non-nil")
	}
}

// TestKillTmuxAttachByName_KillsMatchingPids verifies the pgrep fallback
// actually invokes the kill hook for every pid pgrep returns. This is the
// "what happens after we find a stuck attach client" half of the safety
// net. We stub both pgrep and kill so the test never touches real
// processes — the goal is to prove wiring, not to test pgrep itself.
func TestKillTmuxAttachByName_KillsMatchingPids(t *testing.T) {
	prevPgrep := pgrepRunnerVar
	pgrepRunnerVar = func(pattern string) ([]int, error) {
		// Confirm the pattern is anchored to the attach-session invocation
		// for the given name — we don't want bare-name matches that could
		// hit a concurrent `tmux kill-session -t <name>`.
		if !strings.Contains(pattern, "tmux attach-session -t ") {
			t.Errorf("pgrep pattern should anchor to attach-session invocation; got %q", pattern)
		}
		if !strings.Contains(pattern, "my-session") {
			t.Errorf("pgrep pattern should reference the session name; got %q", pattern)
		}
		return []int{1111, 2222}, nil
	}
	t.Cleanup(func() { pgrepRunnerVar = prevPgrep })

	var killed []int
	var mu sync.Mutex
	prevKill := killByPidVar
	killByPidVar = func(pid int) error {
		mu.Lock()
		defer mu.Unlock()
		killed = append(killed, pid)
		return nil
	}
	t.Cleanup(func() { killByPidVar = prevKill })

	n, err := killTmuxAttachByName("my-session")
	if err != nil {
		t.Fatalf("killTmuxAttachByName: unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 kills; got %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(killed) != 2 || killed[0] != 1111 || killed[1] != 2222 {
		t.Fatalf("expected SIGKILL to pids [1111 2222]; got %v", killed)
	}
}

// TestKillTmuxAttachByName_NoMatches confirms the "session already
// exited" case is handled silently — pgrep returns no pids, no kill is
// attempted, no error is surfaced. This is the common happy-path for the
// pgrep fallback: in most cases by the time we reach this branch the
// attach client has already exited and there's nothing to kill.
func TestKillTmuxAttachByName_NoMatches(t *testing.T) {
	prevPgrep := pgrepRunnerVar
	pgrepRunnerVar = func(string) ([]int, error) { return nil, nil }
	t.Cleanup(func() { pgrepRunnerVar = prevPgrep })

	var killCalls atomic.Int32
	prevKill := killByPidVar
	killByPidVar = func(int) error { killCalls.Add(1); return nil }
	t.Cleanup(func() { killByPidVar = prevKill })

	n, err := killTmuxAttachByName("missing")
	if err != nil {
		t.Fatalf("expected no error for no-match case; got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 kills for no-match case; got %d", n)
	}
	if got := killCalls.Load(); got != 0 {
		t.Fatalf("expected zero kill invocations when pgrep returns nothing; got %d", got)
	}
}

// TestDetach_LogsErrorWhenBothFallbacksFail covers the double-failure
// path called out in the brief: both killAttach is nil AND the pgrep
// fallback returns nothing (or an error), AND wg.Wait is still stuck.
// We must log the abandon ERROR and return — never block. This is
// distinct from TestDetach_BoundsWaitWhenKillAttachNilAndWgHangs above
// in that it asserts on the specific log text used to surface the
// failure mode in agent-factory.log, so an operator grepping for the
// regression signature can find it.
func TestDetach_LogsErrorWhenBothFallbacksFail(t *testing.T) {
	prevSigkill := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 30 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevSigkill })

	prevAbandon := wgWaitAbandonDeadline
	wgWaitAbandonDeadline = 60 * time.Millisecond
	t.Cleanup(func() { wgWaitAbandonDeadline = prevAbandon })

	// pgrep returns an error — simulates "pgrep not on PATH" or other
	// unexpected shellout failures. The fallback should log the error
	// and still hit the abandon deadline cleanly.
	prevPgrep := pgrepRunnerVar
	pgrepRunnerVar = func(string) ([]int, error) {
		return nil, errors.New("synthetic pgrep failure")
	}
	t.Cleanup(func() { pgrepRunnerVar = prevPgrep })

	prevErr := aflog.ErrorLog
	var errBuf bytes.Buffer
	aflog.ErrorLog = log.New(&errBuf, "ERROR: ", 0)
	t.Cleanup(func() { aflog.ErrorLog = prevErr })

	prevWarn := aflog.WarningLog
	var warnBuf bytes.Buffer
	aflog.WarningLog = log.New(&warnBuf, "WARN: ", 0)
	t.Cleanup(func() { aflog.WarningLog = prevWarn })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("double-fallback-fail", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}

	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())
	session.killAttach = nil

	releaseHang := make(chan struct{})
	t.Cleanup(func() { close(releaseHang) })
	// Capture wg locally so Done() doesn't race against Detach's defer
	// nilifying t.wg. The leaked-goroutine contract requires the
	// goroutine to survive past Detach's return — that survival can't
	// itself crash the test process.
	wg := session.wg
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-releaseHang
	}()

	detachDone := make(chan struct{})
	go func() {
		session.Detach()
		close(detachDone)
	}()

	select {
	case <-detachDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Detach blocked despite both fallbacks being exhausted")
	}

	if !strings.Contains(warnBuf.String(), "pgrep fallback failed") {
		t.Fatalf("expected WARN log noting pgrep failure; got %q", warnBuf.String())
	}
	if !strings.Contains(errBuf.String(), "abandoning wg.Wait") {
		t.Fatalf("expected ERROR log on the abandon path; got %q", errBuf.String())
	}
}

// TestDetach_KillAttachSurvivesNextDetach is the direct regression guard
// for Problem A: in the 2026-05-20 incident, the previous Detach's defer
// nilified t.killAttach AFTER Restore() had set a fresh closure for the
// next attach lifecycle. The next Detach then ran with killAttach == nil
// and hit the unbounded-wait bug.
//
// The fix moves the killAttach clear to the inline t.ptmx = nil site
// (before Restore), so by the time Detach returns, t.killAttach is the
// fresh closure Restore just installed — not nil. This test
// reproduces the sequence: Detach → Restore → check killAttach is still
// set. Without the fix, killAttach would be nil here.
func TestDetach_KillAttachSurvivesNextDetach(t *testing.T) {
	// Keep deadlines short so this test doesn't slow the suite, even
	// though we expect wg.Wait to return promptly.
	prevSigkill := wgWaitSigkillDeadline
	wgWaitSigkillDeadline = 500 * time.Millisecond
	t.Cleanup(func() { wgWaitSigkillDeadline = prevSigkill })

	ptyFactory := NewMockPtyFactory(t)
	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return nil, nil },
	}

	session := newTmuxSession(toTmuxName("killattach-survives", ""), "claude", ptyFactory, cmdExec)
	if err := session.Restore(""); err != nil {
		t.Fatalf("initial Restore: %v", err)
	}
	if session.killAttach == nil {
		t.Fatal("Restore should have set killAttach")
	}

	// Stand in for Attach()'s bookkeeping so Detach has something to
	// tear down. wg has no goroutines so wg.Wait returns immediately
	// and we never enter the SIGKILL branch.
	session.attachCh = make(chan struct{})
	session.wg = &sync.WaitGroup{}
	session.ctx, session.cancel = context.WithCancel(context.Background())

	session.Detach()

	// The contract we're enforcing: after Detach returns, the next
	// attach lifecycle inherits a Restore-installed killAttach, not
	// the nil left behind by the old defer. The trace at 00:05:14 on
	// 2026-05-20 showed exactly this invariant breaking — the warning
	// at tmux.go:387 only fires when killAttach == nil at the start
	// of waitForAttachDrain.
	if session.killAttach == nil {
		t.Fatal("Detach's post-Restore state should leave killAttach non-nil — Problem A regression")
	}
	if session.ptmx == nil {
		t.Fatal("Detach's post-Restore state should leave ptmx non-nil")
	}
}
