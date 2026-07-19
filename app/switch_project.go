package app

import (
	"fmt"
	"path/filepath"
	"sort"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// showProjectPickerOverlay opens the project switcher (#1461): every repo af has
// seen — the current project, repos with tracked sessions, and the root_agents
// opt-ins — each with its session count, plus a "+ Add project…" affordance for
// registering a new repo on the fly. The picker navigates like the instances
// rail (up/k, down/j over the full list); there is no search.
func (m *home) showProjectPickerOverlay() (tea.Model, tea.Cmd) {
	projects := m.buildProjectList()
	m.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, m.repoRoot)
	m.projectPickerOverlay.SetWidth(60)
	m.layoutProjectPickerOverlay()
	m.state = stateSwitchProject
	return m, nil
}

// buildProjectList derives the picker's project list with zero config: it fetches
// the daemon's cross-repo session snapshot and groups it by repo root for the
// session counts, then unions in the root_agents opt-ins and the active project.
// Used by the on-demand ctrl+p picker, which fetches synchronously; the always-
// visible Projects section rebuilds from buildProjectListFrom on the background
// poll instead (no second on-loop RPC).
func (m *home) buildProjectList() []overlay.Project {
	data, err := allReposSnapshotFetcher()
	if err != nil {
		log.WarningLog.Printf("project picker: failed to list cross-repo sessions: %v", err)
		data = nil
	}
	return m.buildProjectListFrom(data)
}

// buildProjectListFrom derives the project list from an already-fetched cross-repo
// session snapshot: it groups the sessions by repo root for the counts, then
// unions in the root_agents opt-ins and the active project so repos with no live
// session still appear. Names are the repo basename; ties break by root. Split
// out from buildProjectList so the background poll can reuse the all-repos data
// it already fetched off-loop rather than issuing a second daemon RPC.
func (m *home) buildProjectListFrom(data []session.InstanceData) []overlay.Project {
	counts := map[string]int{}
	// inPlace counts the subset of each repo's live sessions that delete-project
	// tears down instead of archiving (#1973). Keyed off the SAME predicate the
	// daemon applies in deleteProject — Instance.IsExternalWorktree(), which is
	// exactly Worktree.ExternalWorktree on the wire (ToInstanceData sets it from
	// gitWorktree.IsExternalWorktree(), and both read false when no worktree is
	// attached). Deriving it here, from the snapshot that already yields the
	// total, keeps the dialog's split as faithful as the count beside it.
	inPlace := map[string]int{}
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

	for _, d := range data {
		// Only LIVE sessions define an "active project" (#1735): a repo whose
		// sessions are all archived is not an active project — its archived rows
		// live in the sidebar's Archived group and are restorable, which brings
		// the project back. This is what makes delete-project (archive every live
		// session) drop the repo from the list, and restore re-add it.
		if session.IsArchivedData(d) {
			continue
		}
		root := d.Worktree.RepoPath
		if root == "" {
			continue
		}
		seen(root)
		counts[root]++
		if d.Worktree.ExternalWorktree {
			inPlace[root]++
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
			InPlaceCount: inPlace[root],
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

// projectRows maps the discovered project list into Projects-section rows,
// marking the active project so the section highlights where the rail is scoped.
func (m *home) projectRows(projects []overlay.Project) []ui.SidebarProject {
	rows := make([]ui.SidebarProject, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, ui.SidebarProject{
			Name:         p.Name,
			Root:         p.Root,
			SessionCount: p.SessionCount,
			InPlaceCount: p.InPlaceCount,
			Active:       p.Root == m.repoRoot,
		})
	}
	return rows
}

// refreshSidebarProjects rebuilds the bottom Projects section from the same
// cross-repo discovery the ctrl+p picker uses (buildProjectList). Pushed at
// launch and on project switch (paths that fetch synchronously).
func (m *home) refreshSidebarProjects() {
	m.projects.SetProjects(m.projectRows(m.buildProjectList()))
}

// refreshSidebarProjectsFromSnapshot rebuilds the Projects rows from the all-repos
// session snapshot the background poll already fetched off-loop, so the always-
// visible counts stay live when sessions change in ANOTHER repo — without a
// second on-loop daemon RPC. A fetch error leaves the last-known rows intact
// (like handleSnapshot/refreshTasks). Returns whether the visible rows changed.
func (m *home) refreshSidebarProjectsFromSnapshot(data []session.InstanceData, fetchErr error) bool {
	if fetchErr != nil {
		log.WarningLog.Printf("failed to refresh projects section: %v", fetchErr)
		return false
	}
	return m.projects.SetProjects(m.projectRows(m.buildProjectListFrom(data)))
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

// sessionWord pluralizes "session" for the delete-project copy.
func sessionWord(n int) string {
	if n == 1 {
		return "session"
	}
	return "sessions"
}

// deleteProjectConfirmMessage builds the delete-project confirmation (#1735,
// corrected in #1973). It returns the copy in two parts, because the overlay
// clips from the bottom and this dialog's tail was the half that mattered:
//
//   - critical — the consequences the user is consenting to, one count per
//     outcome. The overlay guarantees this renders in full or refuses the
//     action outright, so it must stay short enough to fit the declared 40x10
//     floor (ui/layout/grid.go HardMinWidth/HardMinHeight): a ~34-column text
//     rect leaves four body lines. That budget is why these lines are headlines
//     rather than prose.
//   - detail — the elaboration, which may be clipped (and says when it is).
//
// The destructive count LEADS. A user who reads exactly one line must read the
// one that cannot be undone; the reassuring half is what gives ground first.
//
// The split is honest in both directions. Tearing down an in-place session does
// NOT destroy the user's work — the worktree is theirs, and GitWorktree.Cleanup()
// no-ops for an external worktree, so the branch and uncommitted changes survive.
// What does not survive is the session: af deletes its record, so `af sessions
// restore` cannot bring it back. Saying "you lose your work" would be false;
// saying "restorable" is the bug.
func deleteProjectConfirmMessage(name string, total, inPlace int, restoreKey string) (critical, detail string) {
	archived := total - inPlace
	title := fmt.Sprintf("[!] Delete project '%s'?", name)

	killedLine := fmt.Sprintf("%d in-place %s torn down — not restorable.", inPlace, sessionWord(inPlace))
	archivedLine := fmt.Sprintf("%d %s archived — restorable.", archived, sessionWord(archived))
	gone := "Its worktree is yours — the branch and uncommitted changes stay exactly where they are, but the session and its agent are gone."
	restore := fmt.Sprintf("Restore an archived session (%s, or `af sessions restore`) to bring the project back.", restoreKey)
	repoSafe := "Your real git repository is untouched."

	switch {
	case total == 0:
		return title + "\nIt has no live sessions — it just leaves the projects list.", repoSafe
	case inPlace == 0:
		return title + "\n" + archivedLine,
			"tmux torn down, worktrees moved out — branches and uncommitted work preserved.\n\n" + restore + " " + repoSafe
	case archived == 0:
		return title + "\n" + killedLine, gone + " " + repoSafe
	default:
		return title + "\n" + killedLine + "\n" + archivedLine,
			gone + "\n\n" + restore + " " + repoSafe
	}
}

// deleteProjectResultMessage reports what delete-project ACTUALLY did, using the
// daemon's own counts rather than the pre-confirm estimate (#1973), so the split
// the user consented to is the split they are told about afterward.
//
// The torn-down fragment leads on a mixed delete, deliberately. This lands in
// the one-line transient notice, which the error box clips to the pane width —
// play-testing an 80-col-ish sidebar cut a killed-last message at "tore down 1
// in-place se…", losing exactly the half the user needs. The clipped tail must
// be the reassuring half (what survived), never the consequential one (what did
// not). The full string stays reachable via the notice's details view.
func deleteProjectResultMessage(name string, archived, killed int) string {
	switch {
	case archived == 0 && killed == 0:
		return fmt.Sprintf("Deleted project '%s' — no live sessions to remove", name)
	case killed == 0:
		return fmt.Sprintf("Deleted project '%s' — archived %d %s (restorable)", name, archived, sessionWord(archived))
	case archived == 0:
		return fmt.Sprintf("Deleted project '%s' — tore down %d in-place %s (not restorable, worktree and branch untouched)", name, killed, sessionWord(killed))
	default:
		return fmt.Sprintf(
			"Deleted project '%s' — tore down %d in-place %s (not restorable, worktree and branch untouched) · archived %d %s (restorable)",
			name, killed, sessionWord(killed), archived, sessionWord(archived),
		)
	}
}

// handleDeleteProject opens the delete-project confirmation for the cursor's
// project in the Projects section (#1735). The copy states the real split — what
// is archived and restorable, and what is torn down and is not (#1973) — because
// this message is the entire basis on which the user consents to a destructive
// action. On confirm it dispatches the async daemon archive-then-remove.
func (m *home) handleDeleteProject(proj ui.SidebarProject) (tea.Model, tea.Cmd) {
	repoID := config.RepoIDFromRoot(proj.Root)
	restoreKey := keys.GlobalKeyBindings[keys.KeyRestore].Help().Key
	message, detail := deleteProjectConfirmMessage(proj.Name, proj.SessionCount, proj.InPlaceCount, restoreKey)
	return m, m.confirmActionWithDetail(message, detail, func() tea.Msg {
		return startDeleteProjectMsg{root: proj.Root, repoID: repoID, name: proj.Name}
	})
}

// deleteProjectCmd runs the daemon archive-then-remove off the event loop
// (#1735), mirroring archiveInstanceCmd, and reports completion.
func (m *home) deleteProjectCmd(msg startDeleteProjectMsg) tea.Cmd {
	return func() tea.Msg {
		resp, err := deleteProjectThroughDaemon(msg.root, msg.repoID)
		return projectDeletedMsg{
			root:     msg.root,
			repoID:   msg.repoID,
			name:     msg.name,
			archived: resp.ArchivedCount,
			// KilledCount is the in-place sessions the daemon tore down. Carrying
			// it is what lets the completion report the same archived-vs-torn-down
			// split the confirmation promised (#1973); dropping it is how the TUI
			// came to claim everything was restorable.
			killed: resp.KilledCount,
			err:    err,
		}
	}
}

// handleProjectDeleted finalizes an async delete-project (#1735). On success it
// drops the local root_agents opt-in mirror (the daemon removed it on disk; a
// separate attached TUI reflects it on its next launch, matching how
// RegisterRootAgent's persistence is picked up) and refreshes the Projects
// section so the now-empty project leaves the list immediately.
func (m *home) handleProjectDeleted(msg projectDeletedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("failed to delete project '%s': %w", msg.name, msg.err))
	}
	if m.appConfig != nil {
		for path := range m.appConfig.RootAgents {
			if repo, err := config.RepoFromPath(config.ExpandTilde(path)); err == nil && repo.ID == msg.repoID {
				delete(m.appConfig.RootAgents, path)
			}
		}
	}
	m.refreshSidebarProjects()
	return m, m.showTransientMessage(deleteProjectResultMessage(msg.name, msg.archived, msg.killed))
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

	// Fetch the INCOMING project's snapshot FIRST, while the outgoing project is
	// still fully intact. Everything below this point is unconditional mutation
	// with no way back — the panes are torn down and the projection reset — so a
	// fetch failure (a wedged or unreachable daemon, common against a remote
	// target) must abort here, leaving the current project exactly as it was,
	// rather than stranding the TUI on the new repoID with an empty sidebar
	// (#1788).
	data, err := m.fetchColdStartSnapshot(repo.ID)
	if err != nil {
		return m, m.handleError(fmt.Errorf("failed to load sessions for %s: %w", filepath.Base(repo.Root), err))
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
	//
	// m.program is PROJECT-scoped state, so every path out of this block must
	// land on a value derived from the INCOMING project: its own default_program
	// when it sets one, the global default otherwise. What none of them may do is
	// leave the OUTGOING project's program in place — task creation falls back to
	// m.program without re-resolving config and PERSISTS it into tasks.json, so a
	// carried-over value silently runs this project's tasks under the previous
	// project's agent (#2138). Session creation is separately covered:
	// preflightSessionCreate re-resolves and blocks.
	if resolved, err := config.ResolveConfig(repo.Root); err == nil {
		// A project that sets no default_program already arrives here as the
		// global default: ResolveConfig seeds DefaultProgram from the global
		// config and only overwrites it with a non-empty in-repo value, and a
		// global config that loaded successfully always carries a valid — hence
		// non-empty — default_program (validateConfig runs it through
		// ValidateProgramEnum, which rejects ""). The empty case is therefore
		// unreachable today; the else keeps the rule above true by construction
		// rather than by that chain of reasoning holding forever.
		if resolved.DefaultProgram != "" {
			m.program = resolved.DefaultProgram
		} else if m.appConfig != nil {
			m.program = m.appConfig.DefaultProgram
		}
		m.store.SetHookCount(len(resolved.PostWorktreeCommands))
		m.hooksPane.SetCommands(resolved.PostWorktreeCommands)
	} else {
		log.WarningLog.Printf("switch project: failed to resolve config for %s: %v", repo.Root, err)
		// Clear the hooks pane so the OUTGOING project's hooks cannot leak into
		// the new project. m.repoRoot already points at the new project, so a save
		// from a stale pane would write the previous project's hooks into this
		// project's in-repo config (#1686). At startup the pane starts empty, so
		// this only matters on an in-place switch.
		m.store.SetHookCount(0)
		m.hooksPane.SetCommands(nil)
		// Same reasoning for the program (#2138): a config we cannot parse tells
		// us nothing about this project's preference, so fall back to the global
		// default — the value a project that expresses no preference gets. This
		// branch is where the leak actually shipped, because it was the one path
		// that left m.program untouched.
		if m.appConfig != nil {
			m.program = m.appConfig.DefaultProgram
		}
	}

	// Re-prime the projection from the snapshot fetched above (scoped to the new
	// repoID by the daemon — no cross-repo bleed). Persisted remote-hook sessions
	// arrive on this snapshot too. Materializing cannot fail as a whole: an
	// unrestorable record is skipped, so the switch is committed from here on.
	m.materializeSnapshot(data)

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
