package session

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the fix for the symlinked-session-dir orphan bug.
//
// The agent-server re-execs via os.Executable(), which on Linux reads
// /proc/self/exe — the kernel-resolved (symlink-free) physical path. When a
// session-directory ancestor is a symlink (e.g. a symlinked $HOME), the running
// process's /proc/<pid>/cmdline argv[0] carries the RESOLVED spelling, while the
// mktemp-printed session dir carries the UNRESOLVED one. The reaper
// (remotePIDIdentityKillScript) compares its `expected` against that argv[0]: a
// mismatch makes it skip the kill, still rm -rf the dir, and latch success for an
// orphaned agent-server. Provision-time resolveSessionDirSymlinks captures the
// resolved path the kernel used and persists it, so the reaper's `expected`
// matches the running process's argv[0].
//
// The tests below exercise the mechanism end to end: the resolution method, the
// identity-kill script's match/no-match behavior against a live process whose
// argv[0] carries a resolved path, and the full reap flow's kill+remove+confirm.

// localRunCommandFn returns a runCommandFn that runs each script in a REAL local
// sh, so the resolveSessionDirSymlinks `cd … && pwd -P` mechanism and the
// identity-kill script's /proc/<pid>/cmdline lookup are exercised against the
// actual kernel rather than restated.
func localRunCommandFn() func(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return func(_ time.Duration, script string, _ io.Reader, combined bool) ([]byte, error) {
		cmd := exec.Command("sh", "-c", script)
		if combined {
			return cmd.CombinedOutput()
		}
		return cmd.Output()
	}
}

// startAgentServerLikeProcess starts a long-lived `sleep` process whose
// /proc/<pid>/cmdline argv[0] is set to argv0 — faithfully simulating what
// os.Executable() produces for the re-exec'd agent-server (the kernel resolves
// the executable path and exec sets that resolved path as argv[0]). The process
// is reaped on test completion if still alive.
func startAgentServerLikeProcess(t *testing.T, argv0 string) *exec.Cmd {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err, "sleep is needed to hold a live process for the identity-kill")
	cmd := exec.Command(sleep, "30")
	cmd.Args[0] = argv0
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	return cmd
}

// pidAlive reports whether pid is a running (non-zombie) process. It sends signal
// 0, which a live process accepts and an exited/missing one rejects. Callers that
// already reaped the process via Wait must NOT use this — a zombie is invisible
// here only because Wait already collected it.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// --- resolveSessionDirSymlinks ------------------------------------------------

// The resolution method resolves a symlinked session dir to its physical path
// using `cd … && pwd -P`, exactly as it does on the remote host. This runs against
// a REAL symlink on the local filesystem so the POSIX `pwd -P` mechanism is
// exercised rather than asserted.
func TestResolveSessionDirSymlinksResolvesASymlinkedPath(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "linked-home")
	require.NoError(t, os.Symlink(realRoot, linkRoot))
	sessionDir := filepath.Join(linkRoot, ".af-sessions", "abc")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	p := &sandboxProvisioner{runCommandFn: localRunCommandFn()}
	w := &sandboxWorkspace{shell: p, SessionDir: sessionDir}
	w.resolveSessionDirSymlinks(5 * time.Second)

	want := filepath.Join(realRoot, ".af-sessions", "abc")
	assert.Equal(t, want, w.SessionDir,
		"a symlinked session dir must be resolved to its physical path so the reaper's "+
			"`expected` matches the resolved argv[0] the re-exec produced")
	assert.NotEqual(t, sessionDir, w.SessionDir,
		"and the resolved spelling must differ from the unresolved one, or this fixture proves nothing")
}

// A non-symlinked path resolves to itself, so the resolution is a no-op for the
// common case (including the CI sshd image, whose /root is a real directory).
func TestResolveSessionDirSymlinksIsANoOpOnARealPath(t *testing.T) {
	sessionDir := t.TempDir()
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	p := &sandboxProvisioner{runCommandFn: localRunCommandFn()}
	w := &sandboxWorkspace{shell: p, SessionDir: sessionDir}
	w.resolveSessionDirSymlinks(5 * time.Second)

	assert.Equal(t, sessionDir, w.SessionDir,
		"a path with no symlinks must be unchanged — the fix must not alter behavior on the common case")
}

// The best-effort contract: a remote shell that errors (transport failure, a shell
// that lacks pwd -P) leaves the session dir as mktemp printed it, rather than
// introducing a new provision failure mode.
func TestResolveSessionDirSymlinksLeavesPathUnchangedOnShellError(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("shell does not support pwd -P"), os.ErrNotExist
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/root/.af-sessions/orig"}
	w.resolveSessionDirSymlinks(5 * time.Second)
	assert.Equal(t, "/root/.af-sessions/orig", w.SessionDir,
		"on resolution failure the session dir must stay as mktemp printed it")
}

func TestResolveSessionDirSymlinksLeavesPathUnchangedOnEmptyOutput(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, nil
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/root/.af-sessions/orig"}
	w.resolveSessionDirSymlinks(5 * time.Second)
	assert.Equal(t, "/root/.af-sessions/orig", w.SessionDir,
		"empty output is not a resolved path, so the session dir must stay unchanged")
}

// --- identity-kill against a live process --------------------------------------

// The fix's core claim: when the reaper's `expected` is the RESOLVED path (what the
// fix produces), the identity-kill script signals the running process whose argv[0]
// was set by os.Executable() to that same resolved path.
func TestRemotePIDIdentityKillScriptMatchesResolvedArgvZeroAndKills(t *testing.T) {
	resolvedAfPath := filepath.Join(t.TempDir(), "session", "af")
	require.NoError(t, os.MkdirAll(filepath.Dir(resolvedAfPath), 0o755))

	proc := startAgentServerLikeProcess(t, resolvedAfPath)
	pid := proc.Process.Pid
	require.True(t, pidAlive(pid), "premise: the process must be alive before the kill")

	script := remotePIDIdentityKillScript(strconv.Itoa(pid), resolvedAfPath)
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	require.NoError(t, err, "the identity-kill subshell exits 0 after a successful kill: %s", out)

	// Reap the process to check it was signalled. exec.Cmd.Wait wraps a signal
	// death in *exec.ExitError; os.Process.Wait does not, so Wait on the Cmd.
	waitErr := proc.Wait()
	require.Error(t, waitErr, "the process must have been signalled — the reaper's resolved "+
		"`expected` matched its argv[0]")
}

// When `expected` is the UNRESOLVED spelling (the bug), the same running process
// survives — the mismatch that orphaned the agent-server. The kill subshell exits 0
// (the "PID is now someone else's" branch) without ever signalling the process.
func TestRemotePIDIdentityKillScriptMismatchOnUnresolvedArgvZeroSkipsKill(t *testing.T) {
	realRoot := t.TempDir()
	resolvedAfPath := filepath.Join(realRoot, "session", "af")
	require.NoError(t, os.MkdirAll(filepath.Dir(resolvedAfPath), 0o755))
	// A symlinked spelling of the same path: what mktemp would have printed and what
	// the buggy reaper used as `expected`.
	linkRoot := filepath.Join(t.TempDir(), "linked-home")
	require.NoError(t, os.Symlink(realRoot, linkRoot))
	unresolvedAfPath := filepath.Join(linkRoot, "session", "af")

	proc := startAgentServerLikeProcess(t, resolvedAfPath)
	pid := proc.Process.Pid
	require.True(t, pidAlive(pid), "premise: the process must be alive")

	// The BUGGY reaper: expected = unresolved, running argv[0] = resolved -> mismatch.
	script := remotePIDIdentityKillScript(strconv.Itoa(pid), unresolvedAfPath)
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	require.NoError(t, err,
		"the identity-kill exits 0 WITHOUT killing when the PID is someone else's — the buggy branch: %s", out)

	assert.True(t, pidAlive(pid),
		"the process must SURVIVE when expected (unresolved) mismatches argv[0] (resolved) "+
			"— this is the orphan the bug produces")
}

// --- full reap flow: kill + remove + confirm -----------------------------------

// The resolved session dir flows from provision into reapScript, so the full
// reap (identity-kill + rm -rf + sentinel) kills the running agent-server, removes
// the directory, and prints the sentinel — against a live process whose argv[0]
// is the resolved path. This is the end-to-end proof the fix is wired.
func TestSandboxReapScriptKillsAndRemovesUsingResolvedSessionDir(t *testing.T) {
	resolvedDir := t.TempDir()
	resolvedAfPath := filepath.Join(resolvedDir, "af")
	require.NoError(t, os.WriteFile(resolvedAfPath, []byte("dummy"), 0o755))

	proc := startAgentServerLikeProcess(t, resolvedAfPath)
	pid := proc.Process.Pid

	p := &sandboxProvisioner{
		sessionDir: resolvedDir,
		remotePID:  strconv.Itoa(pid),
	}
	script, expect := p.reapScript("deadbeef")
	out, err := exec.Command("sh", "-c", script).CombinedOutput()

	require.NoError(t, err, "the reap must exit 0 after a clean kill+remove: %s", out)
	assert.Contains(t, string(out), expect,
		"the sentinel must be present — the far side ran and confirmed")
	_, statErr := os.Stat(resolvedDir)
	assert.True(t, os.IsNotExist(statErr),
		"the session dir must be removed after the reap")
	waitErr := proc.Wait()
	require.Error(t, waitErr,
		"the agent-server process must have been KILLED — the resolved `expected` matched its argv[0]")
}

// The BUG, restated as a reap-level failure: with the UNRESOLVED session dir (what
// the code produced before the fix), the same reap script exits 0 and prints the
// sentinel (so reap would latch success) while the agent-server process stays alive —
// the orphan. This test documents the pre-fix behavior the fix eliminates.
func TestSandboxReapScriptWithUnresolvedSessionDirOrphansTheProcess(t *testing.T) {
	realRoot := t.TempDir()
	resolvedDir := filepath.Join(realRoot, ".af-sessions", "s")
	require.NoError(t, os.MkdirAll(resolvedDir, 0o755))
	resolvedAfPath := filepath.Join(resolvedDir, "af")
	require.NoError(t, os.WriteFile(resolvedAfPath, []byte("dummy"), 0o755))
	// The symlink spelling — what mktemp printed before the fix and what the buggy
	// reaper persisted as `expected`.
	linkRoot := filepath.Join(t.TempDir(), "linked-home")
	require.NoError(t, os.Symlink(realRoot, linkRoot))
	unresolvedDir := filepath.Join(linkRoot, ".af-sessions", "s")

	proc := startAgentServerLikeProcess(t, resolvedAfPath)
	pid := proc.Process.Pid

	// Before the fix, p.sessionDir carried the unresolved spelling.
	p := &sandboxProvisioner{
		sessionDir: unresolvedDir,
		remotePID:  strconv.Itoa(pid),
	}
	script, expect := p.reapScript("a1b2c3d4")
	out, err := exec.Command("sh", "-c", script).CombinedOutput()

	// The reap LOOKS successful: exit 0 and the sentinel present.
	require.NoError(t, err,
		"the buggy reap exits 0 because the mismatched identity-kill subshell returns 0: %s", out)
	assert.Contains(t, string(out), expect,
		"and the sentinel is printed — so reap() would latch p.reaped=true")

	// The directory resolvable through the symlink was removed.
	_, statErr := os.Stat(unresolvedDir)
	assert.True(t, os.IsNotExist(statErr),
		"the session dir is removed even though the process was not killed")

	// And the agent-server is STILL ALIVE — the orphan the bug leaks.
	assert.True(t, pidAlive(pid),
		"the agent-server process must be ALIVE with the unresolved `expected` — the orphan, "+
			"holding the deleted worktree's inodes, the tmux session, and the loopback listener")
}

// --- provision wires the resolution -------------------------------------------

// The provision path calls resolveSessionDirSymlinks after makeSessionDir, so a
// symlinked session dir is resolved before it flows into p.sessionDir (and the
// persistible cleanup handle). This verifies the wiring: the `pwd -P` command takes
// the mktemp-printed (unresolved) dir and its output replaces that unresolved
// spelling.
func TestSandboxProvisionResolvesSessionDirBeforePersisting(t *testing.T) {
	var resolveCalled bool
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "mktemp -d") {
			return []byte("/root/.af-sessions/orig\n"), nil
		}
		if strings.Contains(script, "pwd -P") {
			resolveCalled = true
			assert.Contains(t, script, "'/root/.af-sessions/orig'",
				"the resolve must cd into the mktemp-printed (unresolved) dir, not a pre-resolved one")
			return []byte("/realroot/.af-sessions/orig\n"), nil
		}
		return nil, nil
	}}
	w := &sandboxWorkspace{shell: p, SessionDir: "/root/.af-sessions/orig"}
	w.resolveSessionDirSymlinks(5 * time.Second)
	assert.True(t, resolveCalled, "resolveSessionDirSymlinks must actually run the pwd -P command")
	assert.Equal(t, "/realroot/.af-sessions/orig", w.SessionDir,
		"the resolved path must replace the mktemp-printed one, so it flows into p.sessionDir "+
			"and the cleanup handle the reaper will restore")
}

// The resolved session dir drives the reapScript's `expected`, so after provision
// the reaper compares against the resolved path the running process's argv[0]
// carries — not the unresolved mktemp spelling.
func TestSandboxReapScriptExpectedUsesResolvedSessionDir(t *testing.T) {
	p := &sandboxProvisioner{
		sessionDir: "/realroot/.af-sessions/resolved",
		remotePID:  "4242",
	}
	script, _ := p.reapScript("cafe")
	assert.Contains(t, script, "/realroot/.af-sessions/resolved/af",
		"the reaper's `expected` must be the resolved path — the same spelling the re-exec'd "+
			"agent-server carries as argv[0]")
	assert.NotContains(t, script, "/root/.af-sessions/",
		"and must NOT carry the unresolved mktemp spelling, or the identity check mismatches")
}
