package session

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

func accountRecoverRefusingSiblingExec(shellName string, commands *[]string) cmd_test.MockCmdExec {
	inner := nameKeyedExec(map[string]bool{shellName: true})
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			text := cmd.String()
			*commands = append(*commands, text)
			if strings.Contains(text, "kill-session") && strings.Contains(text, shellName) {
				return errors.New("persisted sibling did not stop")
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
}

func accountLostInstanceForRecover(
	t *testing.T,
	repoRoot, worktreePath, branch, agentName, shellName string,
	cmdExec cmd_test.MockCmdExec,
) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	branchCreatedByUs := true
	data := InstanceData{
		Title:    "account-recover-failure",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "account-recover-failure",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}
	instance, err := FromInstanceData(data)
	require.NoError(t, err)
	return instance
}

func TestRecover_AccountTabFailureStopsSpawnedAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_account_recover_failure"
	shellName := agentName + shellTmuxSuffix
	worktreePath := t.TempDir()
	var commands []string
	cmdExec := accountRecoverRefusingSiblingExec(shellName, &commands)
	instance := accountLostInstanceForRecover(
		t, "/tmp/account-recover-failure-repo", worktreePath, "af/recover-failure",
		agentName, shellName, cmdExec,
	)

	err := instance.Recover()
	require.Error(t, err)
	require.Contains(t, err.Error(), "stop the pre-scope process")
	require.False(t, instance.TabAlive(0),
		"a tab recovery refusal after agent spawn must stop the unconfirmed agent")
	require.True(t, commandIncludesSession(commands, "kill-session", agentName),
		"the failure path must explicitly tear down the newly spawned agent")
}

func TestRecover_AccountTabFailureAfterRebuildKeepsCommittedMarker(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/account-recover-rebuilt"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_account_recover_rebuilt"
	shellName := agentName + shellTmuxSuffix
	worktreePath := filepath.Join(t.TempDir(), "missing-worktree")
	var commands []string
	cmdExec := accountRecoverRefusingSiblingExec(shellName, &commands)
	instance := accountLostInstanceForRecover(
		t, repoRoot, worktreePath, branch, agentName, shellName, cmdExec,
	)

	err := instance.Recover()
	require.Error(t, err)
	require.DirExists(t, worktreePath, "the recovery must have committed its worktree rebuild")
	var rebuilt *RecoverRebuiltWorkspaceError
	require.ErrorAs(t, err, &rebuilt,
		"a later tab refusal must preserve the committed-rebuild outcome")
	require.False(t, instance.TabAlive(0),
		"the committed rebuild does not make the partially recovered agent safe to leave running")
}

func TestLoad_AccountTabFailureStopsAgentBeforeDiscard(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_load_failure"
	shellName := agentName + shellTmuxSuffix
	inner := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			text := cmd.String()
			commands = append(commands, text)
			if strings.Contains(text, "kill-session") && strings.Contains(text, shellName) {
				return errors.New("persisted sibling did not stop")
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	worktreePath := t.TempDir()
	_, err := FromInstanceData(InstanceData{
		Title:    "account-load-failure",
		Path:     "/tmp/account-load-failure-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-load-failure-repo",
			WorktreePath: worktreePath,
			SessionName:  "account-load-failure",
			BranchName:   "af/account-load-failure",
		},
	})
	require.Error(t, err)
	require.True(t, commandIncludesSession(commands, "kill-session", agentName),
		"a load error that discards the record must first stop its reattached agent")
}

func TestLoad_AccountLaterTabFailureStopsEarlierRespawnedSibling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_partial_load"
	firstName := agentName + "__first"
	secondName := agentName + "__second"
	inner := nameKeyedExec(map[string]bool{agentName: true, secondName: true})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			text := cmd.String()
			commands = append(commands, text)
			if strings.Contains(text, "kill-session") && strings.Contains(text, secondName) {
				return errors.New("later persisted sibling did not stop")
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	worktreePath := t.TempDir()
	_, err := FromInstanceData(InstanceData{
		Title:    "account-partial-load",
		Path:     "/tmp/account-partial-load-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: "first", Kind: TabKindShell, TmuxName: firstName},
			{Name: "second", Kind: TabKindProcess, Command: "make", TmuxName: secondName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-partial-load-repo",
			WorktreePath: worktreePath,
			SessionName:  "account-partial-load",
			BranchName:   "af/account-partial-load",
		},
	})
	require.Error(t, err)
	require.True(t, commandIncludesSession(commands, "new-session", firstName),
		"the first missing sibling must reproduce as respawned before the later failure")
	require.True(t, commandIncludesSession(commands, "kill-session", firstName),
		"a later restore failure must tear down every sibling this discarded load already respawned")
}

func TestLoad_AccountShellPreparationFailureStopsPreScopeSibling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_shell_prepare_failure"
	shellName := agentName + shellTmuxSuffix
	inner := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			commands = append(commands, cmd.String())
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	_, err := FromInstanceData(InstanceData{
		Title:    "account-shell-prepare-failure",
		Path:     "/tmp/account-shell-prepare-failure-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-shell-prepare-failure-repo",
			WorktreePath: t.TempDir(),
			SessionName:  "account-shell-prepare-failure",
			BranchName:   "af/account-shell-prepare-failure",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no credential-safe account launch mode")
	require.True(t, commandIncludesSession(commands, "kill-session", shellName),
		"a persisted pre-scope sibling must be stopped before shell preparation refuses the load")
}

func TestLoad_AccountUnknownSiblingProbeRetainsInertRecord(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_unknown_probe"
	shellName := agentName + shellTmuxSuffix
	inner := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			commands = append(commands, cmd.String())
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previousRestore := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previousRestore })
	previousProbe := probeRestoredTabSession
	probeRestoredTabSession = func(session *tmux.TmuxSession) (bool, bool) {
		if session.SanitizedName() == shellName {
			return false, false
		}
		return session.ProbeSession()
	}
	t.Cleanup(func() { probeRestoredTabSession = previousProbe })

	instance, err := FromInstanceData(InstanceData{
		ID:       "account-unknown-probe-id",
		Title:    "account-unknown-probe",
		Path:     "/tmp/account-unknown-probe-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-unknown-probe-repo",
			WorktreePath: t.TempDir(),
			SessionName:  "account-unknown-probe",
			BranchName:   "af/account-unknown-probe",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, instance)
	require.True(t, instance.StartupStateUnknown(), "the retained row must stay inert until explicit teardown")
	require.False(t, instance.Started())
	require.True(t, instance.CanKill(), "the retained row must keep an explicit cleanup action")
	stored := instance.ToInstanceData()
	require.True(t, stored.StartupStateUnknown, "the retention marker must survive the next storage projection")
	require.Len(t, stored.Tabs, 2)
	require.Equal(t, shellName, stored.Tabs[1].TmuxName, "the unknown sibling cleanup handle must survive")
	require.False(t, commandIncludesSession(commands, "kill-session", shellName),
		"an inconclusive probe is not permission to kill the sibling")
}

func TestLoad_AccountAgentEnvironmentRefreshFailureRetainsHandle(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_refresh_failure"
	inner := nameKeyedExec(map[string]bool{agentName: true})
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "set-environment") {
				return errors.New("tmux session environment refused update")
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	instance, err := FromInstanceData(InstanceData{
		ID:       "account-refresh-failure-id",
		Title:    "account-refresh-failure",
		Path:     "/tmp/account-refresh-failure-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs:     []TabData{{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName}},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-refresh-failure-repo",
			WorktreePath: t.TempDir(),
			SessionName:  "account-refresh-failure",
			BranchName:   "af/account-refresh-failure",
		},
	})
	require.NoError(t, err)
	require.True(t, instance.StartupStateUnknown())
	require.False(t, instance.Started())
	stored := instance.ToInstanceData()
	require.True(t, stored.StartupStateUnknown)
	require.Len(t, stored.Tabs, 1)
	require.Equal(t, agentName, stored.Tabs[0].TmuxName,
		"a failed in-place scope upgrade must retain the live agent cleanup handle")
}

func TestLoad_AccountRestoreRaceStopsReattachedPreScopeSibling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_account_restore_race"
	shellName := agentName + shellTmuxSuffix
	inner := nameKeyedExec(map[string]bool{agentName: true})
	var commands []string
	probeCount := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			text := cmd.String()
			commands = append(commands, text)
			if strings.Contains(text, "has-session") && strings.Contains(text, shellName) {
				probeCount++
				if probeCount == 1 {
					return assertNoSession
				}
				return nil
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	_, err := FromInstanceData(InstanceData{
		Title:    "account-restore-race",
		Path:     "/tmp/account-restore-race-repo",
		Program:  tmux.ProgramCodex,
		Account:  "work",
		Status:   Running,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     "/tmp/account-restore-race-repo",
			WorktreePath: t.TempDir(),
			SessionName:  "account-restore-race",
			BranchName:   "af/account-restore-race",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "reattached a pre-scope process")
	require.True(t, commandIncludesSession(commands, "kill-session", shellName),
		"a pre-scope process reattached during the probe race must be stopped before the record is discarded")
}

func commandIncludesSession(commands []string, operation, sessionName string) bool {
	for _, command := range commands {
		if !strings.Contains(command, operation) {
			continue
		}
		for _, field := range strings.Fields(command) {
			field = strings.Trim(field, "'\"")
			if field == "="+sessionName || field == "="+sessionName+":" {
				return true
			}
		}
	}
	return false
}
