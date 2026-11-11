package execgroup

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLeadOwnsItsGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "true")
	Lead(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Lead must start the command in its own process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Lead must install the group-kill Cancel")
	}
}

// A group already gone maps to os.ErrProcessDone — the error Wait swallows —
// rather than surfacing as a command failure.
func TestCancelMapsGoneGroupToProcessDone(t *testing.T) {
	cmd := exec.Command("true")
	KillOnCancel(cmd)
	// A pid larger than any platform's maximum names a group that cannot
	// exist, so the signal answers ESRCH deterministically.
	proc, err := os.FindProcess(1 << 23)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	cmd.Process = proc
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel on a gone group = %v, want os.ErrProcessDone", err)
	}
}

// A command that never started has no group to kill: both halves are no-ops,
// not panics on a nil Process.
func TestNilProcessIsNoOp(t *testing.T) {
	cmd := exec.Command("true")
	KillOnCancel(cmd)
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel with nil Process = %v, want nil", err)
	}
	Reap(cmd)
}

// killBound is how long a test waits for a kill to land. A SIGKILLed process
// closes its descriptors within milliseconds, so this is pure slack for a
// loaded or -race runner. It must stay well below holderLife, or a broken kill
// could pass by simply outliving the bound.
const killBound = 10 * time.Second

// holderLife is how long every process a test starts would live on its own.
// Only a working kill ends them inside killBound. forkingGroup's loops do
// not end on their own, so its watchdog ends them after holderLife, and what
// they forked ends holderLife after that.
const holderLife = "60"

// groupHolders is a command whose whole process group shares one pipe.
type groupHolders struct {
	cmd  *exec.Cmd
	pgid int
	// eof closes once every holder of the pipe's write end has exited.
	eof <-chan struct{}
}

// startGroupHolders runs script under sh as the leader of its own group
// (Lead), with stdout on a pipe the test holds only the read end of. Every
// process the script forks inherits that stdout, so the pipe reaches EOF
// exactly when the last of them has exited.
//
// That EOF is the observation, rather than polling kill(-pgid, 0) for ESRCH,
// because a SIGKILLed process closes its descriptors as it dies, while it
// stays a member of its group until something reaps it. An orphaned
// grandchild is reaped by PID 1 or the nearest subreaper, which a test does
// not control and a container's PID 1 may never do.
//
// The script must print "ready" once it has forked what it means to test, so
// a kill cannot win the race against the fork and leave nothing behind for a
// broken kill to miss.
func startGroupHolders(t *testing.T, ctx context.Context, script string) groupHolders {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	Lead(cmd)
	cmd.Stdout = w
	// Bounds Wait the way real callers do, should Cancel leave the leader up.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		t.Fatalf("Start: %v", err)
	}
	// Only the script's processes may hold the write end, or EOF never comes.
	_ = w.Close()

	ready := make(chan struct{})
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		defer func() { _ = r.Close() }()
		br := bufio.NewReader(r)
		if line, err := br.ReadString('\n'); err == nil && line == "ready\n" {
			close(ready)
		}
		_, _ = io.Copy(io.Discard, br)
	}()

	h := groupHolders{cmd: cmd, pgid: cmd.Process.Pid, eof: eof}
	select {
	case <-ready:
		return h
	case <-eof:
		_ = cmd.Wait()
		t.Fatalf("script %q exited before it printed ready", script)
	case <-time.After(killBound):
		// Not yet waited, so the leader's pid still names this test's group.
		killOwnGroup(t, h.pgid)
		_ = cmd.Wait()
		t.Fatalf("script %q did not print ready within %v", script, killBound)
	}
	return h
}

// killOwnGroup SIGKILLs the group the test started. Call it only while the
// group provably still exists — before the leader is waited, or while a
// member still holds the pipe — so the pgid cannot have been reused by now.
// It sends the one plain kill a test can vouch for, not the code under test.
// darwin answers EPERM, not ESRCH, for a group holding only zombies (#7072),
// and a test's own group holds no process it may not signal, so EPERM means
// the same as ESRCH here.
func killOwnGroup(t *testing.T, pgid int) {
	t.Helper()
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		t.Errorf("cleanup kill of the test's own group %d: %v", pgid, err)
	}
}

// killHeldGroup kills the group of a leader that has already been waited,
// when a member still holds the pipe. A member holding it keeps the group,
// and so its pgid, alive. Once the pipe is released there is nothing left to
// kill, and the pgid may already name someone else's group.
func killHeldGroup(t *testing.T, h groupHolders) {
	t.Helper()
	select {
	case <-h.eof:
	default:
		killOwnGroup(t, h.pgid)
	}
}

// Cancel must reach the whole group, not just the direct child: a grandchild
// the child forked into its group dies with it. A Cancel that kills only
// cmd.Process, or nothing at all, leaves the grandchild holding the pipe past
// killBound.
//
// Where sh is bash (macOS), the shell forks the foreground sleep just after
// it prints ready, so the cancel often lands mid-fork. On darwin a single
// kill(-pgid) missed that child about half the time (#7066), which
// TestCancelKillsAChildTheGroupIsForking pins down deliberately.
func TestCancelKillsAGrandchildInTheGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startGroupHolders(t, ctx,
		"sleep "+holderLife+" & echo ready; sleep "+holderLife)

	cancel()
	select {
	case <-h.eof:
	case <-time.After(killBound):
		t.Errorf("a process in the group led by %d still holds the pipe %v after cancel: Cancel did not reach every member of the group", h.pgid, killBound)
		killOwnGroup(t, h.pgid)
	}
	_ = h.cmd.Wait()
}

// Reap must kill a group member that outlived the leader — a backgrounded
// straggler still holding an inherited pipe — after the leader has exited and
// been waited. A Reap that kills only cmd.Process (already done), or nothing
// at all, leaves the straggler holding the pipe past killBound.
func TestReapKillsAStragglerHoldingThePipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := startGroupHolders(t, ctx, "sleep "+holderLife+" & echo ready; exit 0")

	waited := make(chan error, 1)
	go func() { waited <- h.cmd.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			killHeldGroup(t, h)
			t.Fatalf("the leader should exit 0 on its own: %v", err)
		}
	case <-time.After(killBound):
		// Still unwaited, so the leader's pid still names this test's group.
		killOwnGroup(t, h.pgid)
		<-waited
		t.Fatalf("the leader did not exit within %v", killBound)
	}
	select {
	case <-h.eof:
		t.Fatal("the straggler released the pipe before Reap ran, so this test proves nothing")
	default:
	}

	Reap(h.cmd)
	select {
	case <-h.eof:
	case <-time.After(killBound):
		t.Errorf("the straggler in the group led by %d still holds the pipe %v after Reap", h.pgid, killBound)
		// The straggler is alive, so the group and its pgid are still ours.
		killOwnGroup(t, h.pgid)
	}
}

// forkingGroup is a script whose group is always forking: four background
// loops, each forking a pipe holder and killing it again. A group kill lands
// mid-fork in most trials. That is the instant at which XNU's kill(-pgid)
// misses the new child, which then runs on in the group holding the pipe
// (#7066).
//
// The loops never end on their own, and they run in their own group, which
// Ctrl-C does not reach. So the script's first fork is a watchdog that
// SIGKILLs the whole group (kill 0) after holderLife. An interrupted or
// timed-out test binary then leaves nothing running for longer than that,
// plus holderLife for a sleep the watchdog's own kill missed mid-fork.
func forkingGroup(then string) string {
	return "(sleep " + holderLife + "; kill -9 0) & " +
		"for i in 1 2 3 4; do (while :; do sleep " + holderLife +
		" & p=$!; kill $p; wait $p; done) & done; echo ready; " + then
}

// forkTrials is how many times each fork-race test repeats. On macos-15,
// without killGroup's re-signal, a single group kill left a forkingGroup
// child holding the pipe in 20 of 20 trials, and both fork-race tests failed
// on their first trial (#7069's probe runs). The repeats cover a slower or
// faster runner, where the race lands less often. A trial that passes costs
// milliseconds.
const forkTrials = 20

// Cancel must reach a child the group is forking at the moment it lands.
func TestCancelKillsAChildTheGroupIsForking(t *testing.T) {
	for i := 0; i < forkTrials; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		h := startGroupHolders(t, ctx, forkingGroup("wait"))
		cancel()
		select {
		case <-h.eof:
		case <-time.After(killBound):
			t.Errorf("trial %d: a process in the group led by %d still holds the pipe %v after cancel: Cancel missed a child the group was forking", i, h.pgid, killBound)
			killOwnGroup(t, h.pgid)
		}
		_ = h.cmd.Wait()
		if t.Failed() {
			return
		}
	}
}

// Reap must reach a child that a straggler is forking at the moment it lands.
func TestReapKillsAChildAStragglerIsForking(t *testing.T) {
	for i := 0; i < forkTrials; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		h := startGroupHolders(t, ctx, forkingGroup("exit 0"))
		waited := make(chan error, 1)
		go func() { waited <- h.cmd.Wait() }()
		select {
		case err := <-waited:
			if err != nil {
				killHeldGroup(t, h)
				cancel()
				t.Fatalf("trial %d: the leader should exit 0 on its own: %v", i, err)
			}
		case <-time.After(killBound):
			killOwnGroup(t, h.pgid)
			<-waited
			cancel()
			t.Fatalf("trial %d: the leader did not exit within %v", i, killBound)
		}
		select {
		case <-h.eof:
			cancel()
			t.Fatalf("trial %d: the stragglers released the pipe before Reap ran, so this test proves nothing", i)
		default:
		}
		Reap(h.cmd)
		select {
		case <-h.eof:
		case <-time.After(killBound):
			t.Errorf("trial %d: a process in the group led by %d still holds the pipe %v after Reap: Reap missed a child a straggler was forking", i, h.pgid, killBound)
			// A member holds the pipe, so the group and its pgid are still ours.
			killOwnGroup(t, h.pgid)
		}
		cancel()
		if t.Failed() {
			return
		}
	}
}
