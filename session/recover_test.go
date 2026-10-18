package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExec wraps countingExec and additionally captures every
// new-session command string, so tests can assert on the spawned program.
func recordingExec(alive map[string]bool, newSessions *int, spawns *[]string) cmd_test.MockCmdExec {
	inner := countingExec(alive, newSessions)
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "new-session") {
				*spawns = append(*spawns, cmd.String())
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
}

// deadPaneAwareExec teaches an executor to answer the pane-row reads the
// waiting teardown path (CloseAndWaitForPaneExit) makes — list-panes and
// display-message — instead of countingExec's blanket "content", which parses
// as an invalid pane pid and turns every teardown confirmation into a
// refusal. Sessions in `existing` (kept in step with kill/new-session) answer
// with a held dead-pane row; absent ones answer with tmux's exit-1
// "can't find session" diagnostic so absence classifies as determinate empty.
func deadPaneAwareExec(existing map[string]bool, inner cmd_test.MockCmdExec) cmd_test.MockCmdExec {
	state := map[string]bool{}
	for k, v := range existing {
		state[k] = v
	}
	var missing *exec.ExitError
	if err := exec.Command("sh", "-c", "exit 1").Run(); err != nil {
		missing, _ = err.(*exec.ExitError)
	}
	nameOf := func(cmd *exec.Cmd) string {
		// `-t` names the target for list-panes/display-message/kill-session;
		// `-s` names it only for new-session — for list-panes `-s` is a
		// flagless "session format" switch that must not eat the next arg.
		takeValue := !strings.Contains(cmd.String(), "list-panes")
		for i, a := range cmd.Args {
			switch {
			case a == "-t" && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case a == "-s" && takeValue && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimSuffix(strings.TrimPrefix(a, "-t="), ":")
			case strings.HasPrefix(a, "-s=") && takeValue:
				return strings.TrimSuffix(strings.TrimPrefix(a, "-s="), ":")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if err := inner.Run(cmd); err != nil {
				return err
			}
			s := cmd.String()
			switch {
			case strings.Contains(s, "new-session"):
				state[nameOf(cmd)] = true
			case strings.Contains(s, "kill-session"):
				delete(state, nameOf(cmd))
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := cmd.String()
			if !strings.Contains(s, "list-panes") && !strings.Contains(s, "display-message") {
				return inner.Output(cmd)
			}
			name := nameOf(cmd)
			if state[name] {
				return []byte("999999|1|\n"), nil
			}
			if missing == nil {
				return nil, errors.New("can't find session: " + name)
			}
			missing.Stderr = []byte("can't find session: " + name)
			return nil, missing
		},
	}
}

// lostInstanceForRecover loads a Lost instance through the production path
// (FromInstanceData → Start(false) → #970 guard, no re-spawn) with an
// EXISTING worktree directory, ready for Recover.
func lostInstanceForRecover(t *testing.T, agentName, shellName string, exec cmd_test.MockCmdExec) *Instance {
	t.Helper()
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	return restored
}

// TestRecover_RespawnsLostSession: the daemon's explicit Recover (#1108 PR 2)
// re-spawns a Lost instance's tmux session in its worktree — the operation the
// #970 guard forbids at load time — and flips the instance Running like a
// fresh create. The spawned program must carry the resolved-program injection
// (#1132 choke-point) exactly once.
func TestRecover_RespawnsLostSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	require.NoError(t, restored.Recover())

	assert.Greater(t, newSessions, 0, "Recover must re-spawn the missing tmux session")
	assert.Equal(t, Running, restored.GetStatus(),
		"a recovered session is booting its program: Running, like a fresh create")
	assert.True(t, restored.TabAlive(0), "the agent session must exist server-side after Recover")

	// The #1132 choke-point injected claude's --plugin-dir into the spawn —
	// exactly once, recomputed from the clean persisted Program (repeated
	// attempts must never accumulate flags).
	require.NotEmpty(t, spawns)
	agentSpawn := spawns[0]
	assert.Equal(t, 1, strings.Count(agentSpawn, "--plugin-dir"),
		"resolved-program injection must appear exactly once in the spawn: %s", agentSpawn)
	require.NotEmpty(t, restored.RuntimeProgram(),
		"Recover respawns a real process, so it must record the command it launched (#5066)")
	assert.Equal(t, resolveLaunchProgramForInstance(restored).command, restored.RuntimeProgram(),
		"the recorded command must be the respawn's resolved base, not the bare persisted Program")
}

func TestRecover_UnverifiedReattachClearsRuntimeProgram(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_recreated_same_name"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)
	restored.setRuntimeProgram("/old/claude")
	stale := restored.ObserveRuntimeProgram()

	require.NoError(t, restored.Recover())
	require.Zero(t, newSessions, "the fixture must exercise name-only reattachment, not verified respawn")
	require.Empty(t, restored.RuntimeProgram(),
		"a recovered tmux name cannot prove that the pane is the runtime whose command was persisted")
	require.False(t, restored.RuntimeProgramEvidenceCurrent(stale))
}

func TestRespawnForAccountSwap_StartsFreshConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_account_swap_agent"
	var newSessions int
	var spawns []string
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	restored.mu.Lock()
	restored.Tabs[1].Kind = TabKindProcess
	restored.Tabs[1].Command = "git status --short"
	restored.Tabs[1].tmux.SetProgram("git status --short")
	restored.mu.Unlock()
	restored.Path = initTempGitRepo(t)
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err = restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	require.NoError(t, restored.RespawnForAccountSwap())
	require.NotEmpty(t, spawns)
	agentSpawn := spawns[0]
	require.NotContains(t, agentSpawn, "--continue",
		"the previous account's conversation store must never be resumed under the replacement account")
	require.Contains(t, agentSpawn, "--session-id",
		"the replacement claude identity must receive an explicit fresh conversation")
	require.Contains(t, strings.Join(spawns, "\n"), "__af-session-env-exec-account-environment",
		"restored process panes must launch inside the replacement account environment")
	assert.Equal(t, "claude", restored.RuntimeProgram(),
		"an account-swap respawn launches a replacement process and must record its resolved command (#5066)")
}

// A relocation probe that reaches its deadline latches the worktree as stalled
// even though it has not yet created two pathname candidates. The Lost loop must
// honor that latch before os.Stat or rebuild touches the same filesystem: those
// unbounded calls can wedge the daemon's sole status/self-healing goroutine.
func TestRecover_RefusesWorktreeAfterRelocationProbeTimeout(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_relocation_stall"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 300\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(100 * time.Millisecond))

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	moveErr := worktree.MoveWorktree(filepath.Join(t.TempDir(), "archive"))
	require.ErrorIs(t, moveErr, context.DeadlineExceeded,
		"the setup must trip the bounded pre-move probe")
	recovery, hasRecovery := worktree.GetRelocationRecovery()
	require.True(t, hasRecovery,
		"every latched relocation stall must materialize recovery state; absent cannot mean safe")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, recovery.State)
	storedRecovery := restored.ToInstanceData().Worktree.RelocationRecovery
	require.NotNil(t, storedRecovery, "the stall latch must survive the daemon's durable snapshot")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, storedRecovery.State)

	err = restored.Recover()
	require.Error(t, err, "Lost recovery must refuse a worktree whose filesystem already stalled")
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.Equal(t, 0, newSessions, "the stalled worktree must be refused before any agent spawn")
}

// TestRecover_RetryAfterFailureInjectsFlagsOnce: a failed first attempt (the
// outage still biting) must leave the instance retryable — still Lost, tmux
// refs intact — and the eventual successful spawn must still carry the
// injected flags exactly once.
func TestRecover_RetryAfterFailureInjectsFlagsOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_retry"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions, ptyNewSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)

	// First new-session fails (server still hostile), later ones succeed.
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	require.Error(t, restored.Recover(), "first attempt must surface the spawn failure")
	assert.Equal(t, Lost, restored.GetStatus(), "a failed recover leaves the session Lost")
	assert.True(t, restored.Started(), "a failed recover must keep the row killable")

	require.NoError(t, restored.Recover(), "retry must succeed once the server behaves")
	assert.Equal(t, Running, restored.GetStatus())
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			assert.Equal(t, 1, strings.Count(spawn, "--plugin-dir"),
				"every attempt recomputes injection from the clean Program: %s", spawn)
		}
	}
}

// TestRecover_FailsWithoutWorktree: a deleted worktree is the expected
// permanent-failure shape; Recover must name it (the restore loop's
// escalation log leans on this) and leave the session Lost and killable.
func TestRecover_FailsWithoutWorktree(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_nowt"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path deliberately absent.
	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree", "the failure must name the missing worktree")
	assert.Equal(t, 0, newSessions, "no spawn may happen without a worktree")
	assert.Equal(t, Lost, restored.GetStatus())
}

// TestRecover_FailsWithNonDirectoryWorktree: a regular file squatting on the
// persisted worktree path is not rebuildable — rebuild must never delete data
// af did not create — and stats fine, so it would otherwise skip the rebuild
// branch and retry into the tmux spawn-dir refusal until give-up (#5172 review).
// Recover must instead fail fast naming the cause, leave the session Lost, and
// never attempt a spawn.
func TestRecover_FailsWithNonDirectoryWorktree(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_filewt"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path exists but is a regular file, not a directory.
	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))
	require.NoError(t, os.WriteFile(data.Worktree.WorktreePath, []byte("squatter"), 0o644))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory", "the failure must name the squatting file, not a generic spawn error")
	var wtErr *WorktreeUnavailableError
	assert.ErrorAs(t, err, &wtErr, "the permanent-failure shape the restore loop escalates on")
	assert.Equal(t, 0, newSessions, "no spawn may happen when the worktree path is not a directory")
	assert.Equal(t, Lost, restored.GetStatus())
}

func TestRecover_RebuildsMissingWorktreeFromExistingBranchAndResumesRecordedConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_branch"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{},
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	_, statErr := os.Stat(worktreePath)
	require.True(t, os.IsNotExist(statErr), "test setup should start with a missing worktree")

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.True(t, restored.TabAlive(0), "Recover must re-spawn the agent after rebuilding the worktree")
	require.DirExists(t, worktreePath)
	assert.Equal(t, branch, gitOut(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"))

	var agentSpawn string
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			agentSpawn = spawn
			break
		}
	}
	require.NotEmpty(t, agentSpawn, "expected to record the agent new-session command")
	assert.Contains(t, agentSpawn, "codex resume "+conversationID,
		"Recover must resume the recorded Codex conversation, not the latest one")
	assert.NotContains(t, agentSpawn, "resume --last")

	saved := restored.ToInstanceData()
	require.NotNil(t, saved.Worktree.BranchCreatedByUs)
	assert.True(t, *saved.Worktree.BranchCreatedByUs,
		"rebuilding from the surviving branch must preserve original branch ownership")
}

func TestRecover_RebuildsBranchGoneWorktreeFromRecordedBaseAndResumesRecordedConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "base")
	baseSHA := gitOut(t, repoRoot, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "later.txt"), []byte("later\n"), 0644))
	gitOut(t, repoRoot, "add", "later.txt")
	gitOut(t, repoRoot, "commit", "-m", "later")
	require.NotEqual(t, baseSHA, gitOut(t, repoRoot, "rev-parse", "HEAD"))

	const branch = "af/branch-gone"
	const agentName = "af_recover_branch_gone"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-branch-gone")
	branchCreatedByUs := false
	data := InstanceData{
		Title:    "branch-gone",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramClaude,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramClaude,
					ID:          conversationID,
					CaptureKind: ConversationCaptureInjected,
				},
			},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "branch-gone",
			BranchName:        branch,
			BaseCommitSHA:     baseSHA,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{},
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	require.DirExists(t, worktreePath)
	assert.Equal(t, branch, gitOut(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Equal(t, baseSHA, gitOut(t, worktreePath, "rev-parse", "HEAD"),
		"branch-gone recovery should prefer the recorded base over current master")

	var agentSpawn string
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			agentSpawn = spawn
			break
		}
	}
	require.NotEmpty(t, agentSpawn, "expected to record the agent new-session command")
	assert.Contains(t, agentSpawn, "--resume "+conversationID)
	assert.NotContains(t, agentSpawn, "--continue")

	saved := restored.ToInstanceData()
	require.NotNil(t, saved.Worktree.BranchCreatedByUs)
	assert.True(t, *saved.Worktree.BranchCreatedByUs,
		"a branch recreated from recorded base is now owned by Agent Factory")
	assert.Equal(t, baseSHA, saved.Worktree.BaseCommitSHA)
}

func TestRecover_BranchGoneWithoutRecordedConversationDoesNotFreshDispatch(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "base")
	baseSHA := gitOut(t, repoRoot, "rev-parse", "HEAD")

	const branch = "af/no-conversation"
	const agentName = "af_recover_no_conversation"
	shellName := agentName + tmuxTabSeparator + shellTabName
	worktreePath := filepath.Join(t.TempDir(), "repo-no-conversation")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "no-conversation",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "no-conversation",
			BranchName:        branch,
			BaseCommitSHA:     baseSHA,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	tmuxExec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: tmuxExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, tmuxExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded conversation id")
	assert.Equal(t, 0, newSessions, "branch-gone recovery must not dispatch a fresh agent without exact resume")
	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "worktree must remain absent when exact conversation resume is unavailable")
	assert.Error(t, exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "refs/heads/"+branch).Run(),
		"branch must not be recreated without exact conversation resume")
	assert.Equal(t, Lost, restored.GetStatus())
}

// TestRecover_RefusesNonLostAndTombstoned: Recover is for Lost sessions only —
// a live session must never be re-spawned over (adopt, never clobber), and a
// tombstoned record's only future is having its kill finished.
func TestRecover_RefusesNonLostAndTombstoned(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_guard"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	live, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)
	require.Error(t, live.Recover(), "a non-Lost session must be refused")

	tombstoned := lostInstanceForRecover(t, "af_recover_guard2", "af_recover_guard2"+tmuxTabSeparator+shellTabName,
		countingExec(map[string]bool{}, &newSessions))
	tombstoned.MarkUserKilled()
	require.Error(t, tombstoned.Recover(), "a tombstoned session must be refused")
}

// TestRecover_RespawnsSurvivingPaneAfterWorktreeRebuild is the #5370
// regression: a deleted worktree whose agent tmux session SURVIVED used to
// come back through rebuild → RestoreReattached → ConfirmLive — a Running row
// bound to a pane whose cwd is the unlinked inode, divorced from the checkout
// at the same path. After a rebuild, a surviving name can never be inside the
// new worktree: Recover must kill it and re-spawn into the rebuilt checkout
// (the recorded conversation travels through the resume path), never bind the
// stale pane as Running.
func TestRecover_RespawnsSurvivingPaneAfterWorktreeRebuild(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-survivor"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_survivor"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-survivor")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-survivor",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-survivor",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	// Both the agent and the shell tmux sessions are still live — the stale
	// panes whose cwd is the deleted inode.
	var newSessions int
	var spawns, kills []string
	inner := deadPaneAwareExec(map[string]bool{agentName: true, shellName: true},
		recordingExec(map[string]bool{agentName: true, shellName: true}, &newSessions, &spawns))
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				kills = append(kills, cmd.String())
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	_, statErr := os.Stat(worktreePath)
	require.True(t, os.IsNotExist(statErr), "test setup should start with a missing worktree")

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	require.DirExists(t, worktreePath)

	// Every surviving stale pane must have been killed BEFORE its name was
	// re-spawned inside the rebuilt worktree — the agent's included.
	assert.Contains(t, strings.Join(kills, "\n"), agentName,
		"Recover must kill the surviving agent pane — it can never be inside the rebuilt worktree")
	assert.Contains(t, strings.Join(kills, "\n"), shellName,
		"Recover must kill every surviving stale pane, not only the agent's")
	assert.Equal(t, 2, newSessions, "agent + shell must be freshly spawned inside the rebuilt worktree")

	var agentSpawn string
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellName) {
			agentSpawn = spawn
			break
		}
	}
	require.NotEmpty(t, agentSpawn, "expected to record the agent new-session command")
	assert.Contains(t, agentSpawn, "codex resume "+conversationID,
		"the respawn must keep the recorded conversation via the resume path")
}

// TestRecover_RebuiltTeardownSurvivesAPartialFailureRetry is the retry half of
// the #5370 property (#5371 review): when a rebuild commits but a stale-pane
// teardown fails mid-attempt, the next Recover MUST NOT skip the teardown just
// because os.Stat now sees the rebuilt directory — the rebuilt gate is
// re-derived per attempt while a stale survivor still answers has-session.
// Without a durable indication the second attempt reattaches the stale pane
// and ConfirmLive binds it as Running — the original defect through a retry.
func TestRecover_RebuiltTeardownSurvivesAPartialFailureRetry(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-retry"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_retry"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-retry")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-retry",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-retry",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	// The stale agent session survives; kill-session is wedged on attempt 1
	// only, so the rebuild commits while the teardown fails.
	var newSessions int
	var spawns, kills []string
	inner := deadPaneAwareExec(map[string]bool{agentName: true},
		recordingExec(map[string]bool{agentName: true}, &newSessions, &spawns))
	failKills := true
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				kills = append(kills, cmd.String())
				if failKills {
					return errors.New("tmux wedged on kill-session")
				}
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())

	require.Error(t, restored.Recover(), "attempt 1 must fail — the stale pane refused to die")
	require.DirExists(t, worktreePath, "the rebuild committed before the teardown failed")
	assert.Equal(t, 0, newSessions, "no replacement may launch while the stale pane survives")

	// Attempt 2: the worktree now EXISTS, so a re-derived rebuild flag would
	// skip teardown and reattach the stale survivor.
	failKills = false
	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.Contains(t, strings.Join(kills, "\n"), agentName,
		"the retry must still tear down the stale pane from the earlier rebuild")
	require.Len(t, spawns, 1, "the retry must respawn the agent inside the rebuilt worktree")
	assert.Contains(t, spawns[0], "codex resume "+conversationID)
}

// A committed rebuild whose stale-pane teardown is undischarged must keep the
// row Lost against ANY positive observation: a live answer can still be the
// pane bound to the deleted inode, which a status poll cannot tell apart from
// a fresh one. Lifting the row to Running/Ready would drop it out of the
// restore loop and strand the teardown (#5371 review: P1 live-status gate).
func TestTransition_RebuildTeardownPendingHoldsObservedLivenessLost(t *testing.T) {
	i := &Instance{Title: "pending", liveness: LiveLost}
	require.NoError(t, i.SetRebuildTeardownPending(true))

	for _, observed := range []Liveness{LiveRunning, LiveReady, LiveLimitReached, LiveDead, LiveArchived} {
		require.NoError(t, i.Transition(ObserveLiveness(observed)))
		assert.Equal(t, LiveLost, i.GetLiveness(),
			"observed %v must not lift a pending-teardown row out of Lost", observed)
	}
	// A Lost observation is the poll's own mark — it still applies.
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))
	assert.Equal(t, LiveLost, i.GetLiveness())

	// Even a liveness that slipped to Running before the flag was set is
	// pulled back: pending means the live answer is unverifiable.
	running := &Instance{Title: "pending-running", liveness: LiveRunning}
	require.NoError(t, running.SetRebuildTeardownPending(true))
	require.NoError(t, running.Transition(ObserveLiveness(LiveReady)))
	assert.Equal(t, LiveLost, running.GetLiveness(),
		"a pending row must not keep a positive liveness it cannot verify")

	// Once the obligation is discharged, observations apply again.
	require.NoError(t, i.SetRebuildTeardownPending(false))
	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	assert.Equal(t, LiveRunning, i.GetLiveness())
}

// The durable contract behind the flag: every transition must land on disk
// through the installed notify before the dependent step runs, and a refused
// write leaves the flag SET — the obligation may exist either way (unrecorded
// commit or unrecorded discharge), so "set" is the only honest post-error
// state (#5371 review: P2 durable discharge).
func TestSetRebuildTeardownPending_CheckpointContract(t *testing.T) {
	var transitions []bool
	i := &Instance{Title: "ckpt"}
	i.SetRebuildTeardownNotify(func(inst *Instance) error {
		transitions = append(transitions, inst.RebuildTeardownPending())
		return nil
	})
	require.NoError(t, i.SetRebuildTeardownPending(true))
	require.NoError(t, i.SetRebuildTeardownPending(false))
	assert.Equal(t, []bool{true, false}, transitions,
		"the notify must observe the flag already in its new state")

	failure := errors.New("disk wedged")
	i.SetRebuildTeardownNotify(func(*Instance) error { return failure })
	require.ErrorIs(t, i.SetRebuildTeardownPending(true), failure)
	assert.True(t, i.RebuildTeardownPending(),
		"a failed set-checkpoint keeps the flag: the rebuild committed either way")
	require.ErrorIs(t, i.SetRebuildTeardownPending(false), failure)
	assert.True(t, i.RebuildTeardownPending(),
		"a failed clear-checkpoint restores the flag: the discharge never landed")
}

// The clear-checkpoint must precede the first replacement spawn, or a daemon
// exit in the window between them reloads `true` and kills the fresh panes
// (#5371 review). The notify sees zero new-session calls at clear time, and a
// refused clear leaves the row Lost with nothing spawned.
func TestRecover_RebuildTeardownClearPrecedesRespawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-clear"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_clearckpt"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-clear")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-clear",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-clear",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{agentName: true},
		recordingExec(map[string]bool{agentName: true}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())

	clearSpawnCount := -1
	var notifyCalls int
	restored.SetRebuildTeardownNotify(func(inst *Instance) error {
		notifyCalls++
		if !inst.RebuildTeardownPending() {
			clearSpawnCount = newSessions
		}
		return nil
	})
	require.NoError(t, restored.Recover())
	require.Equal(t, 2, notifyCalls, "set at rebuild-commit + clear after confirmed teardown")
	assert.Equal(t, 0, clearSpawnCount,
		"the cleared obligation must be durable before any replacement launches")
	assert.Equal(t, Running, restored.GetStatus())
	assert.Equal(t, 1, newSessions, "the agent respawned after the checkpointed clear")
}
