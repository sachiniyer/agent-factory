package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startedMockInstance builds a started local instance whose agent tab is a
// mock-backed tmux session (#930 PR 4). AddShellTab spawns siblings off that
// session, so they inherit the mock PTY factory / executor and stay hermetic —
// no real tmux server is touched. extraAlive marks additional session names as
// already existing (used by the restart-survival test).
func startedMockInstance(t *testing.T, agentName string, extraAlive ...string) *Instance {
	t.Helper()
	// Isolate config reads from the developer's real ~/.agent-factory (#837).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	alive := map[string]bool{agentName: true}
	for _, n := range extraAlive {
		alive[n] = true
	}
	exec := nameKeyedExec(alive)
	pty := persistPtyFactory{t: t, cmdExec: exec}

	repoPath := "/tmp/tab-lifecycle-" + agentName
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), agentName,
		agentName+"-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec)
	return &Instance{
		Title:       agentName,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs)},
	}
}

// TestFreshStart_OnlyAgentTab is the #1100 headline test, through the
// production path (NewInstance -> Start(true) -> setupTabs): creating a new
// instance must bring up ONLY the agent tab — no terminal (shell) tab is
// auto-created and no __shell tmux session is spawned. The terminal tab stays
// available on demand via AddShellTab (the 't' hotkey / `af sessions
// tab-create` path).
func TestFreshStart_OnlyAgentTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	workdir := t.TempDir()
	runGit(t, workdir, "init")
	runGit(t, workdir, "config", "--local", "user.email", "test@example.com")
	runGit(t, workdir, "config", "--local", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "f.txt"), []byte("x"), 0644))
	runGit(t, workdir, "add", ".")
	runGit(t, workdir, "commit", "-m", "init")

	const agentName = "af_1100_fresh"
	cmdExec := nameKeyedExec(map[string]bool{})
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}

	inst, err := NewInstance(InstanceOptions{Title: "fresh-1100", Path: workdir, Program: "bash"})
	require.NoError(t, err)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec))
	require.NoError(t, inst.Start(true))
	defer func() { _ = inst.Kill() }()

	tabs := inst.GetTabs()
	require.Len(t, tabs, 1, "a fresh instance must come up with only the agent tab (#1100)")
	assert.Equal(t, TabKindAgent, tabs[0].Kind)

	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		agentName+shellTmuxSuffix, defaultShell(), pty, cmdExec)
	assert.False(t, shellTs.ExistsOrUnknown(),
		"no __shell tmux session may be spawned on a fresh start (#1100)")

	// The terminal tab is still available on demand.
	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the on-demand shell tab must be live")
}

// TestAddShellTab_AppendsAndNamesUniquely verifies a human-created shell tab is
// appended, named uniquely per instance ("shell", then "shell-2"), and backed by
// a distinct live tmux session.
func TestAddShellTab_AppendsAndNamesUniquely(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_add")

	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, shellTabName, tab.Name, "first shell tab is named %q", shellTabName)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the new shell tab session must be live")

	tab2, err := inst.AddShellTab()
	require.NoError(t, err)
	assert.Equal(t, "shell-2", tab2.Name, "the second shell tab must get a unique name")
	assert.Equal(t, 3, inst.TabCount())
	assert.True(t, inst.TabAlive(2))
	assert.NotEqual(t, tab.tmux.SanitizedName(), tab2.tmux.SanitizedName(),
		"each shell tab must have a unique tmux session name")
}

// TestCloseTab_RemovesAndProtectsAgent verifies CloseTab removes a shell tab and
// kills its session, but refuses to close the agent tab (index 0) or any
// out-of-range index.
func TestCloseTab_RemovesAndProtectsAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_close")
	_, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount())

	require.Error(t, inst.CloseTab(0), "the agent tab must be unclosable")
	require.Equal(t, 2, inst.TabCount(), "a rejected close must not mutate the tab list")

	require.Error(t, inst.CloseTab(9), "an out-of-range index must be rejected")
	require.Equal(t, 2, inst.TabCount())

	require.NoError(t, inst.CloseTab(1))
	require.Equal(t, 1, inst.TabCount(), "closing a shell tab removes it")
	require.False(t, inst.TabAlive(1), "the closed tab's session must be gone")
}

// TestAddShellTab_SoftCapAtNine verifies new-tab is refused once the instance
// already holds maxTabs (9) tabs — the number-key range can't address more.
func TestAddShellTab_SoftCapAtNine(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_cap")
	// Agent tab + 8 shell tabs = 9 == maxTabs.
	for i := 0; i < maxTabs-1; i++ {
		_, err := inst.AddShellTab()
		require.NoError(t, err)
	}
	require.Equal(t, maxTabs, inst.TabCount())

	_, err := inst.AddShellTab()
	require.Error(t, err, "the 10th tab must be refused")
	require.Contains(t, err.Error(), fmt.Sprintf("%d", maxTabs))
	require.Equal(t, maxTabs, inst.TabCount(), "the cap must not create a tab")
}

// TestAddShellTab_RejectedForUnstarted verifies AddShellTab errors when the
// instance has no live agent session/worktree (e.g. not started).
func TestAddShellTab_RejectedForUnstarted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := NewInstance(InstanceOptions{Title: "unstarted", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	_, err = inst.AddShellTab()
	require.Error(t, err)
	require.Equal(t, 0, inst.TabCount())
}

// TestAttachShellTab_ReconnectsExistingSessionNoSpawn verifies the no-spawn
// reconnect path (#960 PR 2): when the daemon has already created the shell
// session, AttachShellTab binds to that exact session and appends the tab
// without issuing a second new-session that would collide.
func TestAttachShellTab_ReconnectsExistingSessionNoSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_tabs_attach"
	// The daemon already spawned the sibling shell session.
	inst := startedMockInstance(t, agentName, agentName+"__shell")

	tab, err := inst.AttachShellTab("shell")
	require.NoError(t, err)
	assert.Equal(t, "shell", tab.Name)
	assert.Equal(t, TabKindShell, tab.Kind)
	assert.Equal(t, 2, inst.TabCount(), "the reconnected tab must be appended")
	assert.True(t, inst.TabAlive(1), "the reconnected tab must be bound to the live session")
	assert.Equal(t, agentName+"__shell", tab.tmux.SanitizedName(),
		"the tab must bind to the exact daemon-derived session name")

	// A second call for the same name is a no-op returning the existing tab —
	// guards against a refresh racing ahead of the reconnect.
	again, err := inst.AttachShellTab("shell")
	require.NoError(t, err)
	assert.Same(t, tab, again, "a duplicate attach must return the existing tab")
	assert.Equal(t, 2, inst.TabCount(), "a duplicate attach must not append again")
}

// TestAttachShellTab_RejectedForUnstarted verifies AttachShellTab errors when the
// instance has no live agent session/worktree.
func TestAttachShellTab_RejectedForUnstarted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := NewInstance(InstanceOptions{Title: "unstarted", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	_, err = inst.AttachShellTab("shell")
	require.Error(t, err)
	require.Equal(t, 0, inst.TabCount())
}

// TestDropClosedTab_RemovesWithoutKilling verifies the no-kill removal path
// (#960 PR 2): DropClosedTab removes a tab from the list but does NOT tear down
// its tmux session (the daemon already did), and still protects the agent tab.
func TestDropClosedTab_RemovesWithoutKilling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_tabs_drop")
	tab, err := inst.AddShellTab()
	require.NoError(t, err)
	require.Equal(t, 2, inst.TabCount())
	require.True(t, tab.tmux.ExistsOrUnknown())

	require.Error(t, inst.DropClosedTab(0), "the agent tab must be undroppable")
	require.Equal(t, 2, inst.TabCount(), "a rejected drop must not mutate the list")
	require.Error(t, inst.DropClosedTab(9), "an out-of-range index must be rejected")

	require.NoError(t, inst.DropClosedTab(1))
	require.Equal(t, 1, inst.TabCount(), "drop must remove the tab from the list")
	require.True(t, tab.tmux.ExistsOrUnknown(),
		"DropClosedTab must NOT kill the tmux session (the daemon owns the kill)")
}

// TestUniqueShellName covers the per-instance naming sequence in isolation.
func TestUniqueShellName(t *testing.T) {
	assert.Equal(t, "shell", uniqueShellName(nil))
	assert.Equal(t, "shell-2", uniqueShellName([]*Tab{{Name: "shell"}}))
	assert.Equal(t, "shell-3", uniqueShellName([]*Tab{{Name: "shell"}, {Name: "shell-2"}}))
	// A hole in the sequence is filled with the lowest free name.
	assert.Equal(t, "shell-2", uniqueShellName([]*Tab{{Name: "shell"}, {Name: "shell-3"}}))
}

// TestRestartSurvival_HumanCreatedShellTab is the PR 4 restart-survival test: a
// shell tab created by the new-tab hotkey must persist through Storage and
// reconnect to its exact tmux session across an af/daemon restart, exactly like
// the default agent+shell pair (PR 2). Builds a started instance, adds an extra
// shell tab, round-trips it through Storage, and asserts all three tabs reload
// and reconnect.
func TestRestartSurvival_HumanCreatedShellTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_human_tab"
	shellName := agentName + "__shell"
	shell2Name := agentName + "__shell-2"

	inst := startedMockInstance(t, agentName)
	_, err := inst.AddShellTab() // shell
	require.NoError(t, err)
	third, err := inst.AddShellTab() // shell-2 — the "human-created" tab under test
	require.NoError(t, err)
	require.Equal(t, "shell-2", third.Name)
	require.Equal(t, 3, inst.TabCount())

	// Persist through Storage and reload with a fresh Storage, restoring sessions
	// for the exact persisted names so reconnection stays hermetic.
	repoID := config.RepoIDFromRoot(inst.Path)
	ms := newMockStorage()
	saveStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	require.NoError(t, saveStore.SaveInstances([]*Instance{inst}))

	restoreExec := nameKeyedExec(map[string]bool{agentName: true, shellName: true, shell2Name: true})
	restorePty := persistPtyFactory{t: t, cmdExec: restoreExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, restorePty, restoreExec)
	}
	defer func() { restoreTmuxSession = prev }()

	loadStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	loaded, err := loadStore.LoadInstances()
	require.NoError(t, err)
	require.Len(t, loaded, 1)

	restored := loaded[0]
	tabs := restored.GetTabs()
	require.Len(t, tabs, 3, "all three tabs (agent + two shells) must survive a restart")
	assert.Equal(t, TabKindShell, tabs[2].Kind)
	assert.Equal(t, "shell-2", tabs[2].Name, "the human-created tab keeps its name")
	assert.Equal(t, shell2Name, tabs[2].tmux.SanitizedName(),
		"the human-created tab must reconnect to its exact persisted tmux session")
	assert.True(t, restored.TabAlive(2), "the restored human-created tab must be live")
}
