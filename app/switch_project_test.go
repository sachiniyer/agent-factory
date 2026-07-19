package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// TestSelectProjectRowSwitchesProject: with the bottom Projects section focused
// and the cursor on a project row, Enter reuses the #1461 switchProject path to
// re-scope the rail to that project. This is the Tab-focusable Projects surface
// (#1588 follow-up), additive to the ctrl+p picker.
func TestSelectProjectRowSwitchesProject(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	resizeHome(h, 100, 30)

	// A real git repo the row resolves to (switchToProjectRoot runs RepoFromPath).
	repoBRoot := initTestGitRepo(t)
	require.NotEqual(t, h.repoRoot, repoBRoot)

	// Push a Projects list holding repo B (first) and the active project, focus
	// the Projects section, and rest the cursor on repo B's row (the first).
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: filepath.Base(repoBRoot), Root: repoBRoot, SessionCount: 0},
		{Name: filepath.Base(h.repoRoot), Root: h.repoRoot, SessionCount: 1, Active: true},
	})
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())

	proj, ok := h.projects.SelectedProject()
	require.True(t, ok, "cursor must rest on a project row")
	require.Equal(t, repoBRoot, proj.Root)

	// Enter is routed through the focused Projects section.
	mod, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, consumed, "Enter must be consumed by the focused Projects section")
	require.NotNil(t, mod)

	assert.Equal(t, config.RepoIDFromRoot(repoBRoot), h.repoID, "Enter on a project row must switch to it")
	assert.Equal(t, repoBRoot, h.repoRoot)
}

// TestProjectsSectionEscReturnsToTree: Esc on the focused Projects section moves
// the ring back to the tree (mirrors the Automations Esc flow), without touching
// the active project.
func TestProjectsSectionEscReturnsToTree(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return nil, nil
	}))
	resizeHome(h, 100, 30)
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: filepath.Base(h.repoRoot), Root: h.repoRoot, SessionCount: 0, Active: true},
	})
	h.focusRegion(layout.RegionProjects)
	require.True(t, h.projects.Focused())

	before := h.repoID
	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyEsc})
	require.True(t, consumed)
	assert.Equal(t, layout.RegionTree, h.ring.Active(), "Esc returns focus to the tree")
	assert.False(t, h.projects.Focused())
	assert.Equal(t, before, h.repoID, "Esc must not switch projects")
}

// TestSwitchProjectRescopesSidebar is the core #1461 guarantee: switching to
// another project fully swaps the sidebar to that project's sessions — the
// previous project's sessions are hidden (no cross-repo bleed) — and re-scopes
// repoID/repoRoot so new sessions target the newly active project.
func TestSwitchProjectRescopesSidebar(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// Seed the current (repo A) sidebar with a session.
	h.store.AddInstance(newSnapshotTestInstance(t, "repoA-session"))
	require.NotNil(t, findSidebarInstance(h, "repoA-session"))

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}

	// The per-repo fetcher returns repo B's sessions only for repo B's id. Any
	// bleed of repo A into the post-switch sidebar would show up as a stale row.
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		if repoID == repoB.ID {
			return daemon.SnapshotResponse{Instances: []session.InstanceData{
				{Title: "repoB-session", CreatedAt: time.Now()},
			}}, nil
		}
		return daemon.SnapshotResponse{}, nil
	}

	mod, _ := h.switchProject(repoB)
	require.NotNil(t, mod)

	// repoID/repoRoot re-scoped to the target project.
	assert.Equal(t, repoB.ID, h.repoID)
	assert.Equal(t, repoBRoot, h.repoRoot)

	// The sidebar now shows ONLY repo B's session.
	require.NotNil(t, findSidebarInstance(h, "repoB-session"), "the target project's session must appear")
	assert.Nil(t, findSidebarInstance(h, "repoA-session"), "the previous project's sessions must be hidden after switch")

	// The active-project header reflects the new project.
	h.sidebar.SetSize(40, 20)
	assert.Contains(t, h.sidebar.String(), filepath.Base(repoBRoot), "sidebar title should name the active project")

	// A new session created after the switch targets the new project's root.
	h.startNewInstance(false)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, repoBRoot, h.namingInstance.Path, "new sessions must target the switched-to project root")
}

// TestSwitchProjectDropsStaleSnapshot: a background snapshot fetched for the
// previous project (in flight across the switch) must be dropped, not
// reconciled into the new project's view (#1461 cross-repo bleed).
func TestSwitchProjectDropsStaleSnapshot(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	oldRepoID := h.repoID

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		if repoID == repoB.ID {
			return daemon.SnapshotResponse{Instances: []session.InstanceData{
				{Title: "repoB-session", CreatedAt: time.Now()},
			}}, nil
		}
		return daemon.SnapshotResponse{}, nil
	}
	h.switchProject(repoB)
	require.NotNil(t, findSidebarInstance(h, "repoB-session"))

	// A stale snapshot for the OLD repo lands after the switch. The repoID guard
	// must drop it so the old project's session does not reappear.
	h.Update(snapshotFetchedMsg{
		repoID: oldRepoID,
		data:   []session.InstanceData{{Title: "repoA-ghost", CreatedAt: time.Now()}},
	})
	assert.Nil(t, findSidebarInstance(h, "repoA-ghost"), "a snapshot for the previous project must be dropped")
	require.NotNil(t, findSidebarInstance(h, "repoB-session"), "the active project's session must remain")
}

// TestBackgroundRefreshFollowsActiveProject: after a switch the background
// snapshot poll reads the ACTIVE project's tasks (m.repoRoot), not the launch
// repo's, and tags the response with the active repoID (#1461).
func TestBackgroundRefreshFollowsActiveProject(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	require.NoError(t, task.AddTask(task.Task{
		ID: "b1", Name: "B", Prompt: "p", CronExpr: "0 * * * *",
		ProjectPath: repoBRoot, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}))
	require.NoError(t, task.AddTask(task.Task{
		ID: "other", Name: "Other", Prompt: "p", CronExpr: "0 * * * *",
		ProjectPath: "/some/other/repo", Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}))

	h.switchProject(repoB)

	msg, ok := h.fetchSnapshotCmd()().(snapshotFetchedMsg)
	require.True(t, ok)
	assert.Equal(t, repoB.ID, msg.repoID, "the poll response must be tagged with the active repoID")
	require.NoError(t, msg.tasksErr)
	require.Len(t, msg.tasks, 1, "the poll must read only the active project's tasks")
	assert.Equal(t, "b1", msg.tasks[0].ID)
}

// TestSwitchProjectWithArchivedSection: switching away from a project that has
// an archived session (and an expanded/navigated archived folder, #1516/#1518/
// #1527) must fully swap to the target project — the previous project's archived
// row is gone and the sidebar renders coherently, without the archived-section
// state interfering with the scope swap (#1461).
func TestSwitchProjectWithArchivedSection(t *testing.T) {
	h := newTestHome(t)
	h.sidebar.SetSize(40, 20)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// Repo A: a live session plus an archived one, with the archived folder
	// expanded and the cursor driven into it.
	h.store.AddInstance(newSnapshotTestInstance(t, "repoA-live"))
	archivedA := archiveActionInstance(t, "repoA-archived", session.Archived)
	h.store.AddInstance(archivedA)
	h.sidebar.ExpandSection()
	h.sidebar.SelectInstance(archivedA) // drive the cursor into the archived folder
	_ = h.sidebar.String()              // exercises the archived render + auto-collapse path
	require.NotNil(t, findSidebarInstance(h, "repoA-archived"))

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		if repoID == repoB.ID {
			return daemon.SnapshotResponse{Instances: []session.InstanceData{
				{Title: "repoB-live", CreatedAt: time.Now()},
			}}, nil
		}
		return daemon.SnapshotResponse{}, nil
	}

	h.switchProject(repoB)

	assert.Nil(t, findSidebarInstance(h, "repoA-live"), "previous project's live row must be gone")
	assert.Nil(t, findSidebarInstance(h, "repoA-archived"), "previous project's archived row must be gone")
	require.NotNil(t, findSidebarInstance(h, "repoB-live"), "target project's session must appear")

	// The sidebar renders the new scope without stale archived rows or a panic.
	out := h.sidebar.String()
	assert.Contains(t, out, filepath.Base(repoBRoot))
	assert.NotContains(t, out, "repoA-archived")
}

// TestSwitchProjectSameRepoIsNoop: switching to the already-active project is a
// no-op that leaves the sidebar untouched.
func TestSwitchProjectSameRepoIsNoop(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(newSnapshotTestInstance(t, "existing"))

	same := &config.RepoContext{Root: h.repoRoot, ID: h.repoID}
	h.switchProject(same)

	require.NotNil(t, findSidebarInstance(h, "existing"), "a same-repo switch must not wipe the sidebar")
}

// TestSwitchProjectFailedSnapshotLeavesProjectIntact is the #1788 guarantee: a
// switch whose snapshot fetch fails (a wedged or unreachable daemon — most
// likely against a remote target) must be a no-op, not a half-applied switch.
// Before the fix the model was re-scoped to the incoming repo and the projection
// reset BEFORE the fetch ran, so a failure stranded the TUI on the new
// repoID/repoRoot with an empty sidebar: background polls and any new session
// then targeted a project the user could not see.
func TestSwitchProjectFailedSnapshotLeavesProjectIntact(t *testing.T) {
	h := newTestHome(t)
	h.sidebar.SetSize(40, 20)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// Give repo A a concrete root and an open session, as a live TUI would have.
	repoARoot := t.TempDir()
	h.repoRoot = repoARoot
	h.sidebar.SetProjectName(filepath.Base(repoARoot))
	repoAID := h.repoID
	h.store.AddInstance(newSnapshotTestInstance(t, "repoA-session"))
	require.NotNil(t, findSidebarInstance(h, "repoA-session"))

	repoBRoot := t.TempDir()
	repoB := &config.RepoContext{Root: repoBRoot, ID: config.RepoIDFromRoot(repoBRoot)}

	// Repo B's snapshot is unavailable; repo A's would still succeed.
	h.snapshotFetcher = func(repoID string) (daemon.SnapshotResponse, error) {
		if repoID == repoB.ID {
			return daemon.SnapshotResponse{}, errors.New("daemon unavailable")
		}
		return daemon.SnapshotResponse{}, nil
	}

	h.switchProject(repoB)

	// The repo context never moved, so polls and new sessions stay on repo A.
	assert.Equal(t, repoAID, h.repoID, "a failed switch must not re-scope repoID")
	assert.Equal(t, repoARoot, h.repoRoot, "a failed switch must not re-scope repoRoot")

	// Repo A's sidebar survived rather than being cleared for a switch that
	// never landed.
	require.NotNil(t, findSidebarInstance(h, "repoA-session"), "the current project's sessions must survive a failed switch")
	assert.Contains(t, h.sidebar.String(), filepath.Base(repoARoot), "the sidebar must still name the current project")

	// The user-visible payoff: a session created after the failed switch still
	// targets the project the sidebar is showing.
	h.startNewInstance(false)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, repoARoot, h.namingInstance.Path, "new sessions must still target the current project after a failed switch")
}

// TestSwitchProjectClearsHooksWhenConfigResolveFails is the #1686 guarantee:
// when config.ResolveConfig fails for the incoming project (e.g. the global
// config is unreadable/corrupt), the switch must clear the hooks pane rather
// than leave the OUTGOING project's hooks in it. m.repoRoot already points at
// the new project by this point, so a save from a stale pane would write the
// previous project's hooks into the new project's in-repo config.
func TestSwitchProjectClearsHooksWhenConfigResolveFails(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	// The outgoing project (A) has hooks loaded into the pane.
	projectAHooks := []string{"echo 'project-a-hook-1'", "echo 'project-a-hook-2'"}
	h.hooksPane.SetCommands(projectAHooks)
	h.store.SetHookCount(len(projectAHooks))
	require.Equal(t, projectAHooks, h.hooksPane.GetCommands())

	// The incoming project (B) is a real, distinct git repo.
	projectBRoot := initTestGitRepo(t)
	repoB := &config.RepoContext{Root: projectBRoot, ID: config.RepoIDFromRoot(projectBRoot)}

	// Corrupt the global config so ResolveConfig fails on the switch. newTestHome
	// redirects AGENT_FACTORY_HOME to a tempdir, so this only affects the test.
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	globalConfigPath := filepath.Join(configDir, "config.toml")
	require.NoError(t, os.WriteFile(globalConfigPath, []byte("invalid toml {{{\n[broken"), 0o644))
	_, err = config.ResolveConfig(projectBRoot)
	require.Error(t, err, "precondition: ResolveConfig must fail with the corrupt global config")

	h.switchProject(repoB)

	assert.Equal(t, projectBRoot, h.repoRoot, "repoRoot must be re-scoped to the new project")
	assert.Empty(t, h.hooksPane.GetCommands(), "stale hooks from the previous project must be cleared, not carried over")
}

// writeInRepoConfig writes <root>/.agent-factory/config.toml with body.
func writeInRepoConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644))
}

// TestSwitchProjectResetsProgramWhenConfigResolveFails is the #2138 guarantee,
// the m.program half of the #1686 hooks reset above: when ResolveConfig fails
// for the incoming project (here, a corrupt IN-REPO config), the switch must
// reset m.program to the global default rather than leave the OUTGOING
// project's program in place. Session creation is protected by
// preflightSessionCreate, which re-resolves and blocks; task creation is not —
// it falls back to m.program and PERSISTS it into tasks.json, so a stale value
// silently runs the new project's tasks under the previous project's agent.
func TestSwitchProjectResetsProgramWhenConfigResolveFails(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}

	globalDefault := h.appConfig.DefaultProgram
	require.NotEmpty(t, globalDefault, "precondition: the global config carries a default program")

	// The outgoing project (A) set a non-global program, and its hooks are in
	// the pane — both are project-scoped state that must not survive the switch.
	h.program = "codex"
	require.NotEqual(t, globalDefault, h.program, "precondition: the outgoing program differs from the global default")
	h.hooksPane.SetCommands([]string{"echo 'project-a-hook'"})
	h.store.SetHookCount(1)

	// The incoming project (B) is a real git repo whose in-repo config cannot be
	// parsed. Only B's config is corrupt: the global config stays valid, which is
	// what makes "reset to the global default" a well-defined target.
	projectBRoot := initTestGitRepo(t)
	writeInRepoConfig(t, projectBRoot, "invalid toml {{{\n[broken")
	repoB := &config.RepoContext{Root: projectBRoot, ID: config.RepoIDFromRoot(projectBRoot)}

	_, err := config.ResolveConfig(projectBRoot)
	require.Error(t, err, "precondition: ResolveConfig must fail with the corrupt in-repo config")
	global, err := config.LoadConfig()
	require.NoError(t, err, "precondition: the global config must still load")
	require.Equal(t, globalDefault, global.DefaultProgram)

	h.switchProject(repoB)

	assert.Equal(t, projectBRoot, h.repoRoot, "repoRoot must be re-scoped to the new project")
	assert.Equal(t, globalDefault, h.program,
		"the outgoing project's program must be reset to the global default, not carried over (#2138)")
	assert.Empty(t, h.hooksPane.GetCommands(), "hooks must still be cleared on a failed resolve (#1686)")
	assert.Equal(t, 0, h.store.GetHookCount(), "the hook count must still be cleared on a failed resolve (#1686)")
}

// TestSwitchProjectAppliesProjectProgramWhenConfigResolves guards the success
// branch the #2138 reset sits beside: a project whose in-repo config sets
// default_program still wins over both the outgoing program and the global
// default.
func TestSwitchProjectAppliesProjectProgramWhenConfigResolves(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	h.program = "codex"

	projectBRoot := initTestGitRepo(t)
	writeInRepoConfig(t, projectBRoot, "default_program = \"aider\"\n")
	repoB := &config.RepoContext{Root: projectBRoot, ID: config.RepoIDFromRoot(projectBRoot)}

	h.switchProject(repoB)

	assert.Equal(t, "aider", h.program, "the incoming project's default_program must win on a successful resolve")
}

// TestTaskCreateAfterFailedResolveUsesGlobalProgram is the user-visible half of
// #2138: the leaked program was not just a field, it was PERSISTED. Creating a
// task in a project whose config failed to resolve must record the global
// default program, not the previous project's.
func TestTaskCreateAfterFailedResolveUsesGlobalProgram(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	globalDefault := h.appConfig.DefaultProgram
	h.program = "codex"

	projectBRoot := initTestGitRepo(t)
	writeInRepoConfig(t, projectBRoot, "invalid toml {{{\n[broken")
	repoB := &config.RepoContext{Root: projectBRoot, ID: config.RepoIDFromRoot(projectBRoot)}
	_, err := config.ResolveConfig(projectBRoot)
	require.Error(t, err, "precondition: ResolveConfig must fail with the corrupt in-repo config")

	h.switchProject(repoB)

	// Drive the inline create form the way a user would: the program field is
	// left on its default option, which is exactly the case that falls back to
	// m.program in handleTaskCreate.
	tp := h.automations.TaskPane()
	_, _ = h.showTasksOverlay()
	require.Equal(t, stateTasks, h.state)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	require.True(t, tp.IsCreating(), "'n' must open the inline create form")
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nightly")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> trigger selector (cron)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> schedule picker (valid default)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> prompt
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do the thing")})
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> target session
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> path
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> program (default option)
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyTab}) // -> save button
	_, _ = h.handleStateTasks(tea.KeyMsg{Type: tea.KeyEnter})

	saved, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, saved, 1, "the create must have reached disk")
	assert.Equal(t, globalDefault, saved[0].Program,
		"a task created after a failed resolve must carry the global default program, not the previous project's (#2138)")
}

// TestBuildProjectListUnionsSourcesWithCounts: the picker list unions the
// cross-repo session snapshot (with counts), the root_agents opt-ins, and the
// active project, deduped by repo root.
func TestBuildProjectListUnionsSourcesWithCounts(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/repos/active"

	repoWithSessions := "/repos/busy"
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		mk := func(root string) session.InstanceData {
			d := session.InstanceData{Title: "s", CreatedAt: time.Now()}
			d.Worktree.RepoPath = root
			return d
		}
		return []session.InstanceData{mk(repoWithSessions), mk(repoWithSessions)}, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	got := h.buildProjectList()
	byRoot := map[string]overlay.Project{}
	for _, p := range got {
		byRoot[p.Root] = p
	}

	require.Contains(t, byRoot, repoWithSessions)
	assert.Equal(t, 2, byRoot[repoWithSessions].SessionCount, "session count should be grouped by repo root")
	require.Contains(t, byRoot, "/repos/active", "the active project must always be listed")
	assert.Equal(t, 0, byRoot["/repos/active"].SessionCount)

	// Names are basenames and the list is sorted by name.
	assert.Equal(t, "active", byRoot["/repos/active"].Name)
	for i := 1; i < len(got); i++ {
		if got[i-1].Name == got[i].Name {
			continue
		}
		assert.True(t, got[i-1].Name < got[i].Name, "project list should be sorted by name")
	}
}

// TestHandleAddProjectRejectsNonGitPath keeps the overlay open with an inline
// error when the entered path is not a git repository.
func TestHandleAddProjectRejectsNonGitPath(t *testing.T) {
	h := newTestHome(t)
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(nil, h.repoRoot)
	h.state = stateSwitchProject
	// Enter add mode (the add row is selected when there are no projects), which
	// is the only state from which an add is submitted in the real flow.
	h.projectPickerOverlay.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	nonGit := t.TempDir() // a plain dir, not a git repo
	mod, cmd := h.handleAddProject(nonGit)
	require.NotNil(t, mod)
	_ = cmd

	assert.Equal(t, stateSwitchProject, h.state, "an invalid add must keep the picker open")
	require.NotNil(t, h.projectPickerOverlay, "overlay must stay open on an invalid path")
	assert.Contains(t, h.projectPickerOverlay.Render(), "not a git repository")
}

func TestHandleAddProjectRegistersAndSwitches(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	h.projectPickerOverlay = overlay.NewProjectPickerOverlay(nil, h.repoRoot)
	h.state = stateSwitchProject

	// A real git repo so RepoFromPath resolves.
	repoRoot := initTestGitRepo(t)
	mod, _ := h.handleAddProject(repoRoot)
	require.NotNil(t, mod)

	assert.Equal(t, stateDefault, h.state, "a valid add closes the picker")
	assert.Nil(t, h.projectPickerOverlay)
	assert.Equal(t, config.RepoIDFromRoot(repoRoot), h.repoID, "add should switch to the new project")

	// Persisted into root_agents so it appears in the picker next launch.
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	assert.Contains(t, cfg.RootAgents, repoRoot)
}

// initTestGitRepo initializes a bare-minimum git repo in a temp dir and returns
// its resolved main-worktree root (as RepoFromPath would report it).
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	repo, err := config.RepoFromPath(dir)
	require.NoError(t, err)
	return repo.Root
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}
