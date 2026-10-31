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

// A user who attaches to a process tab's session can split the window for a
// shell and exit it; that split pane inherits remain-on-exit, is held dead, and
// stays the session's active pane. Before the fix the roster read's probe read
// that active pane's collected status and stamped it as the recorded command's
// exit while the command kept running — a wrong, first-wins, uncorrectable
// record. The probe now refuses a pane af did not launch (one carrying no
// @af_pane_pid stamp), so the roster read leaves the row unstamped and the
// command's own stop or exit can still be recorded.
func TestRecordProcessTabExitsIgnoresAnUnstampedForeignPane(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))

	const agentName = "af_foreign_pane"
	procName := agentName + tmuxTabSeparator + "deploy"
	// The mock answers the deploy session's ACTIVE pane the way tmux does for
	// a user's split pane that exited with status 7: dead, reaped, and
	// carrying no @af_pane_pid stamp af would have written at launch.
	cmdExec := nameKeyedExecWithFinishedPanes(
		map[string]bool{agentName: true, procName: true},
		map[string]finishedPane{procName: {status: "7", at: "1726000000", unstamped: true}})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	gw, err := git.NewGitWorktreeFromStorage("/tmp/foreign-pane-repo", t.TempDir(), "foreign",
		"foreign-branch", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		Title:       "foreign",
		Path:        "/tmp/foreign-pane-repo",
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs: []*Tab{
			newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, cmdExec)),
			{ID: "deploy", Name: "deploy", Kind: TabKindProcess, Command: "./deploy.sh",
				tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(procName, "./deploy.sh", pty, cmdExec)},
		},
	}

	before := inst.TabsData()
	require.NotNil(t, before[1])
	require.Nil(t, before[1].Exit, "the roster reports no exit until something looks")

	inst.RecordProcessTabExits()

	after := inst.TabsData()
	require.NotNil(t, after[1])
	require.Nil(t, after[1].Exit,
		"an unstamped foreign pane's death is not the recorded command's exit; the row stays unstamped so the command's real exit can still be recorded")
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
