package execgroup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// captureWaitDelay is the WaitDelay these tests give a command. It is long
// enough that a command whose pipes close at once never reaches it, and short
// enough that a test which waits it out stays quick.
const captureWaitDelay = 500 * time.Millisecond

// detachedSettle is how long a survival test watches a member after the
// command returns. A SIGKILL that kill(2) has already queued lands within
// milliseconds, so a reap that reached the member shows up well inside it.
const detachedSettle = time.Second

// runCapture runs script under sh as the leader of its own group (Lead), with
// WaitDelay set, through Output or CombinedOutput.
func runCapture(t *testing.T, ctx context.Context, script string, combined bool, stdin []byte) (*exec.Cmd, []byte, error, time.Duration) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	Lead(cmd)
	cmd.WaitDelay = captureWaitDelay
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	start := time.Now()
	var out []byte
	var err error
	if combined {
		out, err = CombinedOutput(cmd)
	} else {
		out, err = Output(cmd)
	}
	return cmd, out, err, time.Since(start)
}

// planted is a process the script started and reported as "<pid> <pgid>" on
// one line of its output, with the leader's own pid as the line's third field.
type planted struct {
	pid, pgid, leader int
}

// parsePlanted reads every "<pid> <pgid> <leader>" line the script printed.
// It registers a cleanup that SIGKILLs each planted instance by identity, so
// nothing outlives the test, and a recycled pid is never hit.
func parsePlanted(t *testing.T, out []byte) []planted {
	t.Helper()
	var ps []planted
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		var nums [3]int
		ok := true
		for i := range f {
			n, err := strconv.Atoi(f[i])
			if err != nil || n <= 0 {
				ok = false
				break
			}
			nums[i] = n
		}
		if !ok {
			continue
		}
		p := planted{pid: nums[0], pgid: nums[1], leader: nums[2]}
		if proc, err := proctree.Lookup(p.pid); err == nil {
			t.Cleanup(func() { _ = proctree.Signal(proc, syscall.SIGKILL) })
		}
		ps = append(ps, p)
	}
	return ps
}

// requireInLeadersGroup proves a planted process was a member of the group
// the command's leader led, which is the group a reap reads. Without it, a
// process that left the group would pass a survival test vacuously.
func requireInLeadersGroup(t *testing.T, p planted) {
	t.Helper()
	if p.pgid != p.leader {
		t.Fatalf("planted pid %d is in group %d, not the leader's group %d; the test would prove nothing", p.pid, p.pgid, p.leader)
	}
	if p.leader == syscall.Getpgrp() {
		t.Fatalf("the command shares the test's process group %d; Lead did not take effect", p.leader)
	}
}

func gone(pid int) bool {
	_, err := proctree.Lookup(pid)
	return processGone(err)
}

func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if gone(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireStaysAlive fails unless pid is the same live instance for the whole
// settle window.
func requireStaysAlive(t *testing.T, pid int, why string) {
	t.Helper()
	proc, err := proctree.Lookup(pid)
	if err != nil {
		t.Fatalf("pid %d was not running when the command returned: %s (%v)", pid, why, err)
	}
	until := time.Now().Add(detachedSettle)
	for time.Now().Before(until) {
		if !proctree.AliveSame(proc) {
			t.Fatalf("pid %d died after the command returned: %s", pid, why)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The shape a ControlPersist master's ProxyCommand has on OpenSSH 8.0 and
// later: a member of the command's group with none of the command's pipes
// (its stdin and stdout go to the master, its stderr to /dev/null). A command
// that finishes on its own must leave it running, and must not wait for it.
// Reap after os/exec's Output killed it (#7115, #7121).
const detachedMember = `sleep ` + holderLife + ` </dev/null >/dev/null 2>&1 & ` +
	`d=$!; echo "$d $(ps -o pgid= -p $d | tr -d ' ') $$"; `

func TestOutputLeavesAMemberHoldingNoPipeAlive(t *testing.T) {
	_, out, err, elapsed := runCapture(t, context.Background(), detachedMember+"exit 0", false, nil)
	ps := parsePlanted(t, out)
	if err != nil {
		t.Fatalf("a command that exits 0 with every pipe closed must succeed: %v (output %q)", err, out)
	}
	if len(ps) != 1 {
		t.Fatalf("the script must report exactly the detached member, got %q", out)
	}
	requireInLeadersGroup(t, ps[0])
	if elapsed >= captureWaitDelay {
		t.Errorf("took %v: a member holding none of the pipes must not hold the command for WaitDelay (%v)", elapsed, captureWaitDelay)
	}
	requireStaysAlive(t, ps[0].pid, "a member that holds none of the command's pipes was signalled, which kills a ControlPersist master's transport")
}

// The case a group reap cannot get right: one member holds the pipe past
// WaitDelay (the straggler a reap exists for), and another holds none of the
// pipes (a ControlPersist proxy). Only the first may die.
func TestOutputReapsOnlyTheMemberHoldingThePipe(t *testing.T) {
	script := detachedMember +
		`sleep ` + holderLife + ` & h=$!; echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`
	_, out, err, elapsed := runCapture(t, context.Background(), script, false, nil)
	ps := parsePlanted(t, out)
	if len(ps) != 2 {
		t.Fatalf("the script must report the detached member and the holder, got %q (err %v)", out, err)
	}
	member, holder := ps[0], ps[1]
	requireInLeadersGroup(t, member)
	requireInLeadersGroup(t, holder)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("a zero exit with a pipe still held past WaitDelay must report exec.ErrWaitDelay, got %v", err)
	}
	if elapsed < captureWaitDelay {
		t.Errorf("returned after %v, before WaitDelay (%v): the holder must be given WaitDelay to finish", elapsed, captureWaitDelay)
	}
	if !waitGone(holder.pid, killBound) {
		t.Errorf("the member holding the pipe past WaitDelay (pid %d) is still running", holder.pid)
	}
	requireStaysAlive(t, member.pid, "the member holding none of the pipes was signalled alongside the straggler; a group kill does that, and it kills a ControlPersist master's transport")
}

// A non-zero exit still reaps a proven holder. os/exec drops ErrWaitDelay for
// it, so Reap after Output could not tell, but the pipe being open is the same
// evidence whatever the exit status.
func TestOutputReapsAHolderAfterANonZeroExit(t *testing.T) {
	script := `sleep ` + holderLife + ` & h=$!; echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; echo boom >&2; exit 3`
	_, out, err, _ := runCapture(t, context.Background(), script, false, nil)
	ps := parsePlanted(t, out)
	if len(ps) != 1 {
		t.Fatalf("the script must report the holder, got %q (err %v)", out, err)
	}
	requireInLeadersGroup(t, ps[0])
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("want the leader's exit status 3, got %v", err)
	}
	if got := strings.TrimSpace(string(ee.Stderr)); got != "boom" {
		t.Errorf("ExitError.Stderr = %q, want the captured stderr %q", got, "boom")
	}
	if !waitGone(ps[0].pid, killBound) {
		t.Errorf("the member holding the pipe past WaitDelay (pid %d) is still running after a non-zero exit", ps[0].pid)
	}
}

// A member holding stdin unread past WaitDelay, with input left to write, is a
// straggler too: it is what keeps the command from finishing.
func TestOutputReapsAMemberHoldingStdinUnread(t *testing.T) {
	script := `exec 3<&0; sleep ` + holderLife + ` <&3 >/dev/null 2>&1 & h=$!; ` +
		`echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`
	// More than any pipe buffer, so the copy cannot finish without a reader.
	stdin := bytes.Repeat([]byte("x"), 4<<20)
	_, out, err, _ := runCapture(t, context.Background(), script, false, stdin)
	ps := parsePlanted(t, out)
	if len(ps) != 1 {
		t.Fatalf("the script must report the holder, got %q (err %v)", out, err)
	}
	requireInLeadersGroup(t, ps[0])
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("a zero exit with stdin still held past WaitDelay must report exec.ErrWaitDelay, got %v", err)
	}
	if !waitGone(ps[0].pid, killBound) {
		t.Errorf("the member holding stdin unread past WaitDelay (pid %d) is still running", ps[0].pid)
	}
}

// A deadline is not a normal completion. KillOnCancel tears the whole group
// down, a member holding no pipe included, as before.
func TestOutputTimeoutStillKillsTheWholeGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The member's line is flushed before the foreground sleep starts, so it
	// survives the kill in the captured output.
	_, out, err, _ := runCapture(t, ctx, detachedMember+"sleep "+holderLife, false, nil)
	ps := parsePlanted(t, out)
	if len(ps) != 1 {
		t.Fatalf("the script must report the detached member, got %q (err %v)", out, err)
	}
	requireInLeadersGroup(t, ps[0])
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the command must fail at its deadline: err %v, ctx %v", err, ctx.Err())
	}
	if !waitGone(ps[0].pid, killBound) {
		t.Errorf("a deadline must still kill every member of the group, including pid %d, which holds no pipe", ps[0].pid)
	}
}

// Group membership is part of the proof, never descent. A process that holds
// the pipe but left the command's group is not reaped: the reap reads the
// group, and that process is no longer in it.
func TestOutputNeverSignalsAHolderOutsideTheGroup(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("perl is needed to start a process in a group of its own")
	}
	script := perl + ` -e 'setpgrp(0, 0); exec @ARGV' sleep ` + holderLife + ` & h=$!; ` +
		// Wait for the holder to leave the group before reporting it.
		`i=0; while [ "$(ps -o pgid= -p $h | tr -d ' ')" = "$$" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done; ` +
		`echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`
	_, out, err, _ := runCapture(t, context.Background(), script, false, nil)
	ps := parsePlanted(t, out)
	if len(ps) != 1 {
		t.Fatalf("the script must report the holder, got %q (err %v)", out, err)
	}
	if ps[0].pgid == ps[0].leader || ps[0].pgid != ps[0].pid {
		t.Fatalf("the holder must lead a group of its own, got pgid %d (leader %d)", ps[0].pgid, ps[0].leader)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("the holder kept the pipe past WaitDelay, so want exec.ErrWaitDelay, got %v", err)
	}
	requireStaysAlive(t, ps[0].pid, "a process outside the command's group was signalled because it held the pipe")
}

func TestOutputSeparatesStdoutAndStderr(t *testing.T) {
	_, out, err, _ := runCapture(t, context.Background(), `echo out; echo err >&2; exit 2`, false, nil)
	if string(out) != "out\n" {
		t.Errorf("stdout = %q, want %q", out, "out\n")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want an *exec.ExitError, got %v", err)
	}
	if string(ee.Stderr) != "err\n" {
		t.Errorf("ExitError.Stderr = %q, want %q", ee.Stderr, "err\n")
	}
}

func TestCombinedOutputMergesStreams(t *testing.T) {
	_, out, err, _ := runCapture(t, context.Background(), `echo a; echo b >&2; echo c`, true, nil)
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}
	if string(out) != "a\nb\nc\n" {
		t.Errorf("combined output = %q, want %q", out, "a\nb\nc\n")
	}
}

func TestOutputCopiesStdin(t *testing.T) {
	in := bytes.Repeat([]byte("0123456789"), 100_000)
	_, out, err, _ := runCapture(t, context.Background(), `cat`, false, in)
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("cat returned %d bytes, want the %d written to stdin", len(out), len(in))
	}
}

func TestOutputRefusesASetStdout(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Stdout = &bytes.Buffer{}
	if _, err := Output(cmd); err == nil {
		t.Fatal("Output must refuse a command whose Stdout is already set")
	}
	cmd = exec.Command("true")
	cmd.Stderr = &bytes.Buffer{}
	if _, err := CombinedOutput(cmd); err == nil {
		t.Fatal("CombinedOutput must refuse a command whose Stderr is already set")
	}
}

func TestOutputStartFailureClosesItsPipes(t *testing.T) {
	cmd := exec.Command("/nonexistent/af-execgroup-test")
	if _, err := Output(cmd); err == nil {
		t.Fatal("a command that cannot start must fail")
	}
	if f, ok := cmd.Stdout.(*os.File); !ok || f.Close() == nil {
		t.Error("the pipe Output made for a command that never started must already be closed")
	}
}

func TestHeadTailKeepsBothEnds(t *testing.T) {
	w := &headTail{n: 4}
	for _, s := range []string{"ab", "cdef", "ghij", "kl"} {
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got, want := string(w.Bytes()), "abcd\n... omitting 4 bytes ...\nijkl"; got != want {
		t.Errorf("Bytes() = %q, want %q", got, want)
	}
	short := &headTail{n: 4}
	_, _ = short.Write([]byte("abcdef"))
	if got := string(short.Bytes()); got != "abcdef" {
		t.Errorf("nothing was skipped, so Bytes() = %q, want %q", got, "abcdef")
	}
}
