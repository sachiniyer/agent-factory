package session

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #4506 review: the exit evidence a process tab carries must survive every path
// that can lose it — the startup write, the account-scope stop, and the root
// heal's roster carry — and must never invent a completion time.

// A stamp discovered while the daemon loads must enroll the row for the load
// checkpoint: nothing else writes it, and a reboot before any later mutation
// would lose both the pane and the only record of how the command ended.
func TestProcessTabExitStampEnrollsTheLoadCheckpoint(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(*exec.Cmd) error { return nil },
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "pane_dead") {
				return []byte(paneExitAnswer(c, "1", "42", "1726000000")), nil
			}
			return nil, nil
		},
	}
	inst, _, _ := processTabRestoreInstance(t, cmdExec)

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	require.NotNil(t, inst.Tabs[1].Exit)
	replacement := inst.ConsumeLoadRuntimeReplacement()
	assert.True(t, replacement.Replaced,
		"the daemon's startup writer persists only enrolled rows")
	assert.False(t, replacement.Agent,
		"a process tab's exit is not a replacement of the task-owning agent runtime")
}

// An account-scoped sibling reconstructed from disk is stopped before restore
// continues. A process tab already held dead, with nothing of it still running,
// has nothing to stop: it is kept, and its exit status and death time are
// recorded rather than destroyed.
func TestAccountScopedFinishedProcessPaneIsKeptWithItsExit(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_4506_scoped_dead"
	processName := agentName + "__deploy"
	// The mock answers the finished pane the way tmux does: its pane pid is
	// absent from the process table.
	cmdExec := nameKeyedExecWithFinishedPanes(map[string]bool{agentName: true, processName: true},
		map[string]finishedPane{processName: {status: "7", at: "1726000000"}})
	var commands []string
	baseRun := cmdExec.RunFunc
	cmdExec.RunFunc = func(c *exec.Cmd) error {
		commands = append(commands, c.String())
		return baseRun(c)
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	gw, err := git.NewGitWorktreeFromStorage("/tmp/4506-scoped-dead-repo", t.TempDir(), "scoped-dead",
		"scoped-dead-branch", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title:       "scoped-dead",
		Path:        "/tmp/4506-scoped-dead-repo",
		Program:     "codex",
		Account:     "work",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs: []*Tab{
			newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "codex", pty, cmdExec)),
			{ID: "deploy", Name: "deploy", Kind: TabKindProcess, Command: "./deploy.sh",
				tmux:                          tmux.NewTmuxSessionFromSanitizedNameWithDeps(processName, "./deploy.sh", pty, cmdExec),
				accountScopeProvenanceUnknown: true},
		},
	}

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	killed := 0
	for _, command := range commands {
		if strings.Contains(command, "kill-session") && strings.Contains(command, processName) {
			killed++
		}
	}
	assert.Zero(t, killed, "a finished pane runs nothing on any identity, so its output is kept")
	exit := inst.GetTabs()[1].Exit
	require.NotNil(t, exit, "the finished pane's exit is recorded")
	assert.Equal(t, 7, exit.Status)
	assert.True(t, exit.StatusKnown)
	assert.Equal(t, time.Unix(1726000000, 0), exit.At)
}

// An account swap stops a process tab and never relaunches it, so the command
// it ran is not a replacement command and cannot be a reason to refuse the swap.
func TestValidateAccountSwapIgnoresACommandThatWillNotRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_4506_swap_process"
	var newSessions int
	inst := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+"review",
		countingExec(map[string]bool{}, &newSessions))
	inst.mu.Lock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].Command = "claude --resume old-chat"
	inst.Tabs[1].tmux.SetProgram("claude --resume old-chat")
	inst.mu.Unlock()
	inst.Path = initTempGitRepo(t)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())

	require.NoError(t, inst.ValidateAccountSwap("work"),
		"a process tab is stopped, not relaunched, so its conversation arguments pin nothing")
}

// The canary for the narrowed check: skipping a process tab's relaunch checks
// must not skip the one check its STOP depends on — a pane af cannot address.
func TestValidateAccountSwapStillRefusesAnUnaddressableProcessPane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_4506_swap_nil_binding"
	var newSessions int
	inst := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+"review",
		countingExec(map[string]bool{}, &newSessions))
	inst.mu.Lock()
	inst.Tabs[1].Kind = TabKindProcess
	inst.Tabs[1].tmux = nil
	inst.mu.Unlock()
	inst.Path = initTempGitRepo(t)
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())

	require.ErrorContains(t, inst.ValidateAccountSwap("work"), "no tmux binding",
		"a process pane af cannot address cannot be proven stopped, so the refusal stays")
}

// The root heal rebuilds the reaped root's roster from its record; a finished
// process tab's recorded exit is part of that roster, and the reap just killed
// the session it could otherwise have been re-read from.
func TestRestoreCarriedTabsKeepsRecordedExit(t *testing.T) {
	when := time.Unix(1726000000, 0).UTC()
	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_4506_root", "bash",
		persistPtyFactory{t: t, cmdExec: nameKeyedExec(map[string]bool{})}, nameKeyedExec(map[string]bool{}))
	inst := &Instance{
		Title: "root",
		Tabs:  []*Tab{newAgentTab(agentTs)},
		carriedTabs: []TabData{
			{ID: "tab-agent", Name: agentTabName, Kind: TabKindAgent},
			{ID: "tab-deploy", Name: "deploy", Kind: TabKindProcess, Command: "./deploy.sh",
				TmuxName: "af_4506_root__deploy",
				Exit:     &TabExitData{Status: 3, StatusKnown: true, At: when}},
		},
	}

	inst.restoreCarriedTabs()
	require.Len(t, inst.Tabs, 2)
	require.NotNil(t, inst.Tabs[1].Exit, "the carried roster must keep the tab's recorded completion")
	assert.Equal(t, TabExit{Status: 3, StatusKnown: true, At: when}, *inst.Tabs[1].Exit)
}

// A dead pane tmux reported no death time for keeps an unknown time: the wire
// must not present year one as when the command finished.
func TestTabExitDataOmitsAnUnknownTime(t *testing.T) {
	raw, err := json.Marshal(TabExitData{Status: 0, StatusKnown: true})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"at"`)
	assert.NotContains(t, string(raw), "0001-01-01")

	when := time.Unix(1726000000, 0).UTC()
	raw, err = json.Marshal(TabExitData{StatusKnown: true, At: when})
	require.NoError(t, err)
	var back TabExitData
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.True(t, back.At.Equal(when), "a known time still round-trips")
}
