package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The #5188 publication contract this PR carries: daemon.pid is written before
// the control socket can serve its first Ping (a successful readiness probe
// structurally implies the file exists), and a daemon that cannot publish its
// identity fails closed — it must not serve while invisible to StopDaemon,
// Health, `af daemon status`, and doctor. Health's PIDVerified is bound to
// THIS home by classifyDaemonHome, the same binding StopDaemon requires.
// These tests drive RunDaemon in-process — the daemon is real but
// unprivileged, the home is a temp dir, and no tmux or supervisor is
// involved.

// joinTestDaemon registers the cleanup that guarantees a spawned RunDaemon
// goroutine is dead before this test's other cleanups restore package-level
// stubs (the legacy-unit sweep globals, the instant backend) the still-running
// daemon reads while starting up. Registered immediately after the goroutine
// starts so even a mid-test failure cannot leak it into the next test —
// the macOS run showed exactly that: an early assertion left the daemon
// starting behind a restored stub, and the race-detector fired in the NEXT
// test's cleanup.
//
// runDone must be the channel the spawning goroutine CLOSES after delivering
// RunDaemon's result (send-then-close): a drained-but-open channel would look
// alive to the cleanup and cost a 10s false timeout on the happy path, while
// a closed one stays readable forever and answers the "did it exit?" check
// regardless of who consumed the result.
func joinTestDaemon(t *testing.T, runDone <-chan error) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-runDone:
			return // exited — the send-then-close keeps this readable post-drain
		default:
		}
		_, _, _ = RequestShutdown()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("RunDaemon did not exit within 10s of Shutdown during cleanup")
		}
	})
}

// readPIDFilePID parses the PID daemon.pid currently names, returning ok=false
// when the file is absent or malformed.
func readPIDFilePID(t *testing.T, pidPath string) (int, bool) {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// TestRunDaemon_SymlinkedPIDFileFailsClosed pins the fail-closed half of #5188:
// a daemon that cannot write daemon.pid would serve while invisible to
// StopDaemon, `af daemon status`, and doctor — so it must not serve at all.
// The error is actionable (names the path, the cause, and the remedy — the
// message text is pinned here) and reaches the caller; persisting it for
// status/doctor is deferred to #5196.
func TestRunDaemon_SymlinkedPIDFileFailsClosed(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	installInstantBackend(t)
	stubLegacyUnitSweep(t)

	pidPath := filepath.Join(home, "daemon.pid")
	target := filepath.Join(t.TempDir(), "planted-target")
	require.NoError(t, os.WriteFile(target, []byte("1"), 0600))
	require.NoError(t, os.Symlink(target, pidPath))

	cfg := config.DefaultConfig()
	cfg.DaemonPollInterval = 50
	runDone := make(chan error, 1)
	go func() {
		runDone <- RunDaemon(cfg)
		close(runDone)
	}()
	joinTestDaemon(t, runDone)
	var runErr error
	select {
	case runErr = <-runDone:
	case <-time.After(10 * time.Second):
		// Pre-fix RunDaemon logged the refusal and kept serving invisibly —
		// that is the bug. The cleanup join reaps it; fail below.
		t.Fatalf("RunDaemon kept serving with a symlinked daemon.pid — a daemon that cannot publish its PID must fail closed")
	}
	require.Error(t, runErr, "a symlinked daemon.pid must abort startup, not log-and-continue")
	assert.ErrorIs(t, runErr, config.ErrManagedFileSymlink,
		"the refusal must keep ErrManagedFileSymlink identity for callers that classify it")
	for _, want := range []string{pidPath, "symlink", "remove the link"} {
		assert.Contains(t, runErr.Error(), want,
			"the fail-closed message must name the path, the cause, and the remedy")
	}

	// The daemon that failed closed must not be answering; the link itself
	// survives (af neither writes through nor unlinks it).
	if err := pingDaemon(); err == nil {
		t.Error("a daemon that failed closed on daemon.pid must not keep serving the control socket")
	}
	info, lerr := os.Lstat(pidPath)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the planted symlink must survive — af never unlinks a file it would not write through")
}

// TestWriteDaemonPIDFile_RetriesTransientLockContention pins the in-process
// retry: a stop's read-compare-unlink holds daemon.pid.lock only briefly, so a
// contended first attempt must be retried rather than handed to the
// supervisor's StartLimitBurst budget (#5188 review). The lock is held past
// the first attempt's deadline and released before the second, so success here
// is proof a retry ran — under a single-attempt write this fails.
func TestWriteDaemonPIDFile_RetriesTransientLockContention(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	origBudget, origPoll, origPause := daemonPIDLockStartupBudget, daemonPIDLockPoll, daemonPIDWriteRetryPause
	daemonPIDLockStartupBudget = 100 * time.Millisecond
	daemonPIDLockPoll = 5 * time.Millisecond
	daemonPIDWriteRetryPause = 20 * time.Millisecond
	t.Cleanup(func() {
		daemonPIDLockStartupBudget, daemonPIDLockPoll, daemonPIDWriteRetryPause = origBudget, origPoll, origPause
	})

	pidFile := filepath.Join(home, "daemon.pid")
	held, err := os.OpenFile(pidFile+".lock", os.O_CREATE|os.O_RDWR, 0644)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(held.Fd()), syscall.LOCK_EX))
	// Release only after the first attempt's whole budget has lapsed, so a
	// single-attempt write cannot succeed — only a retry can. The second
	// attempt's window runs to ~220ms, leaving generous slack over the
	// scheduled 150ms release for a loaded CI runner.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = held.Close()
	}()

	require.NoError(t, writeDaemonPIDFile(),
		"a transiently contended daemon.pid.lock must be retried in-process, not spent on a supervisor restart")
	pid, ok := readPIDFilePID(t, pidFile)
	require.True(t, ok)
	assert.Equal(t, os.Getpid(), pid)
}

// TestHealth_PIDVerifiedRequiresOwnHome pins the #5188 follow-up from the
// #5017 lane: PIDVerified must carry the same home binding StopDaemon requires
// (classifyDaemonHome), not just liveness + cmdline. A stale daemon.pid whose
// number was recycled onto ANOTHER home's live `af --daemon` is not this
// home's daemon — reporting it verified would name a foreign daemon in "kill
// N" hints.
func TestHealth_PIDVerifiedRequiresOwnHome(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	otherHome := testguard.SocketTempDir(t)

	// A live `af --daemon` argv whose AGENT_FACTORY_HOME names a DIFFERENT
	// home — the recycled-PID shape the file would point at.
	cmd := fakeDaemonCmd(t, "af", "sleep 60; :", "--daemon", "af-test")
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AGENT_FACTORY_HOME=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "AGENT_FACTORY_HOME="+otherHome)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	waitForReady(t, fmt.Sprintf("fake daemon pid=%d cmdline exposes --daemon", pid), func() bool {
		return isAgentFactoryDaemon(pid)
	})

	pidPath := filepath.Join(home, "daemon.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0600))

	h := Health()
	require.Equal(t, pid, h.PIDFilePID)
	assert.False(t, h.PIDVerified,
		"a daemon.pid naming a live af daemon that serves ANOTHER home is not this home's verified daemon")
	assert.False(t, h.PIDUnverifiable,
		"a PROVEN foreign daemon is stale for this home, not inconclusive — only an unbound home earns PIDUnverifiable")
}

// TestHealth_PIDUnverifiableIsNotAbsent pins the third verdict of
// classifyDaemonHome: a pid file naming a LIVE af daemon whose home binding
// is inconclusive (an AGENT_FACTORY_HOME the classifier cannot resolve —
// here the raw "~user" form — or, on macOS, a peer environ nobody may read)
// must surface as PIDUnverifiable, not collapse into unverified-and-stale.
// Unverified-but-live is not absence: doctor must not call the file stale
// over it, and upgrade must not print an all-clear.
func TestHealth_PIDUnverifiableIsNotAbsent(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// A live `af --daemon` argv whose AGENT_FACTORY_HOME is the raw "~user"
	// form: classifyDaemonHome cannot resolve it (config.ConfigDirFor
	// rejects "~user"), so the binding is daemonUnverifiable rather than
	// foreign — the process is a real af daemon, its home merely unproven.
	cmd := fakeDaemonCmd(t, "af", "sleep 60; :", "--daemon", "af-test")
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "AGENT_FACTORY_HOME=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "AGENT_FACTORY_HOME=~no-such-user-5188")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	waitForReady(t, fmt.Sprintf("fake daemon pid=%d cmdline exposes --daemon", pid), func() bool {
		return isAgentFactoryDaemon(pid)
	})

	pidPath := filepath.Join(home, "daemon.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0600))

	h := Health()
	require.Equal(t, pid, h.PIDFilePID)
	assert.False(t, h.PIDVerified,
		"an unproven home binding must never be reported as verified")
	assert.True(t, h.PIDUnverifiable,
		"a live af daemon whose home cannot be bound is PIDUnverifiable — inconclusive, not absent")
}

// TestReadManagedFileNoFollow_RefusesNonManagedShapes pins the descriptor-
// validated read every daemon.pid read now uses: a symlinked, non-regular, or
// oversized pid file is not something af wrote — a FIFO could block a
// lock-holding teardown read forever, and a swapped symlink must never be
// followed.
func TestReadManagedFileNoFollow_RefusesNonManagedShapes(t *testing.T) {
	home := testguard.SocketTempDir(t)
	pidFile := filepath.Join(home, "daemon.pid")

	// Symlink through to a planted file: the target's content must not surface.
	target := filepath.Join(home, "planted.txt")
	require.NoError(t, os.WriteFile(target, []byte("99999"), 0600))
	require.NoError(t, os.Symlink(target, pidFile))
	_, ok := readManagedFileNoFollow(pidFile, daemonPIDFileMaxBytes)
	assert.False(t, ok, "a symlinked pid file must not read through to its target")

	// Non-regular: a FIFO at the path could block os.ReadFile forever.
	require.NoError(t, os.Remove(pidFile))
	require.NoError(t, syscall.Mkfifo(pidFile, 0600))
	_, ok = readManagedFileNoFollow(pidFile, daemonPIDFileMaxBytes)
	assert.False(t, ok, "a non-regular pid file fails the descriptor check without blocking")

	// Oversized: a pid file is a few bytes; a planted large file is refused.
	require.NoError(t, os.Remove(pidFile))
	require.NoError(t, os.WriteFile(pidFile, make([]byte, daemonPIDFileMaxBytes+1), 0600))
	_, ok = readManagedFileNoFollow(pidFile, daemonPIDFileMaxBytes)
	assert.False(t, ok, "an oversized pid file is not something af wrote")

	// A real pid file still reads — the policy refuses shapes, not content.
	require.NoError(t, os.WriteFile(pidFile, []byte("12345\n"), 0600))
	data, ok := readManagedFileNoFollow(pidFile, daemonPIDFileMaxBytes)
	require.True(t, ok)
	assert.Equal(t, "12345\n", string(data))
}
