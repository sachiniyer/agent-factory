package app

import (
	"fmt"
	"path/filepath"
	"sort"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// showProjectPickerOverlay opens the project switcher (#1461): every repo af has
// seen — the current project, repos with tracked sessions, and the root_agents
// opt-ins — each with its session count, plus a "+ Add project…" affordance for
// registering a new repo on the fly. The picker navigates like the instances
// rail (up/k, down/j over the full list); there is no search.
func (m *home) showProjectPickerOverlay() (tea.Model, tea.Cmd) {
	projects, registryDegraded := m.buildProjectList()
	m.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, m.repoRoot)
	m.projectPickerOverlay.SetDegraded(registryDegraded)
	if isRemoteTarget() {
		// The picker's registry rows come from the LOCAL config.ListProjects
		// and its path input resolves on THIS filesystem, but a rebind would
		// land on the REMOTE daemon's registry — where the prj_ id likely
		// does not exist, or worse, names a different record. Observation and
		// mutation must come from the same daemon (#3626's rule); until the
		// switcher reads the remote registry too, rebind stays local-only and
		// the remote repair is `af projects rebind` on the daemon host.
		m.projectPickerOverlay.SetRebindDenied("local registry — `af projects rebind` on daemon host")
	}
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
func (m *home) buildProjectList() ([]overlay.Project, bool) {
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
// The second return reports a DEGRADED registry read (#3298): the union can
// still render from the other sources, but presenting it as complete would be
// a failed read shown as an empty result — every registered sessionless
// project silently vanishes from the picker. Callers surface the degradation.
func (m *home) buildProjectListFrom(data []session.InstanceData) ([]overlay.Project, bool) {
	projects, registryDegraded, _ := m.buildProjectListFromCounted(data)
	return projects, registryDegraded
}

// buildProjectListFromCounted is buildProjectListFrom, also reporting the
// resolution budgets the poll opened — one per uncached path, in the order they
// were opened (#3710).
//
// The ledger is here rather than at resolveProjectPaths because the budgets are
// a property of the POLL: this is what reads the durable registry before it
// resolves anything, so that every candidate the registry names is uncached and
// budgeted in the same round as the snapshot's own paths. A test asserting that
// one stalled path did not spend a healthy one's opportunity has to see the
// round the poll actually opened, not one it assembled for itself.
func (m *home) buildProjectListFromCounted(data []session.InstanceData) ([]overlay.Project, bool, []projectPathProbeBudget) {
	type projectAggregate struct {
		root         string
		rootPriority int
		count        int
		inPlace      int
	}
	projectsByID := map[string]*projectAggregate{}

	// Read the durable registry before resolving any paths so every uncached
	// candidate can start its bounded Git probe together. A single stalled path
	// must not consume the entire opportunity of an inactive worktree that comes
	// later in the snapshot or registry.
	registryDegraded := false
	registeredProjects, err := config.ListProjects()
	if err != nil {
		registryDegraded = true
		log.WarningLog.Printf("failed to read the project registry for the switcher: %v", err)
		registeredProjects = nil
	}
	paths := make([]string, 0, len(data)+len(registeredProjects)+1)
	for _, d := range data {
		if !session.IsArchivedData(d) && d.Worktree.RepoPath != "" {
			paths = append(paths, d.Path)
		}
	}
	for _, project := range registeredProjects {
		if !project.PathExists {
			// Absence is stronger than the short cache TTL: discard the cached
			// spelling immediately so it cannot override a surviving worktree.
			delete(m.projectPathResolutions, project.Root)
		}
		paths = append(paths, project.Root)
	}
	if m.appConfig != nil {
		for path := range m.appConfig.RootAgents {
			paths = append(paths, config.ExpandTilde(path))
		}
	}
	paths = append(paths, m.repoRoot)
	resolvedPaths, probeBudgets := m.resolveProjectPaths(paths)
	resolvePath := func(path string) projectPathResolution { return resolvedPaths[path] }
	ensure := func(resolved projectPathResolution, priority int) *projectAggregate {
		if resolved.id == "" || resolved.root == "" {
			return nil
		}
		aggregate := projectsByID[resolved.id]
		if aggregate == nil {
			aggregate = &projectAggregate{}
			projectsByID[resolved.id] = aggregate
		}
		if aggregate.root == "" || priority > aggregate.rootPriority {
			aggregate.root = resolved.root
			aggregate.rootPriority = priority
		}
		return aggregate
	}

	// inPlace counts the subset of each repo's live sessions that delete-project
	// tears down instead of archiving (#1973). Keyed off the SAME predicate the
	// daemon applies in deleteProject — Instance.IsExternalWorktree(), which is
	// exactly Worktree.ExternalWorktree on the wire (ToInstanceData sets it from
	// gitWorktree.IsExternalWorktree(), and both read false when no worktree is
	// attached). Deriving it here, from the snapshot that already yields the
	// total, keeps the dialog's split as faithful as the count beside it.
	for _, d := range data {
		// Only LIVE sessions define an "active project" (#1735): a repo whose
		// sessions are all archived is not an active project — its archived rows
		// live in the sidebar's Archived group and are restorable, which brings
		// the project back. This is what makes delete-project (archive every live
		// session) drop the repo from the list, and restore re-add it.
		if session.IsArchivedData(d) {
			continue
		}
		identityPath := d.Worktree.RepoPath
		if identityPath == "" {
			continue
		}
		// RepoPath is the durable recorded identity. Hash it without walking
		// through Git so a deliberately retained pre-#3358 row cannot be adopted
		// by an enclosing, unrelated repository merely because its old parent
		// path now resolves there.
		identity := projectPathResolution{
			id: config.RepoIDFromRoot(filepath.Clean(identityPath)), root: filepath.Clean(identityPath),
		}
		// Path is the requested operational workspace. Prefer it only when Git
		// proves it belongs to the session's recorded identity; this collapses a
		// bare.git identity and bare-wt workspace into one selectable project
		// without letting an unrelated stale path relabel the row (#3358).
		workspace := resolvePath(d.Path)
		if workspace.id != identity.id {
			workspace = identity
		}
		aggregate := ensure(workspace, 1)
		if aggregate == nil {
			continue
		}
		aggregate.count++
		if d.Worktree.ExternalWorktree {
			aggregate.inPlace++
		}
	}

	// The #2456 union: derived (live sessions above) ∪ registry ∪ root_agents. The
	// registry (config.ListProjects, the #2355 durable project store the daemon writes
	// via RegisterProject) is what NEW adds land in; the root_agents opt-in is kept in
	// the union for repos registered before the add-verb rewire, so an existing opt-in
	// keeps its switcher entry. Both are on-disk config read in-process here, exactly
	// as the counts above come from the daemon's session snapshot passed in.
	// A registration is a durable claim about a PATH, so it may only lend a
	// repository's identity to the union once Git vouches for that exact
	// workspace and its checkout marker (#3358 review). Without the proof a
	// registered nested checkout that lost its .git metadata, or a path taken
	// over by another checkout, resolves upward into an unrelated enclosing
	// repository and merges its row here — where switching opens that foreign
	// checkout and delete-project aims at its sessions and root-agent state.
	//
	// An unproven entry is not dropped: it falls back to the identity hashed
	// from its own recorded root, exactly as a retained session row does above,
	// so a registered project on a stalled or missing checkout keeps its own
	// row instead of vanishing or borrowing an ancestor's.
	provenRegistryIDs := m.resolveRegisteredProjectIdentities(registeredProjects)
	// registryRecordByRow maps each registry record to the row id the union
	// below assigns it — the resolved repo id when Git proves the recorded
	// root, else the reconciled recorded identity (the same split the union
	// makes). It is what lets a row carry the record's prj_… id — what `b`
	// rebinds — and its path_exists flag, so the picker can offer rebind only
	// where there is a registration to move and flag a checkout that is gone.
	registryRecordByRow := make(map[string]config.Project, len(registeredProjects))
	// recordedIdentities remembers which identity each registry row lent to a
	// path, so the root_agents union below can ask rather than re-hash.
	recordedIdentities := make(map[string]string, len(registeredProjects))
	for _, project := range registeredProjects {
		if project.Root == "" {
			continue
		}
		resolved := resolvePath(project.Root)
		provenID, proven := provenRegistryIDs[project.Root]
		rootPriority := 2
		switch {
		case proven:
			resolved.id = provenID
		default:
			// IDENTITY and ROOT SPELLING are separate decisions here, and an
			// unproven row answers them differently.
			//
			// Its identity is the one it RECORDED, never a hash of its path
			// (#3530 review id 3914971730): hashing splits the row from
			// sessions stored under the real id — most visibly once a bare
			// repository's linked worktree disappears — and for a legacy row
			// with nothing recorded it recreates the real/invented collision
			// this change removes.
			//
			// Its path, though, is exactly what could not be verified, so it
			// must not outrank a live workspace for the spelling the user is
			// shown: a vanished or replaced registry root would otherwise
			// relabel a project with a directory that is no longer there.
			// Below session priority, but still above nothing — a registered
			// project with no sessions is still named by its recorded root.
			resolved = projectPathResolution{
				id: config.ReconciledRepoIDForProject(project), root: filepath.Clean(project.Root),
			}
			rootPriority = 0
		}
		// Keyed by the CANONICAL spelling: a root_agents key is written by a
		// human, through whatever symlink they had, while a record stores the
		// path registration resolved (#2110's rule — macOS `/var` ->
		// `/private/var` makes the two unequal every time).
		recordedIdentities[pathutil.ResolveForCompare(filepath.Clean(project.Root))] = resolved.id
		registryRecordByRow[resolved.id] = project
		ensure(resolved, rootPriority)
	}
	if m.appConfig != nil {
		for path := range m.appConfig.RootAgents {
			expanded := config.ExpandTilde(path)
			resolved := resolvePath(expanded)
			// A root_agents key is a PATH, and an unresolvable one falls back
			// to its own hash — which is no longer any project's identity now
			// that a registered project is addressed by the identity it
			// RECORDED (#3530 review id 3916912933). Unioned under that hash it
			// becomes a second row for one project: zero sessions, nothing to
			// open, and a delete that is refused because delete-project
			// normalizes the same path back to the recorded identity.
			//
			// Only the FALLBACK defers to the record. A key whose path Git
			// still resolves belongs to the repository actually there, exactly
			// as rootAgentKeyMatchesRepo reads it — availability is not
			// identity, and a stale row must not claim a live checkout.
			//
			// The active workspace is excluded explicitly: its resolution is
			// pre-seeded from the identity opening the home already
			// established, so it carries no probe timestamp and would
			// otherwise look like a fallback.
			priority := 2
			// Only a probe that ANSWERED "not a repository" may hand this key
			// to a registry row (#3530 review id 3918120760). A timed-out probe
			// leaves resolvedAt zero exactly as a genuinely absent path does,
			// and the daemon will apply this key to whatever repository is
			// actually there — so deferring on an unanswered probe hides a live
			// occupant behind a stale row and sends delete-project an
			// inconsistent id/path pair.
			if resolved.answeredNotARepo && expanded != m.repoRoot {
				if recorded, ok := recordedIdentities[pathutil.ResolveForCompare(filepath.Clean(expanded))]; ok && recorded != "" {
					resolved.id = recorded
					// Identity and root SPELLING are separate decisions, the
					// same split an unproven registry row makes above (#3530
					// review id 3917445677). This key's path is the one that
					// could not be resolved, so it must not outrank a live
					// session's workspace for what the user is shown and
					// switched to — the row would collapse correctly and then
					// name a directory that is not there.
					priority = 0
				}
			}
			ensure(resolved, priority)
		}
	}
	// The active workspace is the best selectable spelling and wins over a
	// registry or session snapshot that names another linked worktree.
	ensure(resolvePath(m.repoRoot), 3)

	projects := make([]overlay.Project, 0, len(projectsByID))
	for repoID, aggregate := range projectsByID {
		projects = append(projects, overlay.Project{
			RepoID:       repoID,
			Name:         filepath.Base(aggregate.root),
			Root:         aggregate.root,
			SessionCount: aggregate.count,
			InPlaceCount: aggregate.inPlace,
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].Root < projects[j].Root
	})
	for i := range projects {
		if rec, ok := registryRecordByRow[projects[i].RepoID]; ok {
			projects[i].RegistryID = rec.ID
			projects[i].MissingPath = !rec.PathExists
		}
	}
	return projects, registryDegraded, probeBudgets
}

// projectRows maps the discovered project list into Projects-section rows,
// marking the active project so the section highlights where the rail is scoped.
func (m *home) projectRows(projects []overlay.Project) []ui.SidebarProject {
	rows := make([]ui.SidebarProject, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, ui.SidebarProject{
			RepoID:       p.RepoID,
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
	projects, registryDegraded := m.buildProjectList()
	m.projects.SetProjects(m.projectRows(projects))
	m.projects.SetDegraded(registryDegraded)
	m.syncProjectsHint()
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
	projects, registryDegraded := m.buildProjectListFrom(data)
	changed := m.projects.SetProjects(m.projectRows(projects))
	if m.projects.SetDegraded(registryDegraded) {
		changed = true
	}
	m.syncProjectsHint()
	return changed
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
	// A pending rebind makes the picker's form consume every key so nothing can
	// race a second mutation — which would include the always-on Ctrl+C hard
	// exit, leaving a TUI whose daemon call has stalled impossible to quit.
	// Quitting is safe mid-flight: the daemon owns the write, not this process.
	if msg.String() == "ctrl+c" && m.projectPickerOverlay.RebindPending() {
		return m.handleQuit()
	}
	if msg.String() == "D" {
		if proj, ok := m.projectPickerOverlay.HighlightedProject(); ok {
			model, cmd := m.handleDeleteProject(ui.SidebarProject{RepoID: proj.RepoID, Name: proj.Name, Root: proj.Root, SessionCount: proj.SessionCount, InPlaceCount: proj.InPlaceCount})
			if m.state == stateConfirm && m.confirmationOverlay != nil {
				m.confirmationOverlay.OnCancel = func() { m.state = stateSwitchProject }
			}
			return model, cmd
		}
	}

	shouldClose := m.projectPickerOverlay.HandleKeyPress(msg)

	// Add-project submit: validate app-side (the overlay must not shell out to
	// git). An invalid path stays open with an inline error; a valid one is
	// registered and switched to.
	if path, ok := m.projectPickerOverlay.TakeAddRequest(); ok {
		return m.handleAddProject(path)
	}

	// Rebind submit: the overlay stays open while the daemon answers so a
	// rejection is corrected inline, mirroring the add flow's error handling.
	if req, ok := m.projectPickerOverlay.TakeRebindRequest(); ok {
		return m.handleRebindProject(req)
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

// handleAddProject validates a user-entered repo path, switches to it immediately,
// and registers it in the #2355 project registry through the daemon OFF the event
// loop. A path that is not a git repository keeps the overlay open with an inline
// error rather than dismissing the user's typing.
func (m *home) handleAddProject(path string) (tea.Model, tea.Cmd) {
	repo, err := config.RepoFromPath(config.ExpandTilde(path))
	if err != nil {
		// A probe that never answered says nothing about what the user typed
		// (#3504): tell them it is worth retrying instead of rejecting the path.
		if config.RepoProbeUnanswered(err) {
			// The path the user typed is rendered on the line directly above
			// this one, and the overlay is narrow: repeating it here pushes the
			// actionable half off the end (play-tested — "try again" was the
			// part that truncated).
			m.projectPickerOverlay.SetAddError("could not check — git did not answer; try again")
			return m, nil
		}
		m.projectPickerOverlay.SetAddError(fmt.Sprintf("not a git repository: %s", path))
		return m, nil
	}
	m.closeProjectPicker()
	// Switch NOW (local + fast), and register through the daemon off the event loop —
	// mirroring deleteProjectCmd (#1735). RegisterProject is a daemon round-trip that
	// blocks on the admission-retry window while the daemon warms, so running it inline
	// (as the pre-#2456 root_agents file write did) would freeze the TUI on this
	// keystroke. The switch does not depend on the write: the active project already
	// shows (buildProjectListFrom seeds it from m.repoRoot); the registry write only
	// has to persist for the switcher union and the next launch, and handleProjectAdded
	// refreshes the section when it lands.
	model, switchCmd := m.switchProject(repo)
	return model, tea.Batch(switchCmd, m.addProjectCmd(repo.Root))
}

// addProjectCmd registers a project through the daemon — the single writer (#960) —
// off the event loop (#2456), mirroring deleteProjectCmd, and reports completion. It
// forwards the LOCALLY-resolved absolute root, not the raw input, so a relative path
// cannot re-resolve against the daemon's cwd (#2491). The receiver is unused (like
// deleteProjectCmd's), kept for symmetry with the sibling verb.
func (m *home) addProjectCmd(root string) tea.Cmd {
	return func() tea.Msg {
		return projectAddedMsg{root: root, err: registerProjectThroughDaemon(root)}
	}
}

// handleProjectAdded finalizes an async add-project (#2456). On success it refreshes
// the Projects section so the newly-registered project appears in the switcher union
// even when the repo has no sessions. A failure is NON-FATAL — the switch already
// happened; the registration just won't persist for the next launch — and is surfaced
// as a warning rather than an error box, matching the pre-#2456 best-effort write.
func (m *home) handleProjectAdded(msg projectAddedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		log.WarningLog.Printf("failed to register project %s in the registry: %v", msg.root, msg.err)
		return m, nil
	}
	m.refreshSidebarProjects()
	return m, nil
}

// handleRebindProject dispatches the picker's `b` verb: rebind a registered
// project's stable id to the checkout at the entered path. The user-typed path
// is resolved against the TUI's filesystem — this process shares the user's
// machine, so ResolveUserPath is correct here exactly as in handleAddProject —
// and the daemon performs the registry write off the event loop, mirroring
// addProjectCmd. The picker stays open until the answer lands: a rejection
// (not a git repo, path owned by another project) is corrected inline via
// SetRebindError, a success closes and refreshes.
func (m *home) handleRebindProject(req overlay.RebindRequest) (tea.Model, tea.Cmd) {
	abs, err := config.ResolveUserPath(req.Path)
	if err != nil {
		m.projectPickerOverlay.SetRebindError(fmt.Sprintf("cannot resolve path: %s", req.Path))
		return m, nil
	}
	req.Path = abs
	return m, m.rebindProjectCmd(req)
}

// rebindProjectCmd rebinds a registered project through the daemon — the single
// writer (#960) — off the event loop, mirroring addProjectCmd/deleteProjectCmd.
// The reply carries the request's token so handleProjectRebound can tell it
// apart from a reply to any other picker.
func (m *home) rebindProjectCmd(req overlay.RebindRequest) tea.Cmd {
	return func() tea.Msg {
		project, err := rebindProjectThroughDaemon(req.Project.RegistryID, req.Path)
		root := project.Root
		if root == "" {
			root = req.Path
		}
		return projectReboundMsg{token: req.Token, projectID: req.Project.RegistryID, name: req.Project.Name, root: root, err: err}
	}
}

// handleProjectRebound finalizes an async rebind. The reply belongs to the
// picker only when that picker is still open AND still waiting on this
// request's token; a picker closed and reopened in the meantime is a different
// request's (or none's), and must neither close nor show this reply's error.
//
// An owned rejection is fed back inline so the user can correct the path; an
// owned success closes the picker with a toast. An unowned reply still refreshes
// the Projects section on success — the registry did change — and reports a
// rejection through the error box, but it announces success with a toast only
// when no picker is on screen, since a toast over a different picker reads as
// that picker's result.
func (m *home) handleProjectRebound(msg projectReboundMsg) (tea.Model, tea.Cmd) {
	pickerOpen := m.projectPickerOverlay != nil && m.state == stateSwitchProject
	owned := pickerOpen && m.projectPickerOverlay.OwnsRebindReply(msg.token)
	if msg.err != nil {
		if owned {
			m.projectPickerOverlay.SetRebindError(msg.err.Error())
			return m, nil
		}
		return m, m.handleError(fmt.Errorf("failed to rebind project %q: %w", msg.name, msg.err))
	}
	m.refreshSidebarProjects()
	if pickerOpen && !owned {
		return m, nil
	}
	if owned {
		m.closeProjectPicker()
	}
	return m, m.showTransientMessage(fmt.Sprintf("Rebound project '%s' to %s", msg.name, msg.root))
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
	restore := fmt.Sprintf("Restore an archived session (%s) to bring the project back.", restoreKey)
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
	if proj.RepoID == "" {
		return m, m.handleError(fmt.Errorf("cannot delete project %q: repository identity is unavailable", proj.Name))
	}
	restoreKey := keys.GlobalKeyBindings[keys.KeyRestore].Help().Key
	message, detail := deleteProjectConfirmMessage(proj.Name, proj.SessionCount, proj.InPlaceCount, restoreKey)
	return m, m.confirmActionWithDetail(message, detail, func() tea.Msg {
		return startDeleteProjectMsg{root: proj.Root, repoID: proj.RepoID, name: proj.Name}
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
// separate attached TUI reflects it on its next launch, matching how every other
// daemon-side config and registry write is picked up) and refreshes the Projects
// section so the now-empty project leaves the list immediately.
//
// When the deleted project was the active one, it ALSO tears the running TUI out
// of that scope and drops it into registry mode. The daemon has just archived
// every live session of m.repoID and removed the registry entry; without a
// re-scope the TUI stays pinned to the deleted identity — open panes keep
// rendering against a stale m.repoID whose tmux the archive step already tore
// down, and refreshSidebarProjects re-derives the deleted project as an
// Active row with SessionCount 0 via the unconditional active-root pre-seed in
// buildProjectListFromCounted. Mirroring switchProject's teardown (close panes,
// reset the projection, clear m.repoID/m.repoRoot, reset the sidebar project
// name, drop the per-project hooks/program/tasks) lets the TUI fall cleanly
// into the NoRegisteredProjectWorkspace / empty-rail state a fresh launch
// outside a repo would see; switchProject is the only other writer to those
// fields, so a re-scope here is the symmetric recovery for an in-session
// action that removed the active identity from under the TUI.
func (m *home) handleProjectDeleted(msg projectDeletedMsg) (tea.Model, tea.Cmd) {
	committedWarning := msg.err != nil && apiclient.IsMutationCommitted(msg.err)
	if msg.err != nil && !committedWarning {
		return m, m.handleError(fmt.Errorf("failed to delete project '%s': %w", msg.name, msg.err))
	}
	if m.appConfig != nil {
		for path := range m.appConfig.RootAgents {
			if repo, err := config.RepoFromPath(config.ExpandTilde(path)); err == nil && repo.ID == msg.repoID {
				delete(m.appConfig.RootAgents, path)
			}
		}
	}
	// Re-scope when the deleted project was the active one. Evaluated before
	// any scope mutation and captured in rescoped because the teardown below
	// clears m.repoID, so a second read of `msg.repoID == m.repoID` after it
	// would silently invert.
	rescoped := msg.repoID == m.repoID
	if rescoped {
		// Persist the OUTGOING project's pane/selection state under its
		// still-current repoID before any scope field changes, exactly as
		// switchProject does. writeTUIViewState is a no-op once m.repoID is
		// empty, so the order is load-bearing.
		m.flushTUIViewStateBestEffort()
		// Close every open pane (releasing its live termpane attachment) so no
		// pane from the deleted project keeps rendering against the stale
		// scope. The daemon's archive step already tore down the tmux sessions
		// backing those panes, so the pane windows were dead attachments
		// regardless — closing their windows matches switchProject and stops
		// the tab-pane watcher from lingering in its "Session lost" fallback.
		for _, p := range append([]*store.OpenPane(nil), m.store.OpenPanes()...) {
			m.closePaneWindow(p)
		}
		m.store.ResetInstances()
		// ResetInstances drops every row but leaves their *session.Instance
		// pointers behind as keys in m.adoptedSnapshotOps. In an active-project
		// switch the next snapshot (scoped to the new repoID) calls pruneTo, but
		// here the re-scope clears m.repoID into registry mode, where
		// handleSnapshot deliberately skips reconcileSnapshot — the only path
		// that calls pruneTo (#3005). Prune explicitly so entries for the
		// deleted project's rows do not pin them in memory indefinitely while
		// the TUI stays in registry mode.
		m.adoptedSnapshotOps.pruneTo(m.store.GetInstances())
		m.initialPaneOpened = false
		m.hasLastTUIViewState = false
		m.repoID = ""
		m.repoRoot = ""
		m.sidebar.SetProjectName("")
		// The in-repo config, hooks, and program are scoped to the deleted
		// project; clear them so nothing from it leaks into the registry-mode
		// TUI (#1686/#2138). The global default_program is what a project that
		// expresses no preference — and registry mode — resolve to, exactly
		// as newHome and switchProject's failure branch set it.
		m.store.SetHookCount(0)
		m.hooksPane.SetCommands(nil)
		if m.appConfig != nil {
			m.program = m.appConfig.DefaultProgram
		}
		// Tasks are per-project and LoadTasksForCurrentRepo needs a cwd repo,
		// so with no active project the automations strip is empty — not
		// errored — until a project is selected, matching the empty session
		// rail. Reset (not Set) so a held edit against the deleted project's
		// list does not follow the user into registry mode.
		m.store.SetTasks(nil)
		sp := m.automations.TaskPane()
		sp.ResetTasks(nil)
		// ResetTasks clears the backing list but leaves a held create/edit
		// form's `creating` and `hasFocus` set; the form captured its editPath
		// at EnterCreateMode time (from m.repoRoot), so a draft submitted
		// AFTER the rescope clears m.repoID would persist a task pinned to
		// the deleted project's path and its scheduler entry could recreate
		// sessions for it. SetFocus(false) cancels the form (clears creating/
		// editing/pendingCreate/pendingTrigger), matching the intent the
		// comment above already claims. The hooks overlay's save target is
		// m.repoRoot — already empty here — so a held hook add/edit would
		// either error or land in the wrong place; close it the same way.
		sp.SetFocus(false)
		m.hooksPane.SetFocus(false)
		// State overlays scoped to the deleted project (stateTasks/stateHooks)
		// keep their overlay rendered against the cleared identity once
		// m.repoID is empty. Close them so neither the task create form nor
		// the hooks editor stays pinned to a project the user just removed.
		if m.state == stateTasks || m.state == stateHooks {
			m.state = stateDefault
		}
	}
	m.refreshSidebarProjects()
	if rescoped {
		// Re-home focus on the tree (the rail is now empty — a focused pane
		// region just vanished with the closed panes) and re-solve the grid so
		// the cleared project rows and closed pane regions stop reserving
		// space. Runs after refreshSidebarProjects so the grid sizes against
		// the new, scope-cleared project list, matching switchProject's order.
		m.focusTreeForNav()
		m.relayout()
		// The Sessions tree is empty after the rescope, so focus on its rail
		// leaves `j`/`k` and Enter inert. When other projects remain, land
		// focus on the Projects section instead — the only directly
		// actionable region in registry mode — matching newHome's
		// registry-mode initialization. Ring.Focus refuse to leave the
		// Projects region hidden (the grid hides it for <=1 row), so this is
		// a no-op when the section is not visible.
		if m.projects.HasProjects() {
			m.ring.Focus(layout.RegionProjects)
			m.syncFocus()
		}
	}
	success := m.showTransientMessage(deleteProjectResultMessage(msg.name, msg.archived, msg.killed))
	if committedWarning {
		return m, tea.Batch(success, m.handleError(msg.err))
	}
	return m, success
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

	// Re-resolve the new project's default program for future sessions.
	// BranchPrefix and other machine preferences are global-only, so they do not
	// change on switch.
	//
	// m.program is PROJECT-scoped state, so every path out of this block must
	// land on a value derived from the INCOMING project: its own default_program
	// when it sets one, the global default otherwise. What none of them may do is
	// leave the OUTGOING project's program in place — task creation falls back to
	// m.program without re-resolving config and PERSISTS it into tasks.json, so a
	// carried-over value silently runs this project's tasks under the previous
	// project's agent (#2138). Session creation is separately covered:
	// preflightSessionCreate re-resolves and blocks.
	if resolved, err := config.ResolveConfigForRepo(repo); err == nil {
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

	if tasks, err := task.LoadTasksForKnownRepo(repo.Root, repo.ID); err != nil {
		log.WarningLog.Printf("switch project: failed to load tasks for %s: %v", repo.Root, err)
		m.store.SetTasks(nil)
		m.automations.TaskPane().ResetTasks(nil)
	} else {
		m.store.SetTasks(tasks)
		// Reset, not reconcile: an edit held against the outgoing project's
		// list must not follow the user into this one.
		m.automations.TaskPane().ResetTasks(tasks)
	}

	m.restoreTUIViewStateOnLaunch()
	// Re-derive the Projects section for the new scope so its active marker and
	// counts follow the switch.
	m.refreshSidebarProjects()
	m.focusTreeForNav()
	m.relayout()
	return m, tea.Sequence(tea.WindowSize(), m.selectionChanged())
}
