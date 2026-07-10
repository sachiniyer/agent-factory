package app

import (
	"fmt"
	"path/filepath"
	"sort"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// showProjectPickerOverlay opens the searchable project switcher (#1461): every
// repo af has seen — the current project, repos with tracked sessions, and the
// root_agents opt-ins — each with its session count, plus a "+ Add project…"
// affordance for registering a new repo on the fly.
func (m *home) showProjectPickerOverlay() (tea.Model, tea.Cmd) {
	projects := m.buildProjectList()
	m.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, m.repoRoot)
	m.projectPickerOverlay.SetWidth(60)
	m.layoutProjectPickerOverlay()
	m.state = stateSwitchProject
	return m, nil
}

// buildProjectList derives the picker's project list with zero config: it groups
// the daemon's cross-repo session snapshot by repo root for the session counts,
// then unions in the root_agents opt-ins and the active project so repos with no
// live session still appear. Names are the repo basename; ties break by root.
func (m *home) buildProjectList() []overlay.Project {
	counts := map[string]int{}
	var order []string
	seen := func(root string) {
		if root == "" {
			return
		}
		if _, ok := counts[root]; !ok {
			counts[root] = 0
			order = append(order, root)
		}
	}

	if data, err := allReposSnapshotFetcher(); err != nil {
		log.WarningLog.Printf("project picker: failed to list cross-repo sessions: %v", err)
	} else {
		for _, d := range data {
			root := d.Worktree.RepoPath
			if root == "" {
				continue
			}
			seen(root)
			counts[root]++
		}
	}

	if m.appConfig != nil {
		for path := range m.appConfig.RootAgents {
			if repo, err := config.RepoFromPath(config.ExpandTilde(path)); err == nil {
				seen(repo.Root)
			}
		}
	}
	seen(m.repoRoot)

	projects := make([]overlay.Project, 0, len(order))
	for _, root := range order {
		projects = append(projects, overlay.Project{
			Name:         filepath.Base(root),
			Root:         root,
			SessionCount: counts[root],
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].Root < projects[j].Root
	})
	return projects
}

// refreshSidebarProjects rebuilds the bottom Projects section's row list from
// the same cross-repo discovery the ctrl+p picker uses (buildProjectList),
// marking the active project so the section highlights where the rail is
// scoped. Pushed at launch and on project switch, so its counts track the
// picker without a per-frame daemon round-trip.
func (m *home) refreshSidebarProjects() {
	projects := m.buildProjectList()
	rows := make([]ui.SidebarProject, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, ui.SidebarProject{
			Name:         p.Name,
			Root:         p.Root,
			SessionCount: p.SessionCount,
			Active:       p.Root == m.repoRoot,
		})
	}
	m.projects.SetProjects(rows)
}

// switchToProjectRoot resolves a Projects-section row's repo root and switches
// the rail to it, reusing the #1461 switchProject path. A root that no longer
// resolves (repo moved/removed) surfaces an error rather than silently doing
// nothing. Switching to the already-active project is a no-op inside
// switchProject.
func (m *home) switchToProjectRoot(root string) (tea.Model, tea.Cmd) {
	repo, err := config.RepoFromPath(root)
	if err != nil {
		return m, m.handleError(fmt.Errorf("cannot open project %q: %w", filepath.Base(root), err))
	}
	return m.switchProject(repo)
}

// handleStateSwitchProject routes key events to the project picker overlay and
// consumes its outcomes: an add request (validate + register + switch), a chosen
// existing project (switch), or a cancel (close).
func (m *home) handleStateSwitchProject(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	shouldClose := m.projectPickerOverlay.HandleKeyPress(msg)

	// Add-project submit: validate app-side (the overlay must not shell out to
	// git). An invalid path stays open with an inline error; a valid one is
	// registered and switched to.
	if path, ok := m.projectPickerOverlay.TakeAddRequest(); ok {
		return m.handleAddProject(path)
	}

	if !shouldClose {
		return m, nil
	}

	var target *config.RepoContext
	if proj, ok := m.projectPickerOverlay.SelectedProject(); ok {
		repo, err := config.RepoFromPath(proj.Root)
		if err != nil {
			m.closeProjectPicker()
			return m, m.handleError(fmt.Errorf("cannot open project %q: %w", proj.Name, err))
		}
		target = repo
	}
	m.closeProjectPicker()
	if target != nil {
		return m.switchProject(target)
	}
	return m, nil
}

// handleAddProject validates a user-entered repo path, registers it in the
// global root_agents opt-in list (so it persists in the picker), and switches to
// it. A path that is not a git repository keeps the overlay open with an inline
// error rather than dismissing the user's typing.
func (m *home) handleAddProject(path string) (tea.Model, tea.Cmd) {
	repo, err := config.RepoFromPath(config.ExpandTilde(path))
	if err != nil {
		m.projectPickerOverlay.SetAddError(fmt.Sprintf("not a git repository: %s", path))
		return m, nil
	}
	if _, err := config.RegisterRootAgent(repo.Root); err != nil {
		// Non-fatal: the switch still works this session; it just won't be
		// remembered for the next launch.
		log.WarningLog.Printf("failed to register project %s in root_agents: %v", repo.Root, err)
	} else if m.appConfig != nil {
		if m.appConfig.RootAgents == nil {
			m.appConfig.RootAgents = map[string]config.RootAgentConfig{}
		}
		if _, ok := m.appConfig.RootAgents[repo.Root]; !ok {
			m.appConfig.RootAgents[repo.Root] = config.RootAgentConfig{}
		}
	}
	m.closeProjectPicker()
	return m.switchProject(repo)
}

func (m *home) closeProjectPicker() {
	m.projectPickerOverlay = nil
	m.state = stateDefault
}

// switchProject re-scopes the running TUI to repo in place (#1461): it flushes
// the outgoing project's view state, tears down its panes (and their live
// termpane PTYs), resets the projection, then re-primes everything — sidebar,
// tasks, hooks, view state — from the new project's daemon snapshot, which the
// daemon already filters by repoID (so the sidebar shows ONLY the new project).
// New sessions/tabs then target the new repoRoot. A no-op when already viewing
// repo.
func (m *home) switchProject(repo *config.RepoContext) (tea.Model, tea.Cmd) {
	if repo.ID == m.repoID {
		return m, nil
	}

	// Persist the OUTGOING project's pane/selection state under its still-current
	// repoID before anything changes.
	m.flushTUIViewStateBestEffort()

	// Close every open pane (releasing its live termpane attachment) so no stale
	// pane from the previous project can render against the new repo.
	for _, p := range append([]*store.OpenPane(nil), m.store.OpenPanes()...) {
		m.closePaneWindow(p)
	}
	m.store.ResetInstances()
	m.initialPaneOpened = false
	m.hasLastTUIViewState = false

	m.repoID = repo.ID
	m.repoRoot = repo.Root
	m.sidebar.SetProjectName(filepath.Base(repo.Root))

	// Re-resolve the new project's default program for future sessions. AutoYes,
	// BranchPrefix, etc. are global-only, so they do not change on switch.
	if resolved, err := config.ResolveConfig(repo.Root); err == nil {
		if resolved.DefaultProgram != "" {
			m.program = resolved.DefaultProgram
		}
		m.store.SetHookCount(len(resolved.PostWorktreeCommands))
		m.hooksPane.SetCommands(resolved.PostWorktreeCommands)
	} else {
		log.WarningLog.Printf("switch project: failed to resolve config for %s: %v", repo.Root, err)
	}

	// Re-prime the projection from the new project's snapshot (scoped to the new
	// repoID by the daemon — no cross-repo bleed). Persisted remote-hook sessions
	// arrive on this snapshot too; the launch-time importRemoteHookSessions
	// discovery is deliberately NOT re-run here because it resolves the CWD repo,
	// not the switched-to one — newly-discovered remote sessions surface on the
	// next launch instead.
	if err := m.coldStartFromSnapshot(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to load sessions for %s: %w", filepath.Base(repo.Root), err))
	}

	if tasks, err := task.LoadTasksForRepo(repo.Root); err != nil {
		log.WarningLog.Printf("switch project: failed to load tasks for %s: %v", repo.Root, err)
		m.store.SetTasks(nil)
		m.automations.TaskPane().SetTasks(nil)
	} else {
		m.store.SetTasks(tasks)
		m.automations.TaskPane().SetTasks(tasks)
	}

	m.restoreTUIViewStateOnLaunch()
	// Re-derive the Projects section for the new scope so its active marker and
	// counts follow the switch.
	m.refreshSidebarProjects()
	m.focusTreeForNav()
	m.relayout()
	return m, tea.Sequence(tea.WindowSize(), m.selectionChanged())
}
