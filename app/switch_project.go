package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/projectclone"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
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
	projects, registryDegraded, snapshotDegraded := m.buildProjectListWithSnapshotStatus()
	m.projectPickerOverlay = overlay.NewProjectPickerOverlay(projects, m.repoRoot)
	m.projectPickerOverlay.SetDegraded(registryDegraded)
	// Surface the snapshot-fetch failure as a first-class degraded state so
	// the destructive D path can gate on it: a fetch failure zeros every
	// project's live count, and the consent copy reads those counts, so the
	// dialog could otherwise claim "no live sessions" for a project whose
	// live sessions the daemon will still archive and tear down (#1973/#1985).
	m.projectPickerOverlay.SetSnapshotDegraded(snapshotDegraded)
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
// poll instead (no second on-loop RPC). The returned degraded flag covers only
// the registry read; the picker's own snapshot-fetch failure is reported by
// buildProjectListWithSnapshotStatus.
func (m *home) buildProjectList() ([]overlay.Project, bool) {
	projects, registryDegraded, _ := m.buildProjectListWithSnapshotStatus()
	return projects, registryDegraded
}

// projectSnapshotFetchTimeout bounds the cross-repo snapshot read the project
// picker and the Projects section make synchronously on the event loop. It is
// deliberately short — the loop is parked for up to this long — where
// rebindConflictRefreshTimeout bounds the same fetch off-loop and can afford
// ten seconds. A var so tests can shrink it.
var projectSnapshotFetchTimeout = 3 * time.Second

// buildProjectListWithSnapshotStatus is buildProjectList, also reporting whether
// the picker's cross-repo session snapshot fetch FAILED. The failure is a
// first-class degraded state, distinct from the registry read's: the union
// still renders (registry, active root, root_agents), but every project's
// SessionCount/InPlaceCount in it is silently zeroed, so a confirm dialog built
// from those counts can claim "It has no live sessions" for a project whose
// live sessions the daemon will still archive and tear down (#1973/#1985). The
// picker's destructive D path gates on this third return; callers that are not
// destructive consumers of the counts (refreshSidebarProjects) drop it via
// buildProjectList.
func (m *home) buildProjectListWithSnapshotStatus() ([]overlay.Project, bool, bool) {
	// This runs on the bubbletea event loop (Ctrl+P, launch, project switch,
	// add/delete/rebind completion), and the local daemon transport bounds only
	// the dial, so a daemon that accepts the connection and never answers would
	// park Update — key input, repaint and the Ctrl+C hard exit with it — until
	// the process is killed (#6045). The deadline rides the transport read, and
	// its expiry lands in the same snapshotDegraded path as any failed fetch.
	ctx, cancel := context.WithTimeout(context.Background(), projectSnapshotFetchTimeout)
	defer cancel()
	data, err := allReposSnapshotFetcher(ctx)
	snapshotDegraded := false
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("no answer from the daemon within %s: %w", projectSnapshotFetchTimeout, err)
		}
		log.WarningLog.Printf("project picker: failed to list cross-repo sessions: %v", err)
		snapshotDegraded = true
		data = nil
	}
	projects, registryDegraded := m.buildProjectListFrom(data)
	return projects, registryDegraded, snapshotDegraded
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
	if isRemoteTarget() {
		projects, degraded := m.buildRemoteProjectList(data)
		return projects, degraded, nil
	}
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
			projects[i].RegistryRoot = rec.Root
			projects[i].RegistryCheckoutID = rec.CheckoutID
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

// sidebarProjectRows is projectRows' inverse for the merge fallback in
// applySidebarProjects: a pane row has no registry fields, so the converted row
// is session-derived — the merge preserves it unconditionally, and the coalesce
// grafts a fresh registration's identity back onto it when the RepoID matches.
func sidebarProjectRows(rows []ui.SidebarProject) []overlay.Project {
	projects := make([]overlay.Project, 0, len(rows))
	for _, r := range rows {
		projects = append(projects, overlay.Project{
			RepoID:       r.RepoID,
			Name:         r.Name,
			Root:         r.Root,
			SessionCount: r.SessionCount,
			InPlaceCount: r.InPlaceCount,
		})
	}
	return projects
}

// registryBackedRows filters a row set to the registry-backed half — the rows
// a degraded registry read cannot prove absent, for a merge where the session
// snapshot is live truth and the old session rows would only shadow it.
func registryBackedRows(rows []overlay.Project) []overlay.Project {
	registry := make([]overlay.Project, 0, len(rows))
	for _, r := range rows {
		if r.RegistryID != "" {
			// Both callers run this only when the session snapshot landed and
			// the REGISTRY read degraded, so the snapshot is the authority on
			// counts: a repo it does not count has none. A preserved row that
			// a fresh session row later claims gets the live counts grafted on
			// (#4888 review); carrying the stale tallies instead would keep
			// dead sessions on screen for as long as the registry stays
			// degraded.
			r.SessionCount = 0
			r.InPlaceCount = 0
			registry = append(registry, r)
		}
	}
	return registry
}

// refreshSidebarProjects rebuilds the bottom Projects section from the same
// cross-repo discovery the ctrl+p picker uses (buildProjectList). Pushed at
// launch and on project switch (paths that fetch synchronously).
func (m *home) refreshSidebarProjects() {
	projects, registryDegraded := m.buildProjectList()
	m.setSidebarProjects(projects, registryDegraded)
}

// setSidebarProjects is the funnel every Projects-section update flows
// through: it records the applied row set in sidebarProjects (the merge
// source a session-snapshot failure reads) before rendering it.
func (m *home) setSidebarProjects(projects []overlay.Project, registryDegraded bool) {
	m.sidebarProjects = projects
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
	projects, registryDegraded := m.buildProjectListFrom(data)
	return m.applySidebarProjects(projects, registryDegraded, fetchErr)
}

// applySidebarProjects pushes an already-built project list into the Projects
// section, for callers that built the list for another consumer too — the
// section and that consumer must describe the SAME registry read (#4888
// review). A fetch error means the SESSION half never answered; the registry
// half may still have read fine, so the section merges the fresh registry
// rows into its last-known set rather than discarding them — a rebind that
// moved a record between polls would otherwise leave the old root on screen
// indefinitely while the poll stays unhealthy (#4888 review). Returns whether
// the visible rows changed.
func (m *home) applySidebarProjects(projects []overlay.Project, registryDegraded bool, fetchErr error) bool {
	if fetchErr != nil || registryDegraded {
		if fetchErr != nil {
			log.WarningLog.Printf("failed to refresh projects section: %v", fetchErr)
		}
		old := m.sidebarProjects
		if len(old) == 0 {
			// The pane can already hold rows the shadow never recorded —
			// installed before the field existed, or by a direct SetProjects.
			// An unseeded shadow would merge from nil and empty the visible
			// list behind a warning it cannot confirm (#4888 review); the
			// pane's current rows are the last-known set either way.
			old = sidebarProjectRows(m.projects.Projects())
		}
		if fetchErr == nil {
			// The session snapshot landed — only the registry read degraded.
			// Old session rows are stale duplicates of the fresh live ones;
			// only registry-backed rows have absence left unproven.
			old = registryBackedRows(old)
		}
		projects = overlay.MergePreservingSessionRows(old, projects, registryDegraded)
	}
	changed := m.projects.SetProjects(m.projectRows(projects))
	m.sidebarProjects = projects
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
	repo, err := m.repoContextForSelection(root, "")
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
			// The picker's session counts come from the cross-repo snapshot
			// buildProjectList fetches on Ctrl+P; a fetch failure zeros every
			// project's count, and deleteProjectConfirmMessage's "total == 0"
			// consent branch is "It has no live sessions — it just leaves the
			// projects list" — a destructive action consented to on copy the
			// data behind it never confirmed while the daemon still archives
			// and tears down whatever live sessions actually exist (#1973/#1985).
			// Fail closed like switchProject does on its own snapshot fetch:
			// refuse the verb with a notice and keep the picker open. The user
			// re-opens it (a fresh Ctrl+P triggers a fresh fetch) to retry once
			// the daemon is healthy.
			if m.projectPickerOverlay.SnapshotDegraded() {
				return m, m.handleNotice(fmt.Errorf(
					"cannot delete project %q: live-session counts are unavailable (the cross-repo snapshot fetch failed); "+
						"close the picker (Esc) and reopen it (Ctrl+P) to retry",
					proj.Name))
			}
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
		repo, err := m.repoContextForSelection(proj.Root, proj.RepoID)
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
	if projectclone.LooksLikeURL(path) {
		// A git URL (W9.10) is cloned by the DAEMON, on its host, so there is
		// nothing to probe or switch to here: the project appears in the list
		// when the registration lands.
		url := strings.TrimSpace(path)
		if err := projectclone.Validate(url); err != nil {
			m.projectPickerOverlay.SetAddError(err.Error())
			return m, nil
		}
		m.closeProjectPicker()
		return m, func() tea.Msg {
			return projectAddedMsg{root: url, byURL: true, err: registerProjectURLThroughDaemon(url)}
		}
	}
	if isRemoteTarget() {
		return m.handleRemoteAddProject(path)
	}
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
	if msg.err != nil && msg.byURL && !mutationMayHaveLanded(msg.err) {
		// Nothing else on screen says the clone failed, and the user waited on it.
		return m, m.handleError(fmt.Errorf("could not add project %s: %w", msg.root, msg.err))
	}
	if msg.err != nil {
		// A registration that may have landed is re-read rather than assumed
		// either way (#4824): the Projects section shows it if the daemon
		// recorded it, and the message says the outcome is unknown instead of
		// staying silent the way a definite failure does.
		if mutationMayHaveLanded(msg.err) {
			log.WarningLog.Printf("project %s registration outcome unknown: %v", msg.root, msg.err)
			m.refreshSidebarProjects()
			return m, m.handleError(mutationOutcomeError(
				fmt.Sprintf("adding project %s", msg.root), "the projects list", msg.err))
		}
		log.WarningLog.Printf("failed to register project %s in the registry: %v", msg.root, msg.err)
		if isRemoteTarget() {
			// Nothing else happened for the user to see: the remote add does not
			// switch first, so a quiet log line would read as success.
			return m, m.handleError(fmt.Errorf("could not add project %s on the daemon: %w", msg.root, msg.err))
		}
		return m, nil
	}
	m.refreshSidebarProjects()
	return m, nil
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
	//
	// Every branch below derives the value from config, so it follows config from
	// here on (#4889): a launch --program flag scoped the launch project and is
	// replaced by the incoming project's resolution, exactly as it always was.
	m.programFollowsConfig = true
	if isRemoteTarget() {
		// The project's in-repo config is on the daemon's host. Resolving it
		// from THIS machine would read an unrelated (or absent) file, so the
		// program falls back to the global default and the hooks pane is empty
		// (a hooks save is refused in remote mode).
		if m.appConfig != nil {
			m.program = m.appConfig.DefaultProgram
		}
		m.store.SetHookCount(0)
		m.hooksPane.SetCommands(nil)
		m.hooksPane.SetReadOnly(remoteHooksReadOnlyReason())
	} else if resolved, err := config.ResolveConfigForRepo(repo); err == nil {
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

	if tasks, err := m.activeTasks(repo.Root, repo.ID); err != nil {
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

// handleRemoteAddProject registers a project on the target daemon. The path is a
// path ON THE DAEMON'S HOST, so nothing here probes it with this machine's git
// or filesystem: it is validated only for shape (absolute, or ~/…, never
// relative) and sent as typed — the daemon expands ~ against its own home and
// owns every other check. Unlike the local add there is no optimistic switch, so
// handleProjectAdded reports a refusal instead of logging it.
func (m *home) handleRemoteAddProject(path string) (tea.Model, tea.Cmd) {
	if _, err := config.ResolveDaemonHostPath(path); err != nil {
		m.projectPickerOverlay.SetAddError(err.Error())
		return m, nil
	}
	m.closeProjectPicker()
	return m, m.addProjectCmd(path)
}
