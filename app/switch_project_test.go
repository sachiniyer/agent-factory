package app

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestSwitchProjectSameRepoIsNoop: switching to the already-active project is a
// no-op that leaves the sidebar untouched.
func TestSwitchProjectSameRepoIsNoop(t *testing.T) {
	h := newTestHome(t)
	h.store.AddInstance(newSnapshotTestInstance(t, "existing"))

	same := &config.RepoContext{Root: h.repoRoot, ID: h.repoID}
	h.switchProject(same)

	require.NotNil(t, findSidebarInstance(h, "existing"), "a same-repo switch must not wipe the sidebar")
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
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
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
