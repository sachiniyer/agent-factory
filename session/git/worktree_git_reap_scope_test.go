package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

// What git's runner may kill once git has exited on its own (#7121).
//
// A fetch over ssh with ControlMaster/ControlPersist leaves a process in git's
// group that holds none of git's pipes: OpenSSH detaches the master into its
// own session but keeps the master's ProxyCommand (or ProxyJump's `ssh -W`) in
// the caller's group as its transport, with stderr on /dev/null. Killing it
// kills the master, so every later fetch re-authenticates. A transport child
// still holding git's capture pipe past gitWaitDelay is the opposite case: the
// leak the runner exists to end (#769). These tests plant one of each through a
// fake ssh and drive the real runner.

// plantedMember is one process the fake ssh started and recorded.
type plantedMember struct {
	pid, pgid, sshPgid int
}

// fakeSSHPlanting writes a GIT_SSH_COMMAND that runs plant, records one
// "<pid> <pgid> <ssh's pgid>" line per planted process into a file, and then
// acts as the transport by running the remote command (git-upload-pack) on
// this machine.
func fakeSSHPlanting(t *testing.T, plant string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "planted")
	body := "#!/bin/sh\n" +
		// git first runs `ssh -G` to tell OpenSSH from a simple ssh. Fail that
		// probe before planting anything, so each fetch plants exactly once.
		"[ \"$1\" = -G ] && exit 1\n" +
		"record() { echo \"$1 $(ps -o pgid= -p $1 | tr -d ' ') $(ps -o pgid= -p $$ | tr -d ' ')\" >> '" + record + "'; }\n" +
		plant + "\n" +
		"for last; do :; done\n" +
		"exec sh -c \"$last\"\n"
	ssh := filepath.Join(dir, "fake-ssh")
	require.NoError(t, os.WriteFile(ssh, []byte(body), 0o755))
	return ssh, record
}

// Both plants record their own pid. detached has every stdio stream on
// /dev/null: the ControlPersist proxy's shape. holding inherits stderr, which
// git hands ssh, and that is the runner's capture pipe.
const (
	plantDetached = "sleep 60 </dev/null >/dev/null 2>&1 & record $!"
	plantHolding  = "sleep 60 </dev/null >/dev/null & record $!"
)

// readPlanted reads what the fake ssh recorded, proves each process was in
// git's group (it shares ssh's group, which is not the test's), and registers
// a cleanup that kills each instance by identity.
func readPlanted(t *testing.T, record string) []plantedMember {
	t.Helper()
	data, err := os.ReadFile(record)
	require.NoError(t, err, "the fake ssh must have run and recorded what it planted")
	var ms []plantedMember
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.Fields(line)
		require.Len(t, f, 3, "planted record line %q", line)
		var n [3]int
		for i := range f {
			v, err := strconv.Atoi(f[i])
			require.NoError(t, err)
			require.Positive(t, v)
			n[i] = v
		}
		m := plantedMember{pid: n[0], pgid: n[1], sshPgid: n[2]}
		if p, err := proctree.Lookup(m.pid); err == nil {
			t.Cleanup(func() { _ = proctree.Signal(p, syscall.SIGKILL) })
		}
		require.Equal(t, m.sshPgid, m.pgid, "the planted process must be in git's process group, or nothing here tests the reap")
		require.NotEqual(t, syscall.Getpgrp(), m.pgid, "git must lead its own process group (execgroup.Lead), not share the test's")
		ms = append(ms, m)
	}
	return ms
}

func memberGone(pid int) bool {
	_, err := proctree.Lookup(pid)
	return errors.Is(err, proctree.ErrProcessExited) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func waitMemberGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !memberGone(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// requireMemberSurvives fails unless pid is the same live process for a
// settle window after the runner returned. A queued SIGKILL lands within
// milliseconds, so a reap that reached it shows up well inside the window.
func requireMemberSurvives(t *testing.T, pid int, why string) {
	t.Helper()
	p, err := proctree.Lookup(pid)
	require.NoError(t, err, "pid %d was not running when the runner returned: %s", pid, why)
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		require.True(t, proctree.AliveSame(p), "pid %d died after the runner returned: %s", pid, why)
		time.Sleep(20 * time.Millisecond)
	}
}

// fetchThrough runs `git fetch origin` through the real network runner, with
// origin reached over ssh, the transport being the fake.
func fetchThrough(t *testing.T, ssh string) (time.Duration, error) {
	t.Helper()
	sandboxHome(t)
	origin := createGitRepo(t)
	runGit(t, origin, "commit", "--allow-empty", "-m", "initial")
	repoRoot := createGitRepo(t)
	t.Setenv("GIT_SSH_COMMAND", ssh)
	runGit(t, repoRoot, "remote", "add", "origin", "ssh://fake-host.invalid"+origin)
	start := time.Now()
	out, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
	if err != nil {
		t.Logf("fetch output: %s", out)
	}
	return time.Since(start), err
}

func TestGitFetchLeavesAMemberHoldingNoPipeAlive(t *testing.T) {
	ssh, record := fakeSSHPlanting(t, plantDetached)
	elapsed, err := fetchThrough(t, ssh)
	require.NoError(t, err, "the fetch succeeds")
	ms := readPlanted(t, record)
	require.Len(t, ms, 1)
	require.Less(t, elapsed, gitWaitDelay, "a member holding none of git's pipes must not hold the fetch for gitWaitDelay")
	requireMemberSurvives(t, ms[0].pid,
		"a member of git's group that holds none of its pipes was killed after a successful fetch; under ControlPersist that is the master's transport")
}

func TestGitFetchReapsOnlyTheMemberHoldingThePipe(t *testing.T) {
	ssh, record := fakeSSHPlanting(t, plantDetached+"\n"+plantHolding)
	_, err := fetchThrough(t, ssh)
	require.NoError(t, err, "git exited 0, so a pipe held past gitWaitDelay is not a failure")
	ms := readPlanted(t, record)
	require.Len(t, ms, 2)
	detached, holding := ms[0], ms[1]
	require.True(t, waitMemberGone(holding.pid, 10*time.Second),
		"the transport child holding git's capture pipe past gitWaitDelay must be reaped (#769)")
	requireMemberSurvives(t, detached.pid,
		"the member holding none of git's pipes was killed along with the straggler; a group reap does that")
}

// A fetch that fails on its own is that failure, not a timeout, even when the
// deadline passes while cleanup runs (#7243). Here the transport refuses
// authentication at once, git exits 128, and a transport child keeps git's
// stderr past gitWaitDelay, so the deadline passes during WaitDelay and the
// straggler reap. Keyed on ctx.Err(), that read "timed out (remote unreachable
// or stalled)" and dropped git's error, stderr and all.
func TestGitFetchThatFailsOnItsOwnIsNotATimeout(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "planted")
	ssh := filepath.Join(dir, "fake-ssh")
	require.NoError(t, os.WriteFile(ssh, []byte("#!/bin/sh\n"+
		"[ \"$1\" = -G ] && exit 1\n"+
		"sleep 60 </dev/null >/dev/null & echo \"$! $(ps -o pgid= -p $! | tr -d ' ') $(ps -o pgid= -p $$ | tr -d ' ')\" >> '"+record+"'\n"+
		"echo 'git@fake-host.invalid: Permission denied (publickey).' >&2\n"+
		"exit 255\n"), 0o755))

	sandboxHome(t)
	repoRoot := createGitRepo(t)
	t.Setenv("GIT_SSH_COMMAND", ssh)
	runGit(t, repoRoot, "remote", "add", "origin", "ssh://fake-host.invalid/repo.git")
	shortenNetworkTimeout(t, 500*time.Millisecond)
	require.Less(t, networkGitTimeout, gitWaitDelay, "the deadline must fall inside gitWaitDelay for this test to mean anything")

	_, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
	readPlanted(t, record)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "timed out", "git failed on its own before its deadline")
	assert.False(t, errors.Is(err, context.DeadlineExceeded), "nor may it read as a deadline: %v", err)
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr), "git's own failure is the error: %v", err)
	assert.Equal(t, 128, exitErr.ExitCode())
	assert.Contains(t, err.Error(), "Permission denied", "git's stderr rides on its error")
}

// The other side: a fetch the deadline really ended still reads as a timeout,
// still satisfies errors.Is(err, context.DeadlineExceeded) for its callers,
// and now keeps git's own error in the chain too.
func TestGitFetchEndedByItsDeadlineIsATimeout(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	runGit(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	stallOriginViaFakeSSH(t, repoRoot)
	shortenNetworkTimeout(t, 500*time.Millisecond)

	_, err := (&GitWorktree{}).runGitNetworkCommand(repoRoot, "fetch", "origin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "callers test for the deadline: %v", err)
	var exitErr *exec.ExitError
	assert.True(t, errors.As(err, &exitErr), "git's own error stays in the chain: %v", err)
}
