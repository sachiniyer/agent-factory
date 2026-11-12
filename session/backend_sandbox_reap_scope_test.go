package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The scope of runCommand's process-group reap (#7108, #7115).
//
// A member of the step's group that holds none of its pipes is not a leak by
// itself. Under ControlMaster/ControlPersist, OpenSSH detaches the master into
// its own session but leaves the ProxyCommand (or ProxyJump's `ssh -W`) that
// carries it in the step's group, with stdin and stdout on pipes to the master
// and stderr on /dev/null. A successful step therefore returns with the
// master's live transport still in the group. Killing it kills the master:
// every later step re-authenticates, concurrent mux-client steps fail with
// exit 255, and forwards attached to the master die.
//
// The member these tests plant has the same shape: a `sleep` in the group with
// stdin, stdout and stderr all on /dev/null. A successful step must leave it
// alive; a timed-out step must still kill it, because a deadline tears down the
// whole group through KillOnCancel.

// detachedMemberStub is an `ssh` stub that plants one such member.
type detachedMemberStub struct {
	ssh     string
	pidFile string
}

// stubSSHDetachedMember writes an `ssh` stub that backgrounds `sleep 30` with
// all three stdio streams on /dev/null and records three numbers: the sleep's
// pid, the sleep's process group, and the stub's own process group. It then
// prints a marker and runs then: "exit 0" for a step that succeeds at once,
// or a long sleep for one still running at the deadline.
func stubSSHDetachedMember(t *testing.T, then string) detachedMemberStub {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "member.pid")
	body := "#!/usr/bin/env bash\n" +
		"sleep 30 </dev/null >/dev/null 2>&1 &\n" +
		"member=$!\n" +
		`printf '%s %s %s\n' "$member" "$(ps -o pgid= -p "$member" | tr -d ' ')" "$(ps -o pgid= -p $$ | tr -d ' ')" > ` +
		shellQuote(pidFile) + "\n" +
		"printf 'AF-SANDBOX-ANSWER\\n'\n" +
		then + "\n"
	sshPath := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(sshPath, []byte(body), 0o755))
	return detachedMemberStub{ssh: sshPath, pidFile: pidFile}
}

// member reads what the stub recorded and proves the planted sleep was in the
// step's process group, the one Lead created: the sleep shares the stub's
// group, and that group is not this test's. Without that proof a sleep that
// left the group would make the survival test pass without testing anything.
//
// It also takes the sleep's identity while the pid is fresh and registers a
// cleanup that SIGKILLs that instance, so the sleep never outlives the test.
// proctree.Signal re-checks the identity first, so a recycled pid is never hit.
// A sleep already gone when this runs has nothing to clean up.
func (s detachedMemberStub) member(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(s.pidFile)
	require.NoError(t, err, "the stub must have recorded the planted member")
	fields := strings.Fields(string(data))
	require.Len(t, fields, 3, "the stub records the member's pid, its pgid, and the stub's pgid: %q", data)
	nums := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		require.NoError(t, err, "field %d of %q", i, data)
		require.Positive(t, n)
		nums[i] = n
	}
	pid, memberPgid, stubPgid := nums[0], nums[1], nums[2]

	if proc, err := proctree.Lookup(pid); err == nil {
		t.Cleanup(func() { _ = proctree.Signal(proc, syscall.SIGKILL) })
	}

	require.Equal(t, stubPgid, memberPgid,
		"the planted member must be in the step's process group, or nothing here tests the reap")
	require.NotEqual(t, syscall.Getpgrp(), stubPgid,
		"the step must lead its own process group (execgroup.Lead), not share the test's")
	return pid
}

// processGone reports whether pid has exited: it is no longer in the process
// table, or it is a zombie, which proctree reports as ErrProcessExited on both
// Linux and darwin. A zombie counts as gone because nothing guarantees a prompt
// collection. A container whose PID 1 does not reap orphans keeps the
// SIGKILLed sleep as a zombie, and kill(pid, 0) still succeeds on it. Any other
// read error means "cannot tell" and reports false, so an unreadable process
// table fails a test that expects an exit instead of passing it.
func processGone(pid int) bool {
	_, err := proctree.Lookup(pid)
	return errors.Is(err, proctree.ErrProcessExited) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
}

// waitProcessGone polls processGone until it holds or timeout passes. A
// SIGKILLed sleep exits within milliseconds, so a few seconds is ample, and it
// is far shorter than the 30 s the sleep lives if nothing kills it.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if processGone(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sandboxDetachedMemberSettle is how long the survival test watches the planted
// member after the step returns. A SIGKILL that kill(2) has already queued
// takes effect within milliseconds, so a reap that reached the member shows up
// well inside this window.
const sandboxDetachedMemberSettle = 1 * time.Second

// TestSandboxRunLeavesADetachedGroupMemberAliveOnSuccess is the inverse of
// TestSandboxRunReapsStragglerOnNormalCompletion. A step that succeeds while a
// member of its group holds none of its pipes must leave that member running.
// That is the ControlPersist master's ProxyCommand, and killing it kills the
// master. An unconditional reap on every successful exit fails this test. The
// reap belongs only to the exit that proves a member held the capture pipe past
// WaitDelay.
func TestSandboxRunLeavesADetachedGroupMemberAliveOnSuccess(t *testing.T) {
	stub := stubSSHDetachedMember(t, "exit 0")
	p := &sandboxProvisioner{sshCmd: stub.ssh}

	var (
		out []byte
		err error
	)
	start := time.Now()
	require.True(t, returnsWithinExec(sandboxOrphanGuard, func() {
		out, err = p.Run(5*time.Second, "true", nil, true)
	}), "runCommand must return once the stub exits")
	elapsed := time.Since(start)
	pid := stub.member(t)

	require.NoError(t, err, "a step that exits 0 succeeds")
	assert.Contains(t, string(out), "AF-SANDBOX-ANSWER")
	// No member holds the capture pipe, so Wait returns as soon as the stub
	// exits: no exec.ErrWaitDelay, which is exactly what the ControlPersist proxy
	// produces on OpenSSH 8.0 and later. If this ever took WaitDelay, the stub
	// would no longer model that shape.
	require.Less(t, elapsed, sandboxTunnelWaitDelay,
		"a member with its stdio on /dev/null must not hold the step past WaitDelay")

	proc, lerr := proctree.Lookup(pid)
	require.NoError(t, lerr,
		"the detached group member must still be running when runCommand returns. A successful step must not reap a member that holds none of its pipes (a ControlPersist master's ProxyCommand)")
	settle := time.Now().Add(sandboxDetachedMemberSettle)
	for time.Now().Before(settle) {
		require.True(t, proctree.AliveSame(proc),
			"the detached group member died after a successful step. Something signalled the group on the success path, which kills a ControlPersist master's transport")
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSandboxRunStillKillsADetachedGroupMemberOnTimeout pins the other side of
// that line. The survival above is a property of the success path only. A step
// still running at its deadline is torn down as a whole group by KillOnCancel,
// and that includes a member holding none of its pipes. A change that narrowed
// the deadline kill along with the success-path reap would leave a timed-out
// clone's helpers running against a workspace that cleanup is about to delete.
func TestSandboxRunStillKillsADetachedGroupMemberOnTimeout(t *testing.T) {
	stub := stubSSHDetachedMember(t, "sleep 10")
	p := &sandboxProvisioner{sshCmd: stub.ssh}

	var err error
	require.True(t, returnsWithinExec(sandboxOrphanGuard, func() {
		_, err = p.Run(1*time.Second, "true", nil, true)
	}), "a timed-out step must be killed at its deadline")
	pid := stub.member(t)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a deadline kill surfaces as context.DeadlineExceeded: %v", err)
	assert.True(t, waitProcessGone(pid, 3*time.Second),
		"the deadline must kill every member of the step's group, including one that holds none of its pipes")
}
