package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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
			require.ErrorContains(t, err, "must choose which conversation the replacement opens")
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

// A process tab's command is not a replacement command: the swap stops the tab
// and never relaunches it (#4479), so an identity assignment in that command —
// direct or wrapped — can no longer run after the account boundary, and must not
// refuse the swap (#4506 review). A relaunched sibling is a shell, whose
// replacement is af's own startup-free command.
func TestValidateAccountSwapAdmitsAProcessTabThatSetsAnIdentity(t *testing.T) {
	for _, command := range []string{"CLAUDE_CONFIG_DIR=/other make", `sh -c 'CLAUDE_CONFIG_DIR=/other claude'`} {
		t.Run(command, func(t *testing.T) {
			inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
			inst.Tabs = append(inst.Tabs, &Tab{
				ID: "build", Name: "build", Kind: TabKindProcess,
				tmux: tmux.NewTmuxSession("build", command),
			})

			require.NoError(t, inst.ValidateAccountSwap("work"),
				"the swap stops this command; nothing relaunches it under the selected account")
		})
	}
}

// A committed manual swap's retry must resolve the replacement account inside
// the namespace the transaction recorded at commit — not the namespace CURRENT
// configuration derives (#4430 review). Commit under aider→codex records
// AccountAgent=codex; a restart after the override flips to aider→gemini must
// still consult the codex registry, where the drift check then names the real
// mismatch instead of silently binding a gemini-launched pane to a codex
// credential. The un-pinned path would find the decoy "work" in gemini's
// registry and pass the drift check — the exact wrong-namespace recovery the
// durable field exists to prevent.
func TestValidateAccountSwapCommittedRetryUsesRecordedNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramAider: tmux.ProgramCodex}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)

	inst := accountSwapTestInstance(tmux.ProgramAider)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	_, err = inst.SelectAccountForHandoff("", "work", tmux.ProgramAider, tmux.ProgramCodex, true,
		HandoffReasonManual, "head", "")
	require.NoError(t, err)
	require.Equal(t, tmux.ProgramCodex, inst.PendingAccountSwapAgent(),
		"the committed transaction must carry the namespace the account was selected in")
	require.Equal(t, tmux.ProgramCodex, inst.ToInstanceData().PendingAccountSwap.AccountAgent,
		"the namespace must survive a daemon restart — it is the only answer a config flip cannot move")

	// The restart's view: the pane runs the committed codex command (the commit
	// replaced it before the checkpoint), the override moved, and a decoy
	// registration now exists in the namespace current config would resolve.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramCodex))
	cfg.ProgramOverrides = map[string]string{tmux.ProgramAider: tmux.ProgramGemini}
	require.NoError(t, config.SaveConfig(cfg))
	_, err = agentaccount.Register(home, tmux.ProgramGemini, "work")
	require.NoError(t, err)

	// The retry consults the recorded codex registry AND the committed codex
	// command: the pair is consistent, so the swap proceeds — and the decoy
	// gemini/work the flipped resolution would have found is never consulted.
	require.NoError(t, inst.ValidateAccountSwap("work"),
		"the committed retry keeps the command the checkpoint launched, not the flipped override")
	require.NotNil(t, inst.accountSwapLaunch)
	require.True(t, strings.HasPrefix(inst.accountSwapLaunch.program, tmux.ProgramCodex),
		"the retry must relaunch codex, got %q", inst.accountSwapLaunch.program)

	// If the pane evidence itself has drifted — the checkpoint's replacement
	// ran under the flipped override — the pinned namespace still refuses
	// rather than binding a gemini-launched pane to a codex credential.
	drifted := accountSwapTestInstance(tmux.ProgramAider)
	drifted.Path = initTempGitRepo(t)
	driftedGw, err := sessiongit.NewGitWorktreeFromStorage(drifted.Path, drifted.Path, drifted.Title, "main", "", false, true)
	require.NoError(t, err)
	drifted.SetGitWorktreeForTest(driftedGw)
	_, err = drifted.SelectAccountForHandoff("", "work", tmux.ProgramAider, tmux.ProgramCodex, true,
		HandoffReasonManual, "head", "")
	require.NoError(t, err)
	drifted.SetTmuxSession(tmux.NewTmuxSession(drifted.Title, tmux.ProgramGemini))
	err = drifted.ValidateAccountSwap("work")
	require.ErrorContains(t, err, "is a codex account",
		"the retry must consult the recorded codex registry, not the flipped resolution's gemini")
	require.ErrorContains(t, err, "runs gemini",
		"the drift refusal names the launch the pane evidence now produces")
}

// A committed CROSS-agent swap recovered between its identity checkpoint and
// the replacement's first launch finds pane/runtime evidence describing the
// OUTGOING agent. Freezing that command and checking it against the committed
// AccountAgent namespace refuses every retry and strands the session (#4430
// review round 7). The transaction records the frozen incoming command at
// commit; a record written before that field existed instead re-resolves the
// committed ledger target — but never the predecessor's runtime.
func TestValidateAccountSwapCommittedCrossAgentRetryLaunchesRecordedProgram(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramClaude: tmux.ProgramClaude,
		tmux.ProgramCodex:  tmux.ProgramCodex,
	}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramCodex, "work")
	require.NoError(t, err)

	newCommittedSession := func(t *testing.T, withFrozenPlan bool) *Instance {
		inst := accountSwapTestInstance(tmux.ProgramClaude)
		inst.Path = initTempGitRepo(t)
		gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
		require.NoError(t, err)
		inst.SetGitWorktreeForTest(gw)
		if withFrozenPlan {
			// The admission-time launch plan is what commit copies into the
			// durable record: base is the resolved incoming command before
			// conversation injection.
			inst.accountSwapLaunch = &accountSwapLaunchPlan{
				account: "work", base: tmux.ProgramCodex, program: tmux.ProgramCodex,
			}
		}
		_, err = inst.SelectAccountForHandoff("", "work", tmux.ProgramCodex, tmux.ProgramCodex, true,
			HandoffReasonManual, "head", "")
		require.NoError(t, err)
		require.Equal(t, tmux.ProgramCodex, inst.PendingAccountSwapAgent())
		return inst
	}

	// The restart's view, identical for both record shapes: the identity
	// checkpoint landed, the replacement never launched, so pane and runtime
	// evidence still describe the outgoing claude.
	t.Run("frozen program survives restart", func(t *testing.T) {
		inst := newCommittedSession(t, true)
		require.Equal(t, tmux.ProgramCodex, inst.ToInstanceData().PendingAccountSwap.Program,
			"the committed incoming command must be durable beside the namespace")
		inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
		require.NoError(t, inst.ValidateAccountSwap("work"),
			"the retry must launch the committed codex command, not freeze the predecessor's claude")
		require.NotNil(t, inst.accountSwapLaunch)
		require.True(t, strings.HasPrefix(inst.accountSwapLaunch.program, tmux.ProgramCodex),
			"the retry must relaunch codex, got %q", inst.accountSwapLaunch.program)
	})

	t.Run("legacy record re-resolves the committed target", func(t *testing.T) {
		inst := newCommittedSession(t, false)
		require.Empty(t, inst.ToInstanceData().PendingAccountSwap.Program)
		inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, tmux.ProgramClaude))
		require.NoError(t, inst.ValidateAccountSwap("work"),
			"a pre-Program record must recover through the committed ledger target, not the predecessor's pane")
		require.NotNil(t, inst.accountSwapLaunch)
		require.True(t, strings.HasPrefix(inst.accountSwapLaunch.program, tmux.ProgramCodex),
			"the retry must relaunch codex, got %q", inst.accountSwapLaunch.program)
	})
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
	// The pane runs the command the override resolved to — an account swap
	// validates THAT command, not the bare enum.
	inst.SetTmuxSession(tmux.NewTmuxSession(inst.Title, "claude --model sonnet"))

	err := inst.ValidateAccountSwap("work")
	require.Error(t, err, "the scoped command must be validated before any old pane is stopped")
	require.ErrorContains(t, err, "--model")
	require.ErrorContains(t, err, "sonnet")
}

func TestValidateManualAccountSwapPreflightsMissingUnchangedBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()+":/usr/bin:/bin")
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	inst.Account = "ambient"
	inst.preResolvedProgram = "claude"
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	err = inst.ValidateManualAccountSwap("work", tmux.ProgramClaude, false)
	require.ErrorContains(t, err, "launch preflight")
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(), "admission must leave the outgoing runtime untouched")
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
}

// A process tab whose binary has since disappeared cannot block a manual swap:
// the swap only stops its pane (#4506 review).
func TestValidateManualAccountSwapAdmitsAProcessTabWhoseBinaryIsGone(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramClaude), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, tmux.ProgramClaude)
	inst.liveness = LiveRunning
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	missing := filepath.Join(t.TempDir(), "missing-worker")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "worker", Name: "worker", Kind: TabKindProcess, Command: missing,
		tmux: tmux.NewTmuxSession("worker", missing),
	})

	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramClaude, false),
		"a command the swap never relaunches needs no launch preflight")
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
}

func TestValidateManualAccountSwapAcceptsHealthySiblingBinary(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramClaude), []byte("#!/bin/sh\nexit 0\n"), 0700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, tmux.ProgramClaude)
	inst.liveness = LiveRunning
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "worker", Name: "worker", Kind: TabKindProcess, Command: "/usr/bin/true",
		tmux: tmux.NewTmuxSession("worker", "/usr/bin/true"),
	})

	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramClaude, false))
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram())
	require.Nil(t, inst.ToInstanceData().PendingAccountSwap)
}

func TestValidateManualCrossAgentAccountSwapWritesSkillToIncomingAccount(t *testing.T) {
	for _, tc := range []struct {
		agent     string
		skillPath func(string) string
	}{
		{agent: tmux.ProgramCodex, skillPath: codexSkillPathUnder},
		{agent: tmux.ProgramGemini, skillPath: geminiSkillPathUnder},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			bin := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(bin, tc.agent), []byte("#!/bin/sh\nexit 0\n"), 0o700))
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			agentHome(t)
			grantGlobalAgentSkills(t)

			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			cfg.ProgramOverrides = map[string]string{tc.agent: tc.agent}
			require.NoError(t, config.SaveConfig(cfg))
			accountDir := registerAccount(t, tc.agent, "work")

			inst := accountSwapTestInstance(tmux.ProgramClaude)
			inst.Path = initTempGitRepo(t)
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)

			require.NoError(t, inst.ValidateManualAccountSwap("work", tc.agent, true))
			require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
				"validation must not rewrite the outgoing runtime identity")
			require.FileExists(t, tc.skillPath(accountDir),
				"the incoming agent must find the af skill in its selected account root")
		})
	}
}

func TestCheckManualAccountSwapDoesNotRecordLaunchPlan(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, tmux.ProgramClaude), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, tmux.ProgramClaude)
	inst.liveness = LiveRunning
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	require.NoError(t, inst.CheckManualAccountSwap("work", tmux.ProgramClaude, false))
	require.Nil(t, inst.accountSwapLaunch, "the unlocked check must not leave mutation authority behind")
	require.NoError(t, inst.ValidateManualAccountSwap("work", tmux.ProgramClaude, false))
	require.NotNil(t, inst.accountSwapLaunch, "locked admission still records the launch plan")
}

func TestValidateAccountSwapPreflightsCloudAuthenticationMode(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, "cloud mode")
	require.ErrorContains(t, err, "CLAUDE_CODE_USE_BEDROCK")
}

func TestValidateAccountSwapAdmitsAProcessTabWithAConversationSelector(t *testing.T) {
	inst := registeredAccountSwapTestInstance(t, tmux.ProgramClaude, "claude")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "worker", Name: "worker", Kind: TabKindProcess,
		tmux: tmux.NewTmuxSession("worker", "claude --resume sibling-chat"),
	})

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"the swap stops this tab and never relaunches it, so its conversation pins nothing (#4506 review)")
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
		ID: "build", Name: "build", Kind: TabKindShell, Command: "/bin/sh",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(processName, "/bin/sh",
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

// TestPendingAccountSwapHandoffAdmitsOnlySameTargetRetry is the #4393 deadlock
// regression: a session whose committed account swap never delivered is
// permanently bricked — every lifecycle action refuses on the pending marker,
// and the refusal's own remedy ("retry that account swap") is itself a refused
// lifecycle action. A handoff naming the swap's committed account (agent
// explicit or inherited) IS that retry: the pending-swap axis cannot refuse it.
// Every other axis — and every other target — still applies.
func TestPendingAccountSwapHandoffAdmitsOnlySameTargetRetry(t *testing.T) {
	newPending := func() *Instance {
		inst := accountSwapTestInstance("claude")
		_, err := inst.SelectAccountForHandoff("ambient", "work", "claude", "claude", false, HandoffReasonManual, "", "continue the mission")
		require.NoError(t, err)
		inst.inFlightOp = OpNone
		return inst
	}

	retry := newPending()
	require.ErrorContains(t, retry.ValidateRuntimeAction(RuntimeActionHandoff), "account swap",
		"the unqualified handoff check keeps the blanket pending-swap refusal")
	require.NoError(t, retry.ValidateHandoffRuntimeAction("", "work"),
		"retrying the committed account is the remedy the refusal advertises")
	require.NoError(t, retry.ValidateHandoffRuntimeAction("claude", "work"),
		"an explicit agent equal to the recorded one is the same retry")

	// A different account, a different agent, or no account at all is another
	// transaction the committed swap still owns.
	require.ErrorContains(t, newPending().ValidateHandoffRuntimeAction("", "personal"), "account swap")
	require.ErrorContains(t, newPending().ValidateHandoffRuntimeAction("codex", "work"), "account swap")
	require.ErrorContains(t, newPending().ValidateHandoffRuntimeAction("", ""), "account swap")

	// A redirected swap (`--to aider` resolving to codex) commits a codex
	// pane while Program records the requested aider enum. The retry the
	// refusal advertises may spell the committed target EITHER way — the
	// requested enum or the resolved agent — because both name the same
	// committed transaction (#4430 review round 3).
	redirected := accountSwapTestInstance(tmux.ProgramAider)
	redirected.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("swap", tmux.ProgramCodex))}
	_, selectErr := redirected.SelectAccountForHandoff("ambient", "work", tmux.ProgramAider, tmux.ProgramCodex, false, HandoffReasonManual, "", "continue the mission")
	require.NoError(t, selectErr)
	redirected.inFlightOp = OpNone
	require.NoError(t, redirected.ValidateHandoffRuntimeAction(tmux.ProgramAider, "work"),
		"the retry may name the requested enum even though the committed pane runs codex")
	require.NoError(t, redirected.ValidateHandoffRuntimeAction(tmux.ProgramCodex, "work"),
		"or the resolved agent the pane actually runs")
	require.ErrorContains(t, redirected.ValidateHandoffRuntimeAction(tmux.ProgramGemini, "work"), "account swap",
		"a third agent names another transaction and stays fenced")

	// The same goes for an automatic swap: its committed target is retryable,
	// and only that target.
	auto := accountSwapTestInstance("claude")
	_, err := auto.SelectAccountAutomatically("ambient", "work")
	require.NoError(t, err)
	auto.inFlightOp = OpNone
	require.NoError(t, auto.ValidateHandoffRuntimeAction("", "work"))
	require.ErrorContains(t, auto.ValidateHandoffRuntimeAction("", "personal"), "account swap")

	// A pending marker whose target was never committed is no retry either —
	// the identity the request names has not moved, so the row stays fenced.
	stale := newPending()
	stale.Account = "ambient"
	require.ErrorContains(t, stale.ValidateHandoffRuntimeAction("", "work"), "account swap")

	// The exemption clears only the pending-swap axis: a pending swap on a lost
	// session still refuses on liveness, and an in-flight operation still
	// refuses on the op fence.
	lost := newPending()
	lost.liveness = LiveLost
	require.ErrorContains(t, lost.ValidateHandoffRuntimeAction("", "work"), "restore it first")

	busy := newPending()
	busy.inFlightOp = OpReplacing
	require.ErrorContains(t, busy.ValidateHandoffRuntimeAction("", "work"), "busy")
}

type captureAccountSwapEnvironmentPty struct {
	cmd *exec.Cmd
}

func (p *captureAccountSwapEnvironmentPty) Start(command *exec.Cmd) (*os.File, error) {
	p.cmd = command
	return nil, fmt.Errorf("stop after capturing recovered launch environment")
}

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
