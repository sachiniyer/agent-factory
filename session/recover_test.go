package session

import (
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
	shellName := agentName + shellTmuxSuffix
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
	shellName := agentName + shellTmuxSuffix
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
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellTmuxSuffix) {
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
	shellName := agentName + shellTmuxSuffix
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path deliberately NOT created.
	restored, err := FromInstanceData(deadInstanceData(t, Lost, agentName, shellName))
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree", "the failure must name the missing worktree")
	assert.Equal(t, 0, newSessions, "no spawn may happen without a worktree")
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
	shellName := agentName + shellTmuxSuffix
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
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
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
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellTmuxSuffix) {
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
	shellName := agentName + shellTmuxSuffix
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
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
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
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, shellTmuxSuffix) {
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
	shellName := agentName + shellTmuxSuffix
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
	shellName := agentName + shellTmuxSuffix
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

	tombstoned := lostInstanceForRecover(t, "af_recover_guard2", "af_recover_guard2"+shellTmuxSuffix,
		countingExec(map[string]bool{}, &newSessions))
	tombstoned.MarkUserKilled()
	require.Error(t, tombstoned.Recover(), "a tombstoned session must be refused")
}
