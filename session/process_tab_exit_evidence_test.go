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
	assert.True(t, inst.ConsumeLoadRuntimeReplacement(),
		"the daemon's startup writer persists only enrolled rows")
}

// An account-scoped sibling reconstructed from disk is stopped before restore
// continues. When it is a process tab already held dead, that stop destroys
// pane_dead_status and pane_dead_time — so they are read first.
func TestAccountScopedDeadProcessPaneIsStampedBeforeTheScopeStop(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_4506_scoped_dead"
	processName := agentName + "__deploy"
	cmdExec := nameKeyedExec(map[string]bool{agentName: true, processName: true})
	var commands []string
	baseRun := cmdExec.RunFunc
	cmdExec.RunFunc = func(c *exec.Cmd) error {
		commands = append(commands, c.String())
		return baseRun(c)
	}
	baseOutput := cmdExec.OutputFunc
	cmdExec.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "pane_dead") && strings.Contains(joined, processName) {
			killed := false
			for _, command := range commands {
				killed = killed || (strings.Contains(command, "kill-session") && strings.Contains(command, processName))
			}
			if killed {
				return nil, assertNoSession
			}
			return []byte(paneExitAnswer(c, "1", "7", "1726000000")), nil
		}
		return baseOutput(c)
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
	assert.Equal(t, 1, killed, "the pre-scope pane is still stopped")
	exit := inst.GetTabs()[1].Exit
	require.NotNil(t, exit, "the exit the stop was about to destroy is recorded first")
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

// paneExitAnswer renders a probe answer in whatever field layout the probe asked
// for, so the tests pin the parsed meaning rather than one separator.
func paneExitAnswer(c *exec.Cmd, dead, status, at string) string {
	format := c.Args[len(c.Args)-1]
	if strings.Contains(format, "|") {
		return dead + "|" + status + "|" + at + "\n"
	}
	return dead + " " + status + " " + at + "\n"
}
