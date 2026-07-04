package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingExec is nameKeyedExec with a new-session counter. Sessions named in
// `alive` report existing immediately; others are absent until their
// new-session call (which bumps *newSessions), so a test can assert exactly
// whether a missing session was re-spawned during restore.
func countingExec(alive map[string]bool, newSessions *int) cmd_test.MockCmdExec {
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
	return cmd_test.MockCmdExec{
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
				*newSessions++
				existing[n] = true
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
}

// deadInstanceData builds a persisted record for a local instance with the given
// status, an agent + shell tab, and non-empty worktree data — the exact shape
// (worktree present) that lets TmuxSession.Restore re-spawn a missing session.
func deadInstanceData(t *testing.T, status Status, agentName, shellName string) InstanceData {
	t.Helper()
	const repoPath = "/tmp/dead-respawn-repo"
	return InstanceData{
		Title:    "dead-respawn",
		Path:     repoPath,
		Program:  "claude",
		Status:   status,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(t.TempDir(), "wt"), // non-empty: enables re-spawn
			SessionName:  "dead-respawn",
			BranchName:   "dead-branch",
		},
	}
}

// TestDeadInstance_LoadsAsLostWithoutRespawn is the #970 regression updated for
// the #1108 rollforward: an instance persisted as Dead by a pre-Lost build (its
// tmux session vanished out from under it) reloads as Lost — recovery-eligible,
// since only observed-death paths ever wrote persisted Dead — and must NOT
// re-spawn any tmux session on load: neither the agent session
// (TmuxSession.Restore's #386 re-spawn-when-missing path) nor the shell tab
// (setupTabs). Recovery is the daemon's explicit restore loop, never a
// load-time side effect. It must still load as started=true so the daemon's
// SaveInstances checkpoint (which skips !Started instances) keeps the record on
// disk and the user can still kill it.
func TestDeadInstance_LoadsAsLostWithoutRespawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	// Isolate config reads from the developer's real ~/.agent-factory (see
	// tab_persist_test.go for the full rationale).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_dead_agent"
	shellName := agentName + shellTmuxSuffix

	// Both sessions are GONE (the death that produced Dead): a Restore would hit
	// the missing-session branch and re-spawn via new-session.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Dead, agentName, shellName))
	require.NoError(t, err)

	assert.Equal(t, 0, newSessions,
		"a Dead/Lost instance must NOT re-spawn any tmux session on load (#970)")
	assert.Equal(t, Lost, restored.GetStatus(),
		"persisted Dead must roll forward to Lost on load (#1108)")
	assert.True(t, restored.Started(),
		"a Lost instance must load started=true so SaveInstances keeps it on disk")
	assert.False(t, restored.TabAlive(0),
		"the vanished agent session must not exist server-side after load")
}

// TestLostInstance_NotRespawnedOnLoad extends the #970 guard to the Lost status
// itself (#1108): a record persisted as Lost must reload as Lost without any
// re-spawn — the daemon's explicit restore loop owns recovery, not the loader.
func TestLostInstance_NotRespawnedOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_lost_agent"
	shellName := agentName + shellTmuxSuffix

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Lost, agentName, shellName))
	require.NoError(t, err)

	assert.Equal(t, 0, newSessions,
		"a Lost instance must NOT re-spawn any tmux session on load (#970/#1108)")
	assert.Equal(t, Lost, restored.GetStatus())
	assert.True(t, restored.Started())
}

// TestTombstonedInstance_NotRespawnedOnLoad guards the kill-intent tombstone
// (#1108): a record whose UserKilled flag survived a crash mid-kill must not be
// re-spawned on load regardless of its persisted status — its only future is
// having its teardown finished by the daemon poll. Status Running is the
// realistic crash shape: KillSession tombstones BEFORE any status change, so an
// interrupted kill leaves the pre-kill status behind — and Running is exactly
// the status that would otherwise take the #386 re-spawn path.
func TestTombstonedInstance_NotRespawnedOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_tombstone_agent"
	shellName := agentName + shellTmuxSuffix

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	data.UserKilled = true
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	assert.Equal(t, 0, newSessions,
		"a tombstoned instance must NOT be re-spawned on load, whatever its status (#1108)")
	assert.True(t, restored.UserKilled(), "the tombstone must survive the round-trip")
	assert.True(t, restored.Started())
}

// TestDeadInstance_HasUpdatedNilMonitor is the #999 regression, exercised
// through the production path. A persisted Dead instance loads (as Lost, per
// the #1108 rollforward) with started=true but LocalBackend.Start returns
// before TmuxSession.Restore (the only place the tmux monitor is initialized)
// so the session is not re-spawned (#970). The daemon's refreshInstanceStatus
// still polls every started instance via instance.HasUpdated(); before the fix
// that dereferenced a nil monitor and panicked, killing the refresh goroutine
// and zombifying the daemon. HasUpdated must instead return (false,false) — a
// session with no live monitor has nothing to report.
func TestDeadInstance_HasUpdatedNilMonitor(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_dead_hasupdated"
	shellName := agentName + shellTmuxSuffix

	// Both sessions are GONE (the death that produced Dead): Start(false)
	// returns at the Dead/Lost guard before Restore, so the agent TmuxSession's
	// monitor is nil.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Dead, agentName, shellName))
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	require.True(t, restored.Started())

	// This is the exact call refreshInstanceStatus makes every daemon tick.
	// Before the nil-monitor guard it panicked here.
	updated, hasPrompt := restored.HasUpdated()
	assert.False(t, updated, "a restored Dead instance has nothing to report")
	assert.False(t, hasPrompt)
}

// failFirstNewSessionPty is a PtyFactory that fails the first `tmux new-session`
// it sees and forwards every other command to the mock executor (like
// persistPtyFactory). It reproduces a persisted shell tab whose re-spawn fails
// — e.g. the worktree was removed so `tmux new-session -c $workdir` errors —
// while letting a subsequent fresh-shell creation succeed.
type failFirstNewSessionPty struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
	count   *int
}

func (p failFirstNewSessionPty) Start(cmd *exec.Cmd) (*os.File, error) {
	if strings.Contains(cmd.String(), "new-session") {
		*p.count++
		if *p.count == 1 {
			// Simulate the dead shell's re-spawn failing (vanished worktree).
			// Returning an error here makes TmuxSession.Start fail fast without
			// marking the session live, so the session stays absent server-side.
			return nil, fmt.Errorf("new-session failed: worktree gone")
		}
	}
	f, err := os.CreateTemp(p.t.TempDir(), "pty-")
	if err == nil {
		_ = p.cmdExec.Run(cmd)
	}
	return f, err
}

func (p failFirstNewSessionPty) Close() {}

// TestRunningInstance_DeadShellTabReplacedWithFreshShellOnLoad is the #991
// regression, exercised through the production path (FromInstanceData ->
// Start(false) -> setupTabs). A Running instance restores with a live agent tab
// but a shell tab whose tmux session is gone and whose re-spawn fails. Restore
// must NOT leave that dead shell tab in place: with no LIVE shell after restore,
// setupTabs must create a fresh default shell so the user lands on a working
// terminal. Before the fix, hasLiveShell was set on tab presence alone, the
// fallback was skipped, and TabAlive(1) stayed false (a dead terminal).
func TestRunningInstance_DeadShellTabReplacedWithFreshShellOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_991_agent"
	shellName := agentName + shellTmuxSuffix

	// The agent session is alive (reconnect path); the shell session is gone, so
	// its restore hits the re-spawn branch — which the PTY fails on the first
	// new-session. A later fresh-shell new-session succeeds.
	var newSessions, ptyNewSessions int
	exec := countingExec(map[string]bool{agentName: true}, &newSessions)
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)

	assert.True(t, restored.TabAlive(0), "the live agent tab must stay reconnected")
	assert.True(t, restored.TabAlive(1),
		"restore must replace the dead shell tab with a freshly-created live shell (#991)")
	assert.Equal(t, 1, newSessions,
		"exactly one fresh shell session must be spawned (the failed re-spawn never counted)")
	assert.Equal(t, Running, restored.GetStatus())
	assert.True(t, restored.Started())
}

// TestLiveInstance_RespawnsMissingSessionOnLoad guards the seam from the other
// side: the #970 fix must NOT regress the #386/#930 re-spawn-across-reboot path.
// A non-Dead (Running) instance whose tmux server died across a reboot must
// still re-spawn its missing sessions on load.
func TestLiveInstance_RespawnsMissingSessionOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_live_agent"
	shellName := agentName + shellTmuxSuffix

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)

	assert.Greater(t, newSessions, 0,
		"a live instance with a missing session must still re-spawn on load (#386/#930)")
	assert.True(t, restored.Started())
}
