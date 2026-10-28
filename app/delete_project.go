package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
)

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
		// The delete may have archived the project's sessions and dropped its
		// registration with only the reply lost (#4824). The TUI is not
		// re-scoped on a guess; the Projects section is re-read, and the user
		// checks it before deleting again.
		if mutationOutcomeUnknown(msg.err) {
			m.refreshSidebarProjects()
			return m, m.handleError(mutationOutcomeError(
				fmt.Sprintf("deleting project '%s'", msg.name), "the projects list", msg.err))
		}
		return m, m.handleError(fmt.Errorf("failed to delete project '%s': %w", msg.name, msg.err))
	}
	if m.appConfig != nil && !isRemoteTarget() { // root_agents is this machine's config
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
		m.programFollowsConfig = true
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
