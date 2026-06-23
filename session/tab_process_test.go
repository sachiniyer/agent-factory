package session

import (
	"fmt"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddProcessTab_AppendsWithCommandAndName verifies a CLI-spawned process tab
// is appended with the right kind, command, and explicit name, backed by a live
// tmux session running that command (#930 PR 5).
func TestAddProcessTab_AppendsWithCommandAndName(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_proc_add")

	tab, err := inst.AddProcessTab("btop", "monitor")
	require.NoError(t, err)
	assert.Equal(t, "monitor", tab.Name, "explicit --name must be honored")
	assert.Equal(t, TabKindProcess, tab.Kind)
	assert.Equal(t, "btop", tab.Command, "the tab must record its command for persistence")
	assert.Equal(t, 2, inst.TabCount())
	assert.True(t, inst.TabAlive(1), "the new process tab session must be live")
}

// TestAddProcessTab_DefaultNameFromCommandBasename covers default-name
// derivation: with no --name, the tab is named after the command's basename.
func TestAddProcessTab_DefaultNameFromCommandBasename(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_proc_default")

	tab, err := inst.AddProcessTab("/usr/bin/htop -t", "")
	require.NoError(t, err)
	assert.Equal(t, "htop", tab.Name, "default name is the basename of the command's first word")
	assert.Equal(t, "/usr/bin/htop -t", tab.Command)
}

// TestAddProcessTab_CollisionSuffixing verifies a derived name that collides with
// an existing tab is auto-suffixed (-2, -3, …), mirroring shell-tab collision
// handling.
func TestAddProcessTab_CollisionSuffixing(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_proc_collide")

	first, err := inst.AddProcessTab("btop", "")
	require.NoError(t, err)
	assert.Equal(t, "btop", first.Name)

	second, err := inst.AddProcessTab("btop --tree", "")
	require.NoError(t, err)
	assert.Equal(t, "btop-2", second.Name, "a colliding derived name must be suffixed")

	third, err := inst.AddProcessTab("btop", "btop") // explicit name also collides
	require.NoError(t, err)
	assert.Equal(t, "btop-3", third.Name)

	assert.NotEqual(t, first.tmux.SanitizedName(), second.tmux.SanitizedName(),
		"each process tab must have a unique tmux session name")
}

// TestAddProcessTab_EmptyCommandRejected verifies an empty/whitespace command is
// refused before any tab is created.
func TestAddProcessTab_EmptyCommandRejected(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_proc_empty")

	_, err := inst.AddProcessTab("   ", "name")
	require.Error(t, err)
	assert.Equal(t, 1, inst.TabCount(), "a rejected empty command must not create a tab")
}

// TestAddProcessTab_SoftCapAtNine verifies process-tab creation is refused once
// the instance already holds maxTabs (9) tabs.
func TestAddProcessTab_SoftCapAtNine(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_proc_cap")
	for i := 0; i < maxTabs-1; i++ {
		_, err := inst.AddProcessTab(fmt.Sprintf("cmd%d", i), "")
		require.NoError(t, err)
	}
	require.Equal(t, maxTabs, inst.TabCount())

	_, err := inst.AddProcessTab("overflow", "")
	require.Error(t, err, "the 10th tab must be refused")
	require.Contains(t, err.Error(), fmt.Sprintf("%d", maxTabs))
	require.Equal(t, maxTabs, inst.TabCount(), "the cap must not create a tab")
}

// TestAddProcessTab_RejectedForUnstarted verifies AddProcessTab errors when the
// instance has no live agent session/worktree.
func TestAddProcessTab_RejectedForUnstarted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := NewInstance(InstanceOptions{Title: "unstarted-proc", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	_, err = inst.AddProcessTab("btop", "")
	require.Error(t, err)
	require.Equal(t, 0, inst.TabCount())
}

// TestSanitizeTabName covers the name-token sanitization that keeps a tab's
// derived tmux session name valid and round-trippable.
func TestSanitizeTabName(t *testing.T) {
	assert.Equal(t, "btop", sanitizeTabName("btop"))
	assert.Equal(t, "explorer-py", sanitizeTabName("explorer.py"))
	assert.Equal(t, "my-data-explorer", sanitizeTabName("my data explorer"))
	assert.Equal(t, "tab", sanitizeTabName("  tab  "))
	assert.Equal(t, "", sanitizeTabName("///"), "a name with no usable chars sanitizes to empty")
}

// TestProcessTabBaseName covers base-name selection: explicit name wins, else the
// command basename, else the "process" fallback.
func TestProcessTabBaseName(t *testing.T) {
	assert.Equal(t, "monitor", processTabBaseName("monitor", "btop"))
	assert.Equal(t, "btop", processTabBaseName("", "/usr/bin/btop -t"))
	assert.Equal(t, "explorer", processTabBaseName("  ", "explorer --json"))
	assert.Equal(t, "process", processTabBaseName("", "///"), "fall back to process when nothing usable remains")
}

// TestRestartSurvival_ProcessTab is the PR 5 restart-survival test: a CLI-created
// process tab must persist through Storage and reconnect to its exact tmux
// session across an af/daemon restart, with its Command intact — the load-bearing
// #930 requirement applied to process tabs.
func TestRestartSurvival_ProcessTab(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const agentName = "af_proc_restart"
	shellName := agentName + "__shell"
	procName := agentName + "__btop"

	// Mirror production layout: an instance always has agent+shell, and a process
	// tab is added on top.
	inst := startedMockInstance(t, agentName)
	_, err := inst.AddShellTab()
	require.NoError(t, err)
	tab, err := inst.AddProcessTab("btop -t", "") // derives name "btop"
	require.NoError(t, err)
	require.Equal(t, "btop", tab.Name)
	require.Equal(t, 3, inst.TabCount())

	repoID := config.RepoIDFromRoot(inst.Path)
	ms := newMockStorage()
	saveStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	require.NoError(t, saveStore.SaveInstances([]*Instance{inst}))

	restoreExec := nameKeyedExec(map[string]bool{agentName: true, shellName: true, procName: true})
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
	require.Len(t, tabs, 3, "agent, shell, and process tabs must all survive a restart")
	assert.Equal(t, TabKindProcess, tabs[2].Kind)
	assert.Equal(t, "btop", tabs[2].Name, "the process tab keeps its name")
	assert.Equal(t, "btop -t", tabs[2].Command, "the process tab must restore its command intact")
	assert.Equal(t, procName, tabs[2].tmux.SanitizedName(),
		"the process tab must reconnect to its exact persisted tmux session")
	assert.True(t, restored.TabAlive(2), "the restored process tab must be live")
}
