package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFromInstanceData_LiveRowAbsentSessionFailingRespawnDropsRow proves the
// production materializer-drop half of #4812: a persisted Live row (Running
// status, non-empty worktree paths) with a confirmed-absent tmux session
// reaches instance.Start(false) → RestoreWithResult → TmuxSession.Start →
// tmux new-session, and when that respawn fails with a non-inert error,
// FromInstanceData returns (nil, err) so the row is dropped at
// instance_data.go:590. This is the link the daemon skip-set fix
// (daemon.refreshDaemonInstances) builds on: IF the materializer errors on
// every row, a startup-skipped repo's parses-but-zero-rows file would clear
// the skip set on the "parses" signal alone and silently serve [] on the
// wire. The test stages the real FromInstanceData (no stub of the
// materializer) through the same restoreTmuxSession seam dead_respawn_test.go
// already uses.
//
// The worktree is created on disk deliberately: as of #5172 a row whose
// worktree is MISSING loads Lost rather than dropping (see the sibling tests
// below), so the pty-failure drop is only exercised when the directory itself
// is fine and the spawn still fails.
func TestFromInstanceData_LiveRowAbsentSessionFailingRespawnDropsRow(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_unloadable_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// Every session is confirmed-absent: countingExec with an empty alive set
	// answers "no such session" to every has-session, so the agent's Restore
	// takes the confirmed-absent branch and calls TmuxSession.Start(workDir).
	var newSessions, ptyNewSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	// failFirstNewSessionPty fails the first tmux new-session it sees — the
	// agent's re-spawn — reproducing "the worktree was removed so
	// `tmux new-session -c $workdir` errors" (dead_respawn_test.go:245), then
	// lets a later fresh new-session succeed.
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	// deadInstanceData(t, Running, ...) is the exact persisted shape the daemon
	// writes for a Live bound session; creating the worktree keeps the
	// spawn-dir refusal (#5172) out of the way so the pty failure is what drops
	// the row.
	data := deadInstanceData(t, Running, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)

	// The real materializer, through the real LocalBackend.Start → launch →
	// RestoreWithResult → TmuxSession.Start path, drops the row: it returns
	// (nil, err) with a non-inert error (start.go:133 wraps the pty failure in
	// ErrSessionNotStarted, which retainsInertInstance reports false).
	require.Error(t, err, "a Live row whose confirmed-absent re-spawn fails must error, not load inert")
	require.Nil(t, restored, "the unloadable row must not materialize an Instance")
	require.False(t, retainsInertInstance(err),
		"the pty new-session failure is non-inert (ErrSessionNotStarted-wrapped), so the row is dropped, not retained")
	// Exactly one pty new-session was attempted — the agent's re-spawn — and it
	// failed; FromInstanceData returned before any shell tab processing.
	assert.Equal(t, 1, ptyNewSessions,
		"exactly the agent re-spawn was attempted (and failed) before the row dropped")
	// The exec's new-session counter stays 0: the pty failed before forwarding
	// to the mock executor, so no session was recorded as created.
	assert.Equal(t, 0, newSessions,
		"the failed re-spawn never reached the executor, so no session was recorded live")
}

// TestFromInstanceData_LiveRowMissingWorktreeLoadsAsLost is the #5172
// regression: a persisted Live row whose tmux session AND worktree are both
// gone must load as Lost — never spawn a pane into tmux's fallback cwd (the
// daemon's own directory) and never report ready. Before the fix this row
// respawned through start.go's new-session and loaded Ready in a wrong
// directory; now the spawn seam refuses the missing directory and the loader
// marks the row Lost so the restore loop can rebuild the worktree from its
// surviving branch — or record WORKTREE_MISSING_DETECTED when it cannot.
func TestFromInstanceData_LiveRowMissingWorktreeLoadsAsLost(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_missing_worktree_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// The tmux sessions are gone and the worktree directory is gone — the
	// exact #5172 repro shape.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	// The worktree vanished between persist and load — the #5172 repro shape.
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))
	restored, err := FromInstanceData(data)

	require.NoError(t, err,
		"a row whose worktree vanished must still materialize — as Lost, not as a drop")
	require.NotNil(t, restored)
	assert.Equal(t, Lost, restored.GetStatus(),
		"the refused spawn must surface as a lost row the restore loop can rebuild or diagnose")
	assert.True(t, restored.Started(),
		"a Lost row stays killable and restore-eligible (started=true persists the record)")
	assert.Equal(t, 0, newSessions,
		"the spawn was refused at the seam — no new-session ever ran against the missing dir")
}

// TestFromInstanceData_LiveRowPaneInWrongDirLoadsAsLost covers the
// belt-and-braces half at the row level: even if a spawn reports success but
// the pane landed outside the requested worktree, the loader tears it down and
// marks the row Lost with the same outcome as a pre-spawn refusal.
func TestFromInstanceData_LiveRowPaneInWrongDirLoadsAsLost(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_misplaced_pane_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	// The worktree EXISTS, so the pre-spawn stat passes — but the pane's
	// current path is tmux's fallback cwd instead. pane_current_path answers
	// the agent session's placement query (a convicting source; pane_start_path
	// only ever echoes the -c it was handed and cannot convict — #5174 review),
	// while every other session falls through to countingExec's recorded -c.
	var newSessions int
	fallbackDir := t.TempDir()
	inner := countingExec(map[string]bool{}, &newSessions)
	exec := cmd_test.MockCmdExec{
		RunFunc: inner.Run,
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "pane_current_path") &&
				tmuxTargetName(cmd.Args) == agentName {
				return []byte(fallbackDir + "\n"), nil
			}
			return inner.Output(cmd)
		},
	}
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)

	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, Lost, restored.GetStatus(),
		"a pane that spawned outside its worktree is torn down and the row goes lost")
	assert.GreaterOrEqual(t, newSessions, 1,
		"the fixture must exercise a spawn tmux accepted and misplaced")
}

// TestFromInstanceData_LiveRowUnverifiableWorktreeHeldForRetry: a start-dir
// stat error that is NOT ENOENT is not evidence the worktree is gone — the
// spawn is refused without the missing verdict and the row is dropped (held),
// so a later refresh pass retries the load once the directory answers.
func TestFromInstanceData_LiveRowUnverifiableWorktreeHeldForRetry(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_unverifiable_worktree_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	// A NUL byte makes os.Stat answer EINVAL on every platform — a
	// deterministic non-ENOENT stat failure with no filesystem tricks.
	data := deadInstanceData(t, Running, agentName, shellName)
	data.Worktree.WorktreePath = "/unverifiable\x00dir"

	restored, err := FromInstanceData(data)

	require.Error(t, err, "an unproven start dir errors the load — the row is held for retry")
	require.Nil(t, restored, "a held row does not materialize this pass")
	require.ErrorIs(t, err, tmux.ErrSpawnDirUnknown)
	require.NotErrorIs(t, err, tmux.ErrSpawnDirMissing,
		"an unproven stat must never read as a missing worktree")
	require.NotErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, 0, newSessions, "no spawn while the directory state is unproven")
}

// TestStart_RefusesUnresolvedRelocation is the #5172 hold for af's own
// in-flight worktree relocation: while the claim is unresolved neither
// candidate path is authoritative, so Start(false) on a live instance must
// refuse before touching tmux — a plain retryable error, never the missing
// verdict and never a spawn into a stale or replaced directory. A persisted
// record short-circuits in FromInstanceData (the restoredRelocationRecovery
// inert path at instance_data.go:549); this guard covers the in-memory case —
// an instance whose worktree acquired a recovery record after it loaded —
// reached through RelocationSnapshot in launch.
func TestStart_RefusesUnresolvedRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_unresolved_reloc"
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	// The directory itself exists — the refusal must come from the unresolved
	// relocation claim, not the spawn-dir stat.
	worktreeDir := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(
		"/tmp/reloc-repo", worktreeDir, "reloc", "reloc-branch", "", false, true)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State: git.RelocationRecoveryStalled,
	}))
	require.True(t, gw.HasUnresolvedRelocation())

	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	inst := &Instance{
		Title:       agentName,
		Path:        "/tmp/reloc-repo",
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(ts)},
	}

	err = inst.Start(false)
	require.Error(t, err, "an unresolved relocation must refuse the restore")
	assert.Contains(t, err.Error(), "relocation",
		"the refusal must name the unresolved relocation, not a missing worktree")
	assert.Equal(t, 0, newSessions, "no spawn may run while relocation is unresolved")
}

// TestSwapAgent_RefusesUnresolvedRelocation: the swap's own stop-the-old-agent
// step must not run while af's relocation of the worktree is unresolved — a
// stale-but-existing directory would pass the os.Stat gate and take the
// replacement agent into a tree that is not authoritative, with the current
// agent already dead. The refusal is the same launch/respawn keep (#5174).
func TestSwapAgent_RefusesUnresolvedRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_swap_reloc"
	var newSessions, killSessions, panePidQueries int
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			switch {
			case strings.Contains(s, "has-session"):
				return nil // the current agent is live
			case strings.Contains(s, "new-session"):
				newSessions++
				return nil
			case strings.Contains(s, "kill-session"):
				killSessions++
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "pane_pid") {
				panePidQueries++
			}
			if strings.Contains(cmd.String(), "list-panes") {
				return nil, nil
			}
			return []byte("content"), nil
		},
	}

	worktreeDir := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(
		"/tmp/reloc-repo", worktreeDir, "reloc-swap", "reloc-swap-branch", "", false, true)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State: git.RelocationRecoveryStalled,
	}))
	require.True(t, gw.HasUnresolvedRelocation())

	ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", persistPtyFactory{t: t, cmdExec: exec}, exec)
	inst := &Instance{
		Title:       agentName,
		Path:        "/tmp/reloc-repo",
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(ts)},
	}

	err = (&LocalBackend{}).SwapAgent(inst, AgentSwapPlan{})

	require.Error(t, err, "an unresolved relocation must refuse the swap")
	assert.Contains(t, err.Error(), "relocation",
		"the refusal names the unresolved relocation, not a missing worktree")
	assert.NotContains(t, err.Error(), "not a directory")
	assert.Equal(t, 0, killSessions,
		"the current agent must NOT be stopped while the worktree claim is unresolved")
	assert.Equal(t, 0, newSessions, "no replacement spawn may run either")
	assert.Equal(t, 0, panePidQueries,
		"teardown never began — no pane identification query ran")
}
