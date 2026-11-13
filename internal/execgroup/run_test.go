package execgroup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordSweeps replaces the after-deadline sweep with one that records each
// pgid it is handed and then sweeps for real, so a test sees whether it ran.
func recordSweeps(t *testing.T) func() []int {
	t.Helper()
	var mu sync.Mutex
	var swept []int
	prev := sweepAfterCancel
	sweepAfterCancel = func(pgid int) {
		mu.Lock()
		swept = append(swept, pgid)
		mu.Unlock()
		prev(pgid)
	}
	t.Cleanup(func() { sweepAfterCancel = prev })
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), swept...)
	}
}

// A deadline is torn down at least as thoroughly as before #7236 (#7241):
// KillOnCancel's sweep, then a second once the leader has been waited, which
// Reap used to give every command. On darwin the second is what catches a
// child the first missed because the group was forking it.
func TestRunSweepsTheGroupAgainAfterADeadline(t *testing.T) {
	swept := recordSweeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep "+holderLife)
	Lead(cmd)
	cmd.WaitDelay = captureWaitDelay
	if _, err := Output(cmd); err == nil {
		t.Fatal("a command still running at its deadline must fail")
	}
	if got := swept(); len(got) != 1 || got[0] != cmd.Process.Pid {
		t.Errorf("sweeps after the deadline = %v, want one of the leader's group %d", got, cmd.Process.Pid)
	}
}

// The second sweep is keyed on the Cancel having run, never on the context
// being done. A command that exits 0 at once, with a member holding its pipe,
// returns only after WaitDelay, and a deadline that passes meanwhile must not
// turn that into a group kill: the group also holds the ControlPersist proxy
// that #7121 spares.
func TestRunDoesNotSweepWhenTheDeadlinePassesDuringCleanup(t *testing.T) {
	swept := recordSweeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), captureWaitDelay/2)
	defer cancel()
	script := detachedMember +
		`sleep ` + holderLife + ` & h=$!; echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`
	cmd, out, err, _ := runCapture(t, ctx, script, false, nil)
	ps := parsePlanted(t, out)
	if len(ps) != 2 {
		t.Fatalf("the script must report the detached member and the holder, got %q (err %v)", out, err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("the deadline must have passed during WaitDelay, or this test proves nothing")
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("the leader exited 0 before its deadline, so want exec.ErrWaitDelay, got %v", err)
	}
	if got := swept(); len(got) != 0 {
		t.Errorf("swept %v after a leader that exited on its own", got)
	}
	requireInLeadersGroup(t, ps[0])
	requireStaysAlive(t, ps[0].pid, "a deadline that passed during cleanup swept the group, killing a member that holds no pipe")
	_ = cmd
}

func TestRunNeverSweepsAfterANormalExit(t *testing.T) {
	swept := recordSweeps(t)
	_, _, err, _ := runCapture(t, context.Background(), "exit 3", false, nil)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("want exit status 3, got %v", err)
	}
	if got := swept(); len(got) != 0 {
		t.Errorf("swept %v after a command that exited on its own", got)
	}
}

// Output owns a Stderr the caller set too (#7241, R8). Left to os/exec, that
// pipe had its own WaitDelay inside Wait, and a member holding only it was
// never looked for, let alone reaped.
func TestOutputReapsAHolderOfACallerSetStderr(t *testing.T) {
	script := `sleep ` + holderLife + ` </dev/null >/dev/null & h=$!; ` +
		`echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; echo to-stderr >&2; exit 0`
	cmd := exec.CommandContext(context.Background(), "sh", "-c", script)
	Lead(cmd)
	cmd.WaitDelay = captureWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := Output(cmd)
	elapsed := time.Since(start)
	ps := parsePlanted(t, out)
	if len(ps) != 1 {
		t.Fatalf("the script must report the holder, got %q (err %v)", out, err)
	}
	requireInLeadersGroup(t, ps[0])
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("want exec.ErrWaitDelay for a zero exit with stderr held past WaitDelay, got %v", err)
	}
	if !waitGone(ps[0].pid, killBound) {
		t.Errorf("the member holding only the caller's stderr (pid %d) is still running", ps[0].pid)
	}
	if elapsed >= 2*captureWaitDelay {
		t.Errorf("took %v: one WaitDelay (%v), not two, may pass before the holder is reaped", elapsed, captureWaitDelay)
	}
	if got := strings.TrimSpace(stderr.String()); got != "to-stderr" {
		t.Errorf("the caller's Stderr got %q, want %q", got, "to-stderr")
	}
}

// serialWriter fails the test's check if two Writes ever overlap.
type serialWriter struct {
	inflight atomic.Int32
	overlap  atomic.Bool
	mu       sync.Mutex
	buf      bytes.Buffer
}

func (w *serialWriter) Write(p []byte) (int, error) {
	if w.inflight.Add(1) > 1 {
		w.overlap.Store(true)
	}
	defer w.inflight.Add(-1)
	time.Sleep(time.Millisecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// One writer given as both Stdout and Stderr gets one pipe, so it is never
// written from two goroutines at once, as os/exec promises.
func TestRunSharesOnePipeForOneWriter(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c",
		`i=0; while [ $i -lt 200 ]; do echo out$i; echo err$i >&2; i=$((i+1)); done`)
	Lead(cmd)
	cmd.WaitDelay = captureWaitDelay
	w := &serialWriter{}
	cmd.Stdout, cmd.Stderr = w, w
	if err := Run(cmd); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w.overlap.Load() {
		t.Error("the shared writer was written from two goroutines at once")
	}
	if !strings.Contains(w.buf.String(), "out199\nerr199\n") {
		t.Errorf("both streams must reach the writer in order, got the tail %q", tail(w.buf.String(), 40))
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

var errRefused = errors.New("refused")

// refusingWriter takes its first write and refuses the rest.
type refusingWriter struct{ n atomic.Int32 }

func (w *refusingWriter) Write(p []byte) (int, error) {
	if w.n.Add(1) > 1 {
		return 0, errRefused
	}
	return len(p), nil
}

// A writer that refuses bytes stops delivery to it, and the rest is read and
// discarded, so the child never blocks on a pipe nobody drains. os/exec closes
// the read end instead; Run keeps it open, because a reap may still be
// searching for that pipe (#7243). The refusal is the error when nothing else
// failed.
func TestRunReportsAWriterThatRefuses(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c",
		`trap '' PIPE; i=0; while [ $i -lt 20000 ]; do echo line$i 2>/dev/null; i=$((i+1)); done; exit 0`)
	Lead(cmd)
	cmd.WaitDelay = captureWaitDelay
	cmd.Stdout = &refusingWriter{}
	done := make(chan error, 1)
	go func() { done <- Run(cmd) }()
	select {
	case err := <-done:
		if !errors.Is(err, errRefused) {
			t.Errorf("want the writer's refusal as the error, got %v", err)
		}
	case <-time.After(killBound):
		t.Fatal("Run did not return: a refused writer left the child blocked on a pipe nobody drains")
	}
}
