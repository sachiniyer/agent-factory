package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceHookExec is nameKeyedExec plus a hook that fires the first time a tmux
// session is created with `onNewSession`. It lets a test deterministically
// inject a concurrent Kill (flipping started=false) in the exact window
// AddShellTab/AddProcessTab open between spawning the sibling tmux session and
// re-acquiring the lock to append the tab (#990). It also exposes session
// existence so a test can assert the spawned session was torn down (no orphan).
func raceHookExec(alive map[string]bool, onNewSession func()) (cmd_test.MockCmdExec, func(name string) bool) {
	existing := map[string]bool{}
	for k, v := range alive {
		existing[k] = v
	}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				// Strip the exact-match `=name:` wrapper (`-t =name:`) so the modeled
				// session name matches the bare key tmux resolves to (#1006).
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	fired := false
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return assertNoSession
			case strings.Contains(s, "new-session"):
				existing[n] = true
				// Simulate Kill racing in after the spawn but before the append.
				if !fired && onNewSession != nil {
					fired = true
					onNewSession()
				}
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("content"), nil
		},
	}
	isAlive := func(name string) bool { return existing[name] }
	return exec, isAlive
}

// raceMockInstance is startedMockInstance wired to raceHookExec: when the next
// sibling session is spawned, onNewSession runs (a test uses this to flip
// started=false, standing in for a concurrent KillSession). isAlive reports
// whether a tmux session name currently exists, so the test can assert the
// orphan was torn down.
func raceMockInstance(t *testing.T, agentName string, onNewSession func()) (*Instance, func(name string) bool) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	exec, isAlive := raceHookExec(map[string]bool{agentName: true}, onNewSession)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	repoPath := "/tmp/tab-kill-race-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	inst := &Instance{
		Title:       agentName,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}
	return inst, isAlive
}

// flipStarted clears started under the same lock Kill uses, so the recheck in
// AddShellTab/AddProcessTab observes a killed instance — the deterministic
// stand-in for a concurrent KillSession (#990).
func flipStarted(i *Instance) {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
}

// TestAddShellTab_KillRaceDoesNotLeakSession verifies the #990 fix: if the
// session is killed (started flipped false) after the shell tab's tmux session
// is spawned but before it is appended, AddShellTab must NOT append the tab,
// MUST tear down the spawned tmux session (no orphan referencing the deleted
// worktree), and MUST return an error.
func TestAddShellTab_KillRaceDoesNotLeakSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_shell_killrace"
	var inst *Instance
	var isAlive func(string) bool
	inst, isAlive = raceMockInstance(t, agentName, func() { flipStarted(inst) })

	tab, err := inst.AddShellTab()
	require.Error(t, err, "a tab created during teardown must be refused")
	assert.Contains(t, err.Error(), "session was killed during tab creation")
	assert.Nil(t, tab)
	assert.Equal(t, 1, inst.TabCount(), "the raced tab must not be appended")

	orphan := agentName + "__" + shellTabName
	assert.False(t, isAlive(orphan), "the spawned tmux session must be torn down (no orphan)")
}

// TestAddProcessTab_KillRaceDoesNotLeakSession is the AddProcessTab counterpart
// of the shell-tab kill-race test (#990): the same TOCTOU window was copied to
// AddProcessTab, so it must enforce the same recheck-and-teardown.
func TestAddProcessTab_KillRaceDoesNotLeakSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_proc_killrace"
	var inst *Instance
	var isAlive func(string) bool
	inst, isAlive = raceMockInstance(t, agentName, func() { flipStarted(inst) })

	tab, err := inst.AddProcessTab("btop", "")
	require.Error(t, err, "a process tab created during teardown must be refused")
	assert.Contains(t, err.Error(), "session was killed during tab creation")
	assert.Nil(t, tab)
	assert.Equal(t, 1, inst.TabCount(), "the raced tab must not be appended")

	orphan := agentName + "__btop"
	assert.False(t, isAlive(orphan), "the spawned tmux session must be torn down (no orphan)")
}

// TestAttachShellTab_KilledSessionDoesNotSpawn is the #1152 regression: the
// daemon spawned the shell session out-of-band and then killed the instance
// before the TUI reflects the tab. AttachShellTab is a pure TUI-side projection
// of daemon-owned state and must ATTACH ONLY — it must NOT re-spawn the missing
// session. Re-spawning would create a tmux session in the TUI process that
// escapes the daemon's Kill teardown and orphans over the about-to-be-deleted
// worktree, violating the single-writer model (#960) — the same #990 leak class
// AddShellTab guards. It must fail cleanly, spawn nothing, and append nothing.
func TestAttachShellTab_KilledSessionDoesNotSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_attach_killed"
	// Seed only the agent session alive: the shell session the daemon created
	// was already killed, so has-session for it reports missing. onNewSession
	// fires iff a tmux new-session is ever issued — the bug's fingerprint.
	spawned := false
	inst, isAlive := raceMockInstance(t, agentName, func() { spawned = true })

	tab, err := inst.AttachShellTab(shellTabName, "")
	require.Error(t, err, "attaching to a killed session must fail, not resurrect it")
	assert.Contains(t, err.Error(), "failed to reconnect shell tab")
	assert.Nil(t, tab)
	assert.False(t, spawned, "AttachShellTab must never issue tmux new-session (#1152)")
	assert.False(t, isAlive(agentName+"__"+shellTabName),
		"no orphan shell session may exist after a failed attach")
	assert.Equal(t, 1, inst.TabCount(), "the un-attachable tab must not be appended")
}

// TestAttachShellTab_KillRaceDropsProjection covers the append guard: the shell
// session is live when the attach begins (so Restore reconnects), but a
// concurrent Kill flips started=false in the window between AttachShellTab
// releasing its read lock and re-locking to append — exactly the #990 window.
// AttachShellTab must then drop the projection (append nothing) and return an
// error, releasing only the local attach client it opened (never killing the
// session — the daemon owns that).
func TestAttachShellTab_KillRaceDropsProjection(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_attach_race"
	shellName := agentName + "__" + shellTabName

	var inst *Instance
	spawned := false
	flipped := false
	killedSession := false
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "new-session"):
				spawned = true
				return nil
			case strings.Contains(s, "kill-session"):
				if strings.Contains(s, shellName) {
					killedSession = true
				}
				return nil
			case strings.Contains(s, "has-session"):
				// The shell session is live when the attach begins. On its first
				// existence probe (inside Restore, after AttachShellTab released
				// its RLock) simulate a concurrent Kill landing: flip started=false
				// so the write-lock recheck observes the teardown.
				if strings.Contains(s, shellName) && !flipped {
					flipped = true
					inst.mu.Lock()
					inst.started = false
					inst.mu.Unlock()
				}
				return nil // agent + shell both report alive
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	pty := persistPtyFactory{t: t, cmdExec: exec}

	repoPath := "/tmp/tab-attach-race-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	inst = &Instance{
		Title:       agentName,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}

	tab, err := inst.AttachShellTab(shellTabName, "")
	require.Error(t, err, "a tab attached during teardown must be refused")
	assert.Contains(t, err.Error(), "session was killed during tab attach")
	assert.Nil(t, tab)
	assert.False(t, spawned, "the attach path must never spawn a session (#1152)")
	assert.False(t, killedSession,
		"the projection must not kill the daemon-owned session; only release its own attach client")
	assert.Equal(t, 1, inst.TabCount(), "the raced tab must not be appended")
}

// TestAddTab_NoKillRaceStillAppends verifies the fix does not regress the happy
// path: with no concurrent kill, both AddShellTab and AddProcessTab still spawn,
// append, and return a live tab.
func TestAddTab_NoKillRaceStillAppends(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, isAlive := raceMockInstance(t, "af_no_killrace", nil)

	shell, err := inst.AddShellTab()
	require.NoError(t, err)
	require.NotNil(t, shell)
	assert.Equal(t, shellTabName, shell.Name)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, isAlive("af_no_killrace__"+shellTabName), "the shell tab session must be live")

	proc, err := inst.AddProcessTab("btop", "")
	require.NoError(t, err)
	require.NotNil(t, proc)
	assert.Equal(t, "btop", proc.Name)
	assert.Equal(t, 3, inst.TabCount())
	assert.True(t, isAlive("af_no_killrace__btop"), "the process tab session must be live")
}
