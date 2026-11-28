package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7944: a shell tab whose $SHELL exited used to read as live on every surface
// — its tmux session vanished with it, so nothing could observe the death. The
// fix gives shell tabs the remain-on-exit contract process tabs already carry
// (#4479): the death is held as a dead pane, the exit probe stamps it, and the
// roster reads dead everywhere a process tab already did. These pin that the
// shell kind rides the same evidence path — they fail where the probes or the
// spawn still treat the shell kind as unobservable.

// shellTabFixture builds a started local instance whose agent tab and —
// unless withoutShell — one shell tab share a mock-backed executor and a
// real-file PTY factory: sessions named in alive report existing, sessions
// named in finished hold a held dead pane for as long as they exist, and every
// executor call lands in commands.
func shellTabFixture(t *testing.T, alive map[string]bool, finished map[string]finishedPane, withoutShell ...bool) (*Instance, *[]string) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	var commands []string
	cmdExec := nameKeyedExecWithFinishedPanes(alive, finished)
	baseRun := cmdExec.RunFunc
	cmdExec.RunFunc = func(c *exec.Cmd) error {
		commands = append(commands, c.String())
		return baseRun(c)
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	repoPath := "/tmp/dead-shell-repo"
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, existingDir(t, filepath.Join(t.TempDir(), "wt")), "dead-shell",
		"dead-shell-branch", "", false, true)
	require.NoError(t, err)

	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	tabs := []*Tab{
		newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec)),
	}
	if len(withoutShell) == 0 || !withoutShell[0] {
		tabs = append(tabs, &Tab{ID: "shell-id", Name: "terminal", Kind: TabKindShell,
			tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(shellName, defaultShell(), pty, cmdExec)})
	}
	inst := &Instance{
		Title:       "dead-shell",
		Path:        repoPath,
		Program:     "bash",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        tabs,
	}
	return inst, &commands
}

// The roster read is where a live exit gets recorded — for a shell tab exactly
// as for a process tab: the probe answers the held dead pane's status and
// time, and the row carries them into every surface that renders the roster.
func TestRecordTabExitsStampsADeadShellTab(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	inst, _ := shellTabFixture(t,
		map[string]bool{agentName: true, shellName: true},
		map[string]finishedPane{shellName: {status: "0", at: "1726000000", start: afPaneStart(defaultShell())}})

	before := inst.TabsData()
	require.Len(t, before, 2)
	require.Nil(t, before[1].Exit, "the roster reports no exit until something looks")

	inst.RecordTabExits()

	after := inst.TabsData()
	exit := after[1].Exit
	require.NotNil(t, exit, "the refreshed roster row carries the shell's recorded exit")
	assert.Equal(t, 0, exit.Status)
	assert.True(t, exit.StatusKnown)
	assert.Equal(t, time.Unix(1726000000, 0), exit.At)
}

// The restore path stamps the same evidence when it reattaches to a held dead
// shell pane — and must NOT respawn it: a pane tmux still holds is not the
// session-gone case the #991 dead-shell replacement exists for.
func TestSetupTabsStampsARestoredDeadShellTab(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	inst, commands := shellTabFixture(t,
		map[string]bool{agentName: true, shellName: true},
		map[string]finishedPane{shellName: {status: "0", at: "1726000000", start: afPaneStart(defaultShell())}})

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))

	exit := inst.Tabs[1].Exit
	require.NotNil(t, exit, "a held dead shell pane's exit is stamped on restore")
	assert.True(t, exit.StatusKnown)
	spawns := 0
	for _, c := range *commands {
		if strings.Contains(c, "new-session") && strings.Contains(c, shellName) {
			spawns++
		}
	}
	assert.Zero(t, spawns, "a shell whose pane tmux still holds is kept dead, never respawned")
}

// A shell whose session is gone ENTIRELY (no held pane — tmux-server loss, a
// killed session, or a pre-#7944 tab) still takes the #991 fresh-shell
// replacement on restore: absence is the signal the respawn exists for, and
// remain-on-exit never reaches it.
func TestSetupTabsRespawnsAShellWhoseSessionIsGone(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	inst, commands := shellTabFixture(t,
		map[string]bool{agentName: true},
		nil)
	// The shell died while the roster could see it, the tmux server was lost
	// after, and the restore now launches a replacement: the old exit described
	// the OLD incarnation and must be cleared, or the live shell reads dead
	// forever (and the interactive guard refuses a healthy PTY).
	inst.Tabs[1].Exit = &TabExit{Status: 0, StatusKnown: true, At: time.Unix(1726000000, 0)}

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Nil(t, inst.Tabs[1].Exit, "a respawned shell drops the exit that described its dead predecessor")

	spawns := 0
	armed := false
	for _, c := range *commands {
		if strings.Contains(c, "new-session") && strings.Contains(c, shellName) {
			spawns++
			armed = strings.Contains(c, "remain-on-exit on")
		}
	}
	assert.Equal(t, 1, spawns, "a shell whose session is definitively gone is replaced")
	assert.True(t, armed, "the replacement shell rides the same remain-on-exit spawn the create path uses")
}

// The create path arms remain-on-exit inside the new-session invocation — the
// whole mechanism this fix rests on. Without it the pane exits with the
// session and nothing below can ever observe the death.
func TestAddShellTabArmsRemainOnExit(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + shellTabName
	inst, commands := shellTabFixture(t, map[string]bool{agentName: true}, nil, true)

	_, err := inst.AddShellTabWithID("shell-1")
	require.NoError(t, err)

	armed := false
	for _, c := range *commands {
		if strings.Contains(c, "new-session") && strings.Contains(c, shellName) && strings.Contains(c, "remain-on-exit on") {
			armed = true
		}
	}
	assert.True(t, armed,
		"the shell's new-session invocation must carry remain-on-exit, like a process tab's (#7944)")
}

// TabDead is the interactive-entry probe (#7944): provably dead — a stamped
// exit, a held dead pane, or a definitively gone session — refuses; everything
// else answers alive-or-unknown and is left to the bind.
func TestTabDead(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"

	t.Run("held dead pane", func(t *testing.T) {
		inst, _ := shellTabFixture(t,
			map[string]bool{agentName: true, shellName: true},
			map[string]finishedPane{shellName: {status: "0", at: "1726000000", start: afPaneStart(defaultShell())}})
		assert.True(t, inst.TabDead(1))
	})
	t.Run("stamped exit", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true}, nil)
		inst.Tabs[1].Exit = &TabExit{StatusKnown: true}
		assert.True(t, inst.TabDead(1))
	})
	t.Run("gone session", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true}, nil)
		assert.True(t, inst.TabDead(1))
	})
	t.Run("live shell", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true}, nil)
		assert.False(t, inst.TabDead(1))
	})
	t.Run("agent and out-of-range", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true}, nil)
		assert.False(t, inst.TabDead(0))
		assert.False(t, inst.TabDead(-1))
		assert.False(t, inst.TabDead(9))
	})
}
