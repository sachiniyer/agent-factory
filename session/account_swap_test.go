package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func accountSwapTestInstance(program string) *Instance {
	return &Instance{
		ID:         "swap-id",
		Title:      "swap",
		Path:       "/repo",
		Program:    program,
		backend:    &LocalBackend{},
		started:    true,
		liveness:   LiveLimitReached,
		inFlightOp: OpRespawning,
		Tabs:       []*Tab{newAgentTab(tmux.NewTmuxSession("swap", program))},
	}
}

func registeredAccountSwapTestInstance(t *testing.T, program, resolved string) *Instance {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{program: resolved}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, program, "work")
	require.NoError(t, err)
	inst := accountSwapTestInstance(program)
	inst.Path = initTempGitRepo(t)
	return inst
}

func TestValidateAccountSwapRefusesConversationSelectors(t *testing.T) {
	for _, tc := range []struct {
		program string
		arg     string
	}{
		{"claude --continue", "--continue"},
		{"claude --resume old-chat", "--resume old-chat"},
		{"claude -r=old-chat", "-r=old-chat"},
		{"claude --session-id old-chat", "--session-id old-chat"},
		{"codex resume old-chat", "resume old-chat"},
		{"codex --model gpt-5 resume old-chat", "resume old-chat"},
		{"codex exec resume --last --model gpt-5", "resume --last"},
		{"codex exec --model gpt-5 resume old-chat", "resume old-chat"},
	} {
		t.Run(tc.program, func(t *testing.T) {
			err := accountSwapTestInstance(tc.program).ValidateAccountSwap("work")
			require.ErrorContains(t, err, "fresh conversation")
			require.ErrorContains(t, err, tc.arg, "the refusal must name the user-pinned selector")
		})
	}
}

func TestValidatedAccountSwapFencesLazyVSCodeStartBeforeCommit(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.ErrorContains(t, inst.TabSpawnBlocked(), "account swap",
		"a preflighted swap must fence an existing VS Code tab from lazily starting its old-identity editor")
	require.True(t, inst.EndLimitResume())
	require.NoError(t, inst.TabSpawnBlocked(),
		"an aborted pre-commit swap must not strand the lazy-start fence")
}

func TestSelectAccountAutomaticallyClearsPriorConversationAndCapture(t *testing.T) {
	inst := accountSwapTestInstance("codex")
	prior := AgentConversationData{Agent: tmux.ProgramCodex, ID: "old-rollout"}
	require.True(t, inst.SetAgentConversation(prior))
	oldRuntime := inst.AgentRuntimeToken()

	_, err := inst.SelectAccountAutomatically("ambient", "work")
	require.NoError(t, err)
	require.True(t, inst.AgentConversation().Empty(),
		"the old account's conversation identity must not survive the durable account boundary")
	require.False(t, inst.SetAgentConversationForRuntime(oldRuntime,
		AgentConversationData{Agent: tmux.ProgramCodex, ID: "late-old-rollout"}),
		"an asynchronous capture from the stopped runtime must not restore its conversation")
}

func TestValidateAccountSwapRefusesSiblingIdentityOverride(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramCodex, "codex")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess,
		tmux: tmux.NewTmuxSession("build", "CODEX_HOME=/other make"),
	})

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, `tab "build"`)
	require.ErrorContains(t, err, "sets an identity or shell-startup variable itself",
		"a command-local assignment runs AFTER the account boundary installed the selected root")
}

func TestValidateAccountSwapRefusesUnprovableSiblingIdentityOverride(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramCodex, "codex")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "wrapped", Name: "wrapped", Kind: TabKindProcess,
		tmux: tmux.NewTmuxSession("wrapped", `sh -c 'CODEX_HOME=/other codex'`),
	})

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, `tab "wrapped"`)
	require.ErrorContains(t, err, "sets an identity or shell-startup variable itself",
		"an interpreter wrapper must not hide an identity assignment from the swap boundary")
}

func TestValidateAccountSwapPreflightsStartupFreeShellReplacement(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "shell", Name: "shell", Kind: TabKindShell,
		tmux: tmux.NewTmuxSession("shell", "/bin/bash"),
	})

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"an ambient shell is replaced by af's startup-file-free account command; validating the predecessor would make that replacement unreachable")
}

func TestAutomaticAccountSwapFailsClosedForDocker(t *testing.T) {
	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.SetBackend(&dockerBackend{})
	require.False(t, inst.SupportsAutomaticAccountSwap(),
		"automatic Docker replacement needs a durable runtime identity and frozen provision plan")
}

func TestValidateAccountSwapMintsAClaudeConversationForEachMove(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	_, err = agentaccount.Register(home, tmux.ProgramClaude, "personal")
	require.NoError(t, err)

	require.NoError(t, inst.ValidateAccountSwap("work"))
	first := inst.accountSwapLaunch.conversation.ID
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	require.True(t, inst.ClearPendingAccountSwap("", "work"))
	inst.EndLimitResume()

	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())
	require.NoError(t, inst.ValidateAccountSwap("personal"))
	second := inst.accountSwapLaunch.conversation.ID
	require.NotEmpty(t, first)
	require.NotEmpty(t, second)
	require.NotEqual(t, first, second,
		"returning to an account-local store must never reuse an earlier Claude session id")
}

func TestValidateAccountSwapPreflightsResolvedScopedLaunch(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude --model sonnet")

	err := inst.ValidateAccountSwap("work")
	require.Error(t, err, "the scoped command must be validated before any old pane is stopped")
	require.ErrorContains(t, err, "--model")
	require.ErrorContains(t, err, "sonnet")
}

func TestValidateAccountSwapPreflightsCloudAuthenticationMode(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, "cloud mode")
	require.ErrorContains(t, err, "CLAUDE_CODE_USE_BEDROCK")
}

func TestValidateAccountSwapRefusesSiblingConversationSelector(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "worker", Name: "worker", Kind: TabKindProcess,
		tmux: tmux.NewTmuxSession("worker", "claude --resume sibling-chat"),
	})

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, `tab "worker"`)
	require.ErrorContains(t, err, "--resume sibling-chat")
}

func TestValidateAccountSwapRefusesRestoredTmuxTabWithoutBinding(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "worker", Name: "worker", Kind: TabKindProcess, Command: "git status --short",
	})

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, `tab "worker"`)
	require.ErrorContains(t, err, "no tmux binding",
		"preflight must reject an unrestorable sibling before the old runtime is stopped")
}

type failAccountSwapProcessPty struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
	name    string
}

func (p failAccountSwapProcessPty) Start(cmd *exec.Cmd) (*os.File, error) {
	if strings.Contains(cmd.String(), "new-session") && strings.Contains(cmd.String(), p.name) {
		return nil, fmt.Errorf("process restart refused")
	}
	f, err := os.CreateTemp(p.t.TempDir(), "pty-")
	if err == nil {
		_ = p.cmdExec.Run(cmd)
	}
	return f, err
}

func (p failAccountSwapProcessPty) Close() {}

func shortLivedProcessExec(processName string) cmd_test.MockCmdExec {
	var newSessions int
	inner := countingExec(map[string]bool{}, &newSessions)
	spawned := false
	probesAfterSpawn := 0
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			command := cmd.String()
			if strings.Contains(command, processName) {
				switch {
				case strings.Contains(command, "new-session"):
					spawned = true
				case strings.Contains(command, "has-session") && spawned:
					probesAfterSpawn++
					if probesAfterSpawn > 2 {
						return assertNoSession
					}
				}
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
}

func TestRespawnForAccountSwapAcceptsProcessThatExitsAfterSuccessfulRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_swap_short_process"
	processName := agentName + tmuxTabSeparator + shellTabName
	executor := shortLivedProcessExec(processName)
	inst := lostInstanceForRecover(t, agentName, processName, executor)
	inst.mu.Lock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].Command = "true"
	inst.Tabs[1].tmux.SetProgram("true")
	inst.mu.Unlock()
	inst.Path = initTempGitRepo(t)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())
	require.NoError(t, inst.ValidateAccountSwap("work"))
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	require.NoError(t, inst.RespawnForAccountSwap(),
		"a process that started under the selected account may complete before the replacement is validated")
	require.NoError(t, inst.ValidateAccountSwapReplacementPanes())
	stored := inst.ToInstanceData()
	require.True(t, stored.PendingAccountSwap.ReplacementPanesStarted,
		"successful pane starts must be durable proof for crash recovery")
	restored, err := FromInstanceData(stored)
	require.NoError(t, err)
	require.NoError(t, restored.ValidateAccountSwapReplacementPanes(),
		"a restart must not reinterpret a completed process as a failed launch")
}

func TestRespawnForAccountSwapPropagatesSiblingRestartFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	const agentName = "af_swap_restart"
	const processName = agentName + "__build"
	var newSessions int
	executor := countingExec(map[string]bool{}, &newSessions)
	inst := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName, executor)
	inst.mu.Lock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].Command = "git status --short"
	inst.Tabs[1].tmux.SetProgram("git status --short")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess, Command: "git status --short",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(processName, "git status --short",
			failAccountSwapProcessPty{t: t, cmdExec: executor, name: processName}, executor),
	})
	inst.mu.Unlock()
	inst.Path = initTempGitRepo(t)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())
	require.NoError(t, inst.ValidateAccountSwap("work"))
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	err = inst.RespawnForAccountSwap()
	require.ErrorContains(t, err, `tab "build"`)
	require.False(t, inst.TabAlive(0),
		"a partially restored account boundary must not leave its new agent running")
}

func TestRespawnForAccountSwapUsesThePreflightedProgramSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_swap_frozen_launch"
	var newSessions int
	var spawns []string
	inst := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	inst.mu.Lock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].Command = "git status --short"
	inst.Tabs[1].tmux.SetProgram("git status --short")
	inst.mu.Unlock()
	inst.Path = initTempGitRepo(t)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())
	require.NoError(t, inst.ValidateAccountSwap("work"))

	cfg.ProgramOverrides[tmux.ProgramClaude] = "claude --model changed-after-preflight"
	require.NoError(t, config.SaveConfig(cfg))
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	require.NoError(t, inst.RespawnForAccountSwap())
	require.NotEmpty(t, spawns)
	require.NotContains(t, spawns[0], "changed-after-preflight",
		"the stopped runtime must be replaced with the exact command admitted by preflight")
}

func TestStopForAccountSwapStopsEveryCredentialBearingPane(t *testing.T) {
	var mu sync.Mutex
	var killed []string
	inner := nameKeyedExec(map[string]bool{
		"af_swap":        true,
		"af_swap__shell": true,
		"af_swap__build": true,
	})
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				mu.Lock()
				killed = append(killed, strings.Join(cmd.Args, " "))
				mu.Unlock()
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	inst := lostInstanceForRecover(t, "af_swap", "af_swap__shell", cmdExec)
	inst.mu.Lock()
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess, Command: "make",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_swap__build", "make", nil, cmdExec),
	})
	inst.mu.Unlock()
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())

	require.NoError(t, inst.StopForAccountSwap())
	mu.Lock()
	joined := strings.Join(killed, "\n")
	mu.Unlock()
	for _, name := range []string{"af_swap", "af_swap__shell", "af_swap__build"} {
		require.Contains(t, joined, name, "every credential-bearing pane must be stopped before identity commit")
	}
}

func TestStopForAccountSwapDoesNotStopSiblingsAfterAgentTeardownFails(t *testing.T) {
	var mu sync.Mutex
	var killed []string
	inner := nameKeyedExec(map[string]bool{
		"af_swap":        true,
		"af_swap__shell": true,
	})
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				joined := strings.Join(cmd.Args, " ")
				mu.Lock()
				killed = append(killed, joined)
				mu.Unlock()
				if strings.Contains(joined, "af_swap") && !strings.Contains(joined, "af_swap__shell") {
					return fmt.Errorf("agent teardown could not be confirmed")
				}
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	inst := lostInstanceForRecover(t, "af_swap", "af_swap__shell", cmdExec)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())

	err := inst.StopForAccountSwap()
	require.ErrorContains(t, err, "agent")
	mu.Lock()
	joined := strings.Join(killed, "\n")
	mu.Unlock()
	require.Contains(t, joined, "af_swap", "the agent teardown must be attempted first")
	require.NotContains(t, joined, "af_swap__shell",
		"an ordinary-resume fallback needs the untouched sibling when the agent teardown is unconfirmed")
}

func TestPendingAccountSwapFencesArchiveAndHandoffButAllowsDelivery(t *testing.T) {
	newPending := func() *Instance {
		inst := accountSwapTestInstance("claude")
		_, err := inst.SelectAccountAutomatically("ambient", "work")
		require.NoError(t, err)
		inst.inFlightOp = OpNone
		return inst
	}

	archive := newPending()
	require.Equal(t, LifecycleActionNone, archive.LifecycleAction())
	require.ErrorContains(t, archive.Transition(BeginArchive()), "account swap")

	handoff := newPending()
	require.ErrorContains(t, handoff.ValidateRuntimeAction(RuntimeActionHandoff), "account swap")
	require.NoError(t, handoff.ValidateRuntimeAction(RuntimeActionResumeLimit),
		"the pending notice must remain deliverable")

	tabSpawn := newPending()
	require.ErrorContains(t, tabSpawn.TabSpawnBlocked(), "account swap",
		"a durable identity change must fence new credential-bearing panes until replacement completes")
}

type captureAccountSwapEnvironmentPty struct {
	cmd *exec.Cmd
}

func (p *captureAccountSwapEnvironmentPty) Start(command *exec.Cmd) (*os.File, error) {
	p.cmd = command
	return nil, fmt.Errorf("stop after capturing recovered launch environment")
}

func (*captureAccountSwapEnvironmentPty) Close() {}

func TestSynchronizeAccountSwapRuntimeMetadataRestoresSessionEnvPassthrough(t *testing.T) {
	const passthrough = "AF_TEST_ACCOUNT_SWAP_RECOVERY_TOKEN"
	t.Setenv(passthrough, "recovered-value")
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.SessionEnvPassthrough = []string{passthrough}
	require.NoError(t, config.SaveConfig(cfg))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	_, err = inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	inst.EndLimitResume()
	stored := inst.ToInstanceData().ForStorage()
	stored.Worktree = GitWorktreeData{
		RepoPath: inst.Path, WorktreePath: inst.Path,
		SessionName: inst.Title, BranchName: "main", ExternalWorktree: true,
	}
	restored, err := FromInstanceData(stored)
	require.NoError(t, err)

	pty := &captureAccountSwapEnvironmentPty{}
	execu := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return fmt.Errorf("session not found") },
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	recoveredTmux := tmux.NewTmuxSessionWithDeps("recovered-account-swap", "claude", pty, execu)
	restored.mu.Lock()
	restored.Tabs[0].tmux = recoveredTmux
	restored.mu.Unlock()

	require.NoError(t, restored.SynchronizeAccountSwapRuntimeMetadata())
	require.Error(t, recoveredTmux.Start(t.TempDir()))
	require.NotNil(t, pty.cmd, "the recovered pane never reached its launch environment")
	require.Contains(t, pty.cmd.Env, passthrough+"=recovered-value",
		"retiring the recovery marker must not make later tabs forget configured passthrough variables")
}
