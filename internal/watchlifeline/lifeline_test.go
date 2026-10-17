package watchlifeline

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for af: started with Arg, it runs the
// lifeline exactly as main does.
func TestMain(m *testing.M) {
	HandleExec()
	os.Exit(m.Run())
}

// lifelineProc is one lifeline started the way the daemon starts it: its own
// process group, the lifeline read end on fd 3, the ack write end on fd 4.
type lifelineProc struct {
	cmd    *exec.Cmd
	keep   *os.File // the daemon's lifeline write end; closing it = daemon death
	ack    string
	stdout *bufio.Reader
	done   chan error
}

func startLifeline(t *testing.T, grace time.Duration, script string) *lifelineProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lr, lw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ar, aw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, Argv(grace, "sh", "-c", script)...)
	cmd.ExtraFiles = []*os.File{lr, aw}
	cmd.Stdout = outW
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = lr.Close()
	_ = aw.Close()
	_ = outW.Close()
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = lw.Close()
		_ = outR.Close()
	})
	ackLine, _ := bufio.NewReader(ar).ReadString('\n')
	_ = ar.Close()
	p := &lifelineProc{cmd: cmd, keep: lw, ack: strings.TrimSuffix(ackLine, "\n"), stdout: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	return p
}

func (p *lifelineProc) readPID(t *testing.T) int {
	t.Helper()
	line, err := p.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse pid %q: %v", line, err)
	}
	return pid
}

func (p *lifelineProc) wait(t *testing.T, within time.Duration) *os.ProcessState {
	t.Helper()
	select {
	case <-p.done:
		return p.cmd.ProcessState
	case <-time.After(within):
		t.Fatalf("lifeline still running after %s", within)
		return nil
	}
}

func gone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func waitGone(t *testing.T, pid int, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !gone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) still alive after %s", what, pid, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShellExitCodePassesThrough(t *testing.T) {
	p := startLifeline(t, time.Second, "exit 7")
	if p.ack != AckStarted {
		t.Fatalf("ack %q, want %q", p.ack, AckStarted)
	}
	state := p.wait(t, 5*time.Second)
	if state.ExitCode() != 7 {
		t.Fatalf("lifeline exit %v, want the shell's exit status 7", state)
	}
}

// TestShellSignalDeathPassesThrough: a shell killed by SIGTERM must read as a
// SIGTERM death to the daemon, which treats that as stop/reload rather than a
// failure that counts against the crash-loop budget.
func TestShellSignalDeathPassesThrough(t *testing.T) {
	p := startLifeline(t, time.Second, "kill -TERM $$; sleep 5")
	state := p.wait(t, 5*time.Second)
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("lifeline wait status %v, want death by SIGTERM", state)
	}
}

// TestDaemonDeathStopsWholeGroup is #5329: the daemon dies (its lifeline write
// end closes) while the watch command and a TERM-ignoring grandchild run. The
// lifeline SIGTERMs the group, then SIGKILLs it after the grace.
func TestDaemonDeathStopsWholeGroup(t *testing.T) {
	const grace = 300 * time.Millisecond
	p := startLifeline(t, grace, `sh -c 'trap "" TERM; echo $$; while :; do sleep 0.05; done' & wait`)
	grandchild := p.readPID(t)
	time.Sleep(100 * time.Millisecond) // let the grandchild install its trap

	_ = p.keep.Close()

	state := p.wait(t, grace+5*time.Second)
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("lifeline wait status %v, want death by its own group SIGKILL", state)
	}
	waitGone(t, grandchild, 5*time.Second, "TERM-ignoring grandchild")
}

// TestDaemonDeathTermIsEnoughForCooperativeScript: a script that exits on
// SIGTERM is gone well before the grace, so the lifeline does not wait it out.
func TestDaemonDeathTermIsEnoughForCooperativeScript(t *testing.T) {
	const grace = 3 * time.Second
	p := startLifeline(t, grace, `echo $$; while :; do sleep 0.05; done`)
	shell := p.readPID(t)

	start := time.Now()
	_ = p.keep.Close()
	p.wait(t, grace+5*time.Second)
	if elapsed := time.Since(start); elapsed >= grace {
		t.Fatalf("lifeline took %s, the whole grace, though the script exits on SIGTERM", elapsed)
	}
	waitGone(t, shell, 5*time.Second, "watch shell")
}

// TestGroupSIGTERMDoesNotOrphanTheScript: the daemon's stop path (and a
// script's own `kill 0`) SIGTERMs the whole group. The lifeline must survive
// it, so a script that ignores SIGTERM is still bound to the daemon.
func TestGroupSIGTERMDoesNotOrphanTheScript(t *testing.T) {
	p := startLifeline(t, 200*time.Millisecond, `trap "" TERM; echo $$; while :; do sleep 0.05; done`)
	shell := p.readPID(t)
	time.Sleep(100 * time.Millisecond)

	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		t.Fatalf("lifeline exited on a group SIGTERM (%v); the TERM-ignoring script is now unbound", p.cmd.ProcessState)
	case <-time.After(300 * time.Millisecond):
	}

	_ = p.keep.Close()
	p.wait(t, 5*time.Second)
	waitGone(t, shell, 5*time.Second, "TERM-ignoring watch shell")
}

func TestShellStartFailureIsAcked(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lr, lw, _ := os.Pipe()
	ar, aw, _ := os.Pipe()
	defer lw.Close()
	cmd := exec.Command(exe, Argv(time.Second, "/nonexistent/af-test-shell", "-c", "true")...)
	cmd.ExtraFiles = []*os.File{lr, aw}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = lr.Close()
	_ = aw.Close()
	line, _ := io.ReadAll(ar)
	err = cmd.Wait()

	supported, ok, startErr := ParseAck(string(line))
	if !supported || ok || startErr == nil {
		t.Fatalf("ack %q parsed as supported=%v ok=%v err=%v, want a start failure", line, supported, ok, startErr)
	}
	if cmd.ProcessState.ExitCode() != ExitStartFailed {
		t.Fatalf("exit %v (%v), want %d", cmd.ProcessState, err, ExitStartFailed)
	}
}

// TestUnknownProtocolRefusesWithoutAck: a binary must never half-speak a
// protocol version it does not know. No ack means the daemon falls back.
func TestUnknownProtocolRefusesWithoutAck(t *testing.T) {
	lr, lw, _ := os.Pipe()
	ar, aw, _ := os.Pipe()
	defer lr.Close()
	defer lw.Close()
	defer ar.Close()

	code := Run([]string{Arg, "v2", "5000", "--", "sh", "-c", "true"}, lr, aw)
	_ = aw.Close()
	line, _ := io.ReadAll(ar)

	if code != ExitUsage || len(line) != 0 {
		t.Fatalf("Run = %d, ack %q; want %d and no ack", code, line, ExitUsage)
	}
}

func TestMissingDescriptorsRefuse(t *testing.T) {
	if code := Run(Argv(time.Second, "sh", "-c", "true"), nil, nil); code != ExitUsage {
		t.Fatalf("Run without descriptors = %d, want %d", code, ExitUsage)
	}
	// An open descriptor that is not a pipe is not a lifeline either.
	f, err := os.CreateTemp(t.TempDir(), "not-a-pipe")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if code := Run(Argv(time.Second, "sh", "-c", "true"), f, f); code != ExitUsage {
		t.Fatalf("Run with a regular file for descriptors = %d, want %d", code, ExitUsage)
	}
}

func TestArgvParses(t *testing.T) {
	inv, err := parse(Argv(1500*time.Millisecond, "/bin/zsh", "-c", "echo -- hi"))
	if err != nil {
		t.Fatal(err)
	}
	if inv.grace != 1500*time.Millisecond || inv.shell != "/bin/zsh" || strings.Join(inv.args, "|") != "-c|echo -- hi" {
		t.Fatalf("parsed %+v", inv)
	}
	for _, bad := range [][]string{
		{Arg},
		{Arg, Protocol, "x", "--", "sh"},
		{Arg, Protocol, "100", "sh"},
		{Arg, Protocol, "-1", "--", "sh"},
	} {
		if _, err := parse(bad); err == nil {
			t.Fatalf("parse(%q) accepted a malformed argv", bad)
		}
	}
}

func TestParseAck(t *testing.T) {
	cases := []struct {
		line          string
		supported, ok bool
	}{
		{AckStarted + "\n", true, true},
		{AckStartFailed + "exec: no such file\n", true, false},
		{"", false, false},
		{"Error: unknown command \"__watch-lifeline\" for \"af\"\n", false, false},
		{"v2 started\n", false, false},
	}
	for _, c := range cases {
		supported, ok, _ := ParseAck(c.line)
		if supported != c.supported || ok != c.ok {
			t.Errorf("ParseAck(%q) = %v,%v, want %v,%v", c.line, supported, ok, c.supported, c.ok)
		}
	}
}
