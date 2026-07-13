package app

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
)

// handleDefaultKeyPress handles key events in stateDefault (main interaction state).
func (m *home) handleDefaultKeyPress(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd) {
	switch name {
	case keys.KeyHelp:
		return m.showHelpScreen(helpTypeGeneral{}, nil)
	case keys.KeyErrorDetails:
		return m.showErrorDetails()

	// Tree navigation. Each moves the sidebar cursor and re-homes the focus
	// ring on the tree (focusTreeForNav) so the ring-reading attach verb `o`
	// resolves the freshly selected instance, not a pane left focused by a
	// prior interactive session (#1233 wrong-target-input class).
	case keys.KeyUp:
		m.sidebar.Up()
		m.focusTreeForNav()
		return m, m.selectionChanged()
	case keys.KeyDown:
		m.sidebar.Down()
		m.focusTreeForNav()
		return m, m.selectionChanged()
	case keys.KeyLeft:
		m.sidebar.CollapseSection()
		m.focusTreeForNav()
		return m, m.selectionChanged()
	case keys.KeyRight:
		m.sidebar.ExpandSection()
		m.focusTreeForNav()
		return m, m.selectionChanged()
	// `]` / `[` jump between the focusable SECTIONS — the instances tree and the
	// automations / projects rails — skipping workspace panes (#1706). Since
	// automations/projects moved out of the sidebar into their own rail regions,
	// this steps the focus ring rather than walking sidebar headers.
	case keys.KeyNextSection:
		return m, m.focusAdjacentSection(false)
	case keys.KeyPrevSection:
		return m, m.focusAdjacentSection(true)

	// Instance creation
	case keys.KeyNewRemote:
		return m.startNewInstance(true)

	case keys.KeyNew:
		return m.startNewInstance(false)

	case keys.KeyTaskList:
		// Open the task manager overlay (task creation lives on its `n` key —
		// `s` became the split verb in #1024 PR 5). The in-rail automations
		// section stays a compact summary; the manager gets a centered modal
		// so its form is never clamped into the narrow rail.
		return m.showTasksOverlay()

	// N-pane verbs (#1088/#1321): s opens the selected tab as a pane (or
	// focuses its pane); S commits an active preview alongside; x hides the
	// focused pane back to the background.
	case keys.KeyOpenPane:
		return m.handleOpenPane()
	case keys.KeySplitPane:
		return m.handleSplitPane()
	case keys.KeyHidePane:
		return m.handleHidePane()

	case keys.KeySearch:
		return m.showSearchOverlay()

	case keys.KeySwitchProject:
		return m.showProjectPickerOverlay()

	// Hooks configuration (#1024 PR 4: an overlay, not a sidebar slot)
	case keys.KeyHooks:
		return m.showHooksOverlay()

	// PR actions
	case keys.KeyOpenPR:
		return m.handleOpenPR()
	case keys.KeyCopyPR:
		return m.handleCopyPR()

	// Scrolling (each pane scrolls its own view, #1088)
	case keys.KeyShiftUp:
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollUp()
		}
		return m, m.selectionChanged()
	case keys.KeyShiftDown:
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollDown()
		}
		return m, m.selectionChanged()

	// Focus ring (#1088): Tab cycles tree → open panes (in order) →
	// automations. Tabs themselves are reached via the tree and the 1-9
	// jump keys.
	case keys.KeyTab:
		return m, m.cycleFocus(false)
	case keys.KeyShiftTab:
		return m, m.cycleFocus(true)

	// Tab lifecycle (#930 PR 4): `t` creates, `w` kills the selection's
	// active tab. Hiding a PANE is `x` — `w` keeps its kill meaning
	// everywhere (§2.3).
	case keys.KeyNewTab:
		return m.handleNewTab()
	case keys.KeyCloseTab:
		return m.handleCloseTab()

	// Instance actions
	case keys.KeyKill:
		return m.handleKill()
	case keys.KeyArchive:
		return m.handleArchive()
	case keys.KeyRestore:
		return m.handleRestore()
	case keys.KeyLimitRetry:
		return m.handleLimitRetry()
	case keys.KeyEnter:
		return m.handleEnter()
	case keys.KeyAttach:
		return m.handleAttach()
	case keys.KeyQuit:
		return m.handleQuit()

	default:
		return m, nil
	}
}

// handleKill handles the kill/delete session action. The confirmation only
// flips the row to Deleting; the slow teardown (remote delete_cmd over ssh,
// tmux kill, worktree removal) runs in killInstanceCmd's background goroutine
// so the event loop never blocks on it (#844).
func (m *home) handleKill() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is already being deleted", selected.Title))
	}

	// Capture the title at confirmation time so that background tick events
	// cannot change which instance we operate on.
	selectedTitle := selected.Title

	// Runs synchronously in the confirmation overlay's OnConfirm, on the
	// event loop — keep it fast. Raising the optimistic OpKilling op here (not in
	// the background cmd) guarantees the row is visibly deleting and kill/attach
	// are fenced off before any other event can be processed. OpKilling is a local
	// overlay (the daemon does not emit a kill op; it drops the record), so it
	// composes to Deleting for rendering and survives non-terminal reconciles.
	killAction := func() tea.Msg {
		for _, inst := range m.store.GetInstances() {
			if inst.Title == selectedTitle {
				_ = inst.Transition(session.BeginKill())
				return startKillMsg{title: selectedTitle}
			}
		}
		// The row was removed (e.g. by an external kill picked up by the
		// background refresh) while the dialog was open; nothing to do.
		return nil
	}

	// Check for uncommitted changes in the worktree (skip for backends without a
	// local worktree, e.g. remote hook sessions).
	warning := ""
	if selected.Capabilities().Workspace == session.WorkspaceLocalWorktree {
		if wt := selected.GetWorktreePath(); wt != "" {
			warning = killConfirmationWarning(wt)
		}
	}

	reserved := session.IsReservedTitle(selectedTitle)
	message := killConfirmMessage(selectedTitle, warning, reserved)
	cmd := m.confirmAction(message, killAction)
	if reserved && m.confirmationOverlay != nil {
		// Break the muscle-memory D+y reflex on the daemon-managed singleton
		// (#1238): a distinct confirm key means a reflexive 'y' — the ordinary
		// kill confirmation — is ignored, so the user has to read the warning
		// and press the named key before root is torn down.
		m.confirmationOverlay.SetConfirmKey(rootKillConfirmKey)
	}
	return m, cmd
}

// rootKillConfirmKey is the confirm key the kill dialog demands for the reserved
// root agent (#1238). Deliberately NOT the ordinary 'y' so an inattentive D+y —
// the exact gesture that silently decapitated root's event pipeline on
// 2026-07-05 — cannot dispatch the kill; the key is surfaced in the rendered
// prompt ("Press k to confirm").
const rootKillConfirmKey = "k"

// killConfirmMessage builds the kill-confirmation copy for a session. The
// reserved root agent (#1238) gets distinct, consequence-bearing copy instead of
// the generic "[!] Kill session 'root'?" that killing any throwaway worktree
// shows: killing root stops every scheduled/watch-task delivery to it (the
// inbound event pipeline) until it self-heals or the daemon is restarted. This
// mirrors the reserved-title guard the create path already applies
// (app/handle_input.go). #1237 made root self-heal ~2 min after a kill, so the
// copy names that recovery rather than the pre-#1237 "until the daemon restarts".
func killConfirmMessage(title, warning string, reserved bool) string {
	var message string
	if reserved {
		message = fmt.Sprintf(
			"[!] '%s' is the daemon-managed root agent, not a scratch session.\n"+
				"Killing it stops scheduled and watch-task delivery to '%s' until it\n"+
				"self-heals (~2 min) or you restart the daemon.\n\n"+
				"Kill the root agent anyway?", title, title)
	} else {
		message = fmt.Sprintf("[!] Kill session '%s'?", title)
	}
	if warning != "" {
		message += "\n\n" + warning
	}
	return message
}

// killInstanceCmd returns a tea.Cmd that performs the actual session teardown
// off the event loop. The daemon call blocks for the whole teardown — for
// remote instances delete_cmd often runs over ssh and takes tens of seconds —
// which is exactly why this must not run on the Update goroutine (#844). The
// teardown itself (delete_cmd → tmux kill → worktree removal, #802 ordering)
// is unchanged: it goes through the killSessionThroughDaemon seam — now the HTTP
// apiclient's KillSession (#1592 Phase 2 PR3) — which keeps the title blocked
// against reuse until the teardown completes.
func (m *home) killInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	// Capture the kill seam on the event loop, before the goroutine: it is a
	// package var swapped by test seams, so reading it inside the cmd goroutine
	// would race a sibling parallel test's swap (#960 PR 4 race-fix class).
	kill := killSessionThroughDaemon
	return func() tea.Msg {
		// The shell tab's tmux session is owned by the instance and torn down by
		// LocalBackend.Kill (looping all tabs) inside the daemon teardown — there
		// is no longer a UI-side terminal cache to clean up (#930 PR 2).
		if err := kill(title, repoID); err != nil {
			log.ErrorLog.Printf("could not kill instance: %v", err)
			return instanceKilledMsg{title: title, err: err}
		}
		return instanceKilledMsg{title: title}
	}
}

// handleInstanceKilled finalizes an async kill on the event loop. On success
// the row is removed from the sidebar (the daemon already deleted the disk
// record). On failure the row flips back to Ready so the user can retry the
// kill, and the underlying error — the evidence, per #797 — lands in the
// error box.
func (m *home) handleInstanceKilled(msg instanceKilledMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		for _, inst := range m.store.GetInstances() {
			if inst.Title == msg.title && inst.GetInFlightOp() == session.OpKilling {
				// Clear the optimistic kill op: the row reverts to its underlying
				// daemon liveness so the user can retry the kill.
				_ = inst.Transition(session.RevertKill())
				break
			}
		}
		return m, m.handleError(fmt.Errorf("failed to kill session '%s': %w", msg.title, msg.err))
	}

	// The TUI's in-memory instance is untouched by the daemon-side teardown,
	// so its repo name is still resolvable here for repo-section bookkeeping.
	// The row may already be gone when the background refresh noticed the
	// deleted disk record first — both removals are no-ops then.
	var repoName string
	var repoErr error = fmt.Errorf("instance not found")
	for _, inst := range m.store.GetInstances() {
		if inst.Title == msg.title {
			repoName, repoErr = inst.RepoName()
			break
		}
	}
	if repoErr == nil {
		m.store.RemoveInstanceByTitleWithRepo(msg.title, repoName)
	} else {
		m.store.RemoveInstanceByTitle(msg.title)
	}
	return m, m.selectionChanged()
}

// handleArchive is the archive verb (`a`): on a LIVE row it archives behind a
// confirmation, since archive tears down tmux and relocates the worktree. On an
// Archived/Lost/Dead row there is nothing to archive, so `a` is a no-op — the
// dedicated `r` restore key (handleRestore) owns that transition (#1605). Remote
// and in-place sessions can't be archived (no relocatable worktree) and are
// rejected up front with an immediate message; the daemon enforces the same
// rules authoritatively.
func (m *home) handleArchive() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	title := selected.Title

	// A resting (Archived/Lost/Dead) row has no live worktree/tmux to tear down;
	// `a` does nothing. Restore is on its own key now (`r`).
	if selected.GetLiveness() == session.LiveArchived ||
		selected.GetLiveness() == session.LiveLost ||
		selected.GetLiveness() == session.LiveDead {
		return m, nil
	}

	// Live row → archive. Fail fast on the unarchivable session kinds for a
	// snappy message (the daemon rejects them too).
	if !selected.Capabilities().Archive {
		return m, m.handleError(fmt.Errorf("cannot archive remote session '%s': it has no local worktree to relocate", title))
	}
	if selected.IsExternalWorktree() {
		return m, m.handleError(fmt.Errorf("cannot archive in-place session '%s': archive relocates the worktree, which it doesn't own", title))
	}

	restoreKey := keys.GlobalKeyBindings[keys.KeyRestore].Help().Key
	message := fmt.Sprintf("[!] Archive session '%s'?\n\nIts tmux is torn down and its worktree is moved out to the archive directory (branch + uncommitted changes preserved). Restore later with %s (or `af sessions restore`).", title, restoreKey)
	return m, m.confirmAction(message, func() tea.Msg {
		// Raise the optimistic OpArchiving op so the row visibly shows archiving
		// while the RPC runs (#1195). It composes to Deleting for rendering; the
		// reconcile clears it once the daemon liveness settles on Archived, and the
		// completion handler finalizes it — so the row can never strand (#1187).
		for _, inst := range m.store.GetInstances() {
			if inst.Title == title {
				_ = inst.Transition(session.BeginArchive())
				break
			}
		}
		return startArchiveMsg{title: title}
	})
}

// handleRestore is the restore verb (`r`, #1605): on an Archived/Lost/Dead row it
// restores the session (non-destructive — no confirm). On a live row there is
// nothing to restore, so `r` is a no-op — archive stays on `a` (handleArchive).
//
// No confirmation: restore only moves the worktree back and re-spawns the agent.
// Raise the optimistic OpRestoring op (mirroring how archive raises OpArchiving):
// it re-homes the row into the live Instances section AT ONCE via ShownArchived
// (#1210) and fences a double-restore, while leaving liveness LiveArchived so the
// snapshot reconcile still observes the Archived→live transition and runs its
// rebuild/re-Start (#1203). The reconcile rebuild clears the op by replacing the
// row; a restore failure clears it in handleInstanceRestored.
func (m *home) handleRestore() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	title := selected.Title

	// Only a resting (Archived/Lost/Dead) row can be restored; on a live row `r`
	// does nothing (archive is on `a`).
	if selected.GetLiveness() != session.LiveArchived &&
		selected.GetLiveness() != session.LiveLost &&
		selected.GetLiveness() != session.LiveDead {
		return m, nil
	}
	if selected.GetInFlightOp() == session.OpRestoring {
		return m, m.handleError(fmt.Errorf("session '%s' is already being restored", title))
	}
	_ = selected.Transition(session.MarkRestoring())
	return m, m.restoreInstanceCmd(title)
}

// archiveInstanceCmd runs the daemon archive (tmux teardown + worktree move) off
// the event loop (#1028), mirroring killInstanceCmd — the RPC blocks for the
// whole operation, so it must not run on the Update goroutine.
func (m *home) archiveInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	archive := archiveSessionThroughDaemon
	return func() tea.Msg {
		if _, err := archive(title, repoID); err != nil {
			log.ErrorLog.Printf("could not archive instance %q: %v", title, err)
			return instanceArchivedMsg{title: title, err: err}
		}
		return instanceArchivedMsg{title: title}
	}
}

// restoreInstanceCmd runs the daemon restore/recovery off the event loop.
func (m *home) restoreInstanceCmd(title string) tea.Cmd {
	repoID := m.repoID
	restore := restoreSessionThroughDaemon
	return func() tea.Msg {
		if _, err := restore(title, repoID); err != nil {
			log.ErrorLog.Printf("could not restore instance %q: %v", title, err)
			return instanceRestoredMsg{title: title, err: err}
		}
		return instanceRestoredMsg{title: title}
	}
}

// handleInstanceArchived finalizes an async archive. On success the RPC has
// already returned, so the daemon has committed the session to Archived; mark
// the local row Archived IMMEDIATELY (mirroring how handleInstanceKilled
// finalizes the row) so it partitions into the Archived folder without waiting
// for — or depending on — the next snapshot poll. This is belt-and-suspenders
// alongside the reconcile override (#1028): together they guarantee the row can
// never strand on "Tearing down session…" even if a poll caught it mid-fence.
// On failure the error lands in the error box.
func (m *home) handleInstanceArchived(msg instanceArchivedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// Archive failed: clear the optimistic op so the row reverts to its
		// underlying daemon liveness rather than stranding as archiving.
		for _, inst := range m.store.GetInstances() {
			if inst.Title == msg.title && inst.GetInFlightOp() == session.OpArchiving {
				_ = inst.Transition(session.ClearOp())
				break
			}
		}
		return m, m.handleError(fmt.Errorf("failed to archive session '%s': %w", msg.title, msg.err))
	}
	for _, inst := range m.store.GetInstances() {
		if inst.Title == msg.title {
			// SetArchived is an unconditional projection-mirror: it copies the
			// daemon's already-committed Archived state (started=false,
			// liveness=Archived, op cleared) onto the read-only row. It is NOT the
			// fenced CommitArchive transition (that's the daemon's I2 enforcement
			// point) — the TUI row may be in any state here (optimistic OpArchiving,
			// or already Archived if the reconcile settled first), so it stays a
			// direct unconditional mirror rather than a fenced edge (#1195 Phase 2d).
			inst.SetArchived()
			break
		}
	}
	return m, m.selectionChanged()
}

// limitRetriedMsg reports completion of an async usage-limit manual retry
// (#1146, `c`). On success the daemon cleared the limit and re-delivered the
// prompt; a non-nil err is surfaced in the error box.
type limitRetriedMsg struct {
	title string
	err   error
}

// handleLimitRetry is the usage-limit manual-retry verb (#1146, `c`): on a
// session blocked at a usage-limit wall it asks the daemon to re-spawn (if the
// agent exited) and re-deliver the pending prompt, un-stalling the work. It is a
// no-op with an explanatory message on any non-limit row. The daemon RPC re-
// delivers a prompt (SendPromptCommand sleeps to let control sequences drain, and
// a respawn can take a beat), so it runs OFF the event loop like the kill/archive
// cmds rather than freezing the TUI.
func (m *home) handleLimitRetry() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	if !selected.LimitReached() {
		return m, m.handleError(fmt.Errorf("session '%s' is not blocked on a usage limit", selected.Title))
	}
	return m, m.resumeFromLimitCmd(selected.Title)
}

// resumeFromLimitCmd runs the daemon resume (re-spawn if needed + re-deliver the
// prompt + clear the limit state) off the event loop (#1146), mirroring
// restoreInstanceCmd.
func (m *home) resumeFromLimitCmd(title string) tea.Cmd {
	repoID := m.repoID
	resume := resumeFromLimitThroughDaemon
	return func() tea.Msg {
		if err := resume(title, repoID); err != nil {
			log.ErrorLog.Printf("could not resume limited session %q: %v", title, err)
			return limitRetriedMsg{title: title, err: err}
		}
		return limitRetriedMsg{title: title}
	}
}

// handleLimitRetried finalizes an async usage-limit retry. On success the daemon
// has already cleared the limit + set Running and persisted; clear the local row
// optimistically for instant feedback (the badge disappears without waiting for
// the next snapshot reconcile). On failure the error lands in the error box.
func (m *home) handleLimitRetried(msg limitRetriedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("failed to resume session '%s': %w", msg.title, msg.err))
	}
	for _, inst := range m.store.GetInstances() {
		if inst.Title == msg.title {
			inst.ClearLimitReached()
			break
		}
	}
	return m, nil
}

// handleInstanceRestored finalizes an async restore. The row already re-homed
// into the live Instances section the moment the restore was dispatched (the
// OpRestoring overlay handleArchive raised — #1210), so there is no stale
// Archived frame to wait out here.
//
// On SUCCESS we deliberately do NOT flip liveness: the daemon has committed the
// session live, and the next snapshot reports it live, at which point the
// reconcile's Archived→live REBUILD re-Starts the row (restoring started + the
// agent-tmux binding, #1203) and clears the OpRestoring overlay by replacing the
// row. Flipping liveness to live here would make that reconcile see live→live,
// SKIP the rebuild, and strand the row "live but not started" — reintroducing the
// exact #1203 regression (unattachable restored session). So the visual re-home
// is eager, but the liveness transition — and thus the rebuild — stays owned by
// the reconcile path.
//
// On FAILURE (e.g. the origin repo is gone) clear the OpRestoring overlay so the
// row drops back into the Archived section — its worktree is still shelved — and
// surface the error.
func (m *home) handleInstanceRestored(msg instanceRestoredMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		for _, inst := range m.store.GetInstances() {
			if inst.Title == msg.title && inst.GetInFlightOp() == session.OpRestoring {
				_ = inst.Transition(session.ClearOp())
				break
			}
		}
		return m, m.handleError(fmt.Errorf("failed to restore session '%s': %w", msg.title, msg.err))
	}
	for _, inst := range m.store.GetInstances() {
		switch {
		case inst.Title != msg.title:
			continue
		case inst.GetLiveness() == session.LiveLost || inst.GetLiveness() == session.LiveDead:
			_ = inst.Transition(session.ConfirmLive())
		}
		break
	}
	return m, m.selectionChanged()
}

// killConfirmationWarning returns the data-loss warning line for the kill
// confirmation dialog, or "" if the worktree at wt is verifiably clean. Kill
// tears the worktree down with `git worktree remove -f`, which bypasses git's
// own refusal to delete a dirty worktree, so this check is the only warning
// the user gets. If `git status` itself fails we cannot prove the worktree is
// clean — fail closed and warn that changes may be lost rather than silently
// skipping the warning (#815).
func killConfirmationWarning(wt string) string {
	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		log.WarningLog.Printf("could not verify worktree status for %s before kill: %v", wt, err)
		return fmt.Sprintf("WARNING: Could not verify worktree status (%v); it may contain uncommitted changes that will be lost!", err)
	}
	if len(strings.TrimSpace(string(out))) > 0 {
		return "WARNING: This worktree has uncommitted changes that will be lost!"
	}
	return ""
}

// handleEnter is the Enter verb (#1089 PR 2, RFC §2.3): enter INTERACTIVE
// mode on the pane — every keystroke forwards to the agent/shell in place,
// no full-screen takeover, Ctrl-] returns to nav.
//
// Enter resolves its target from context: a focused workspace pane owns Enter,
// otherwise the SIDEBAR SELECTION owns Enter. The tree/nav path keeps the
// #1233/#1236 stale-target fix because tree navigation re-homes the focus ring
// on the tree before Enter is pressed; in that context Enter opens (or focuses)
// the selected instance's pane, exactly like `s`, then enters it. But when the
// ring is already on a pane, re-reading GetSelectedInstance would silently jump
// input to whichever row the sidebar currently highlights (#1253).
//
// Bindings that cannot embed — remote instances, whose only local terminal is
// the full-screen hook PTY — fall back to the full-screen attach Enter used to
// do; dead/transitional sessions keep their guard errors.
func (m *home) handleEnter() (tea.Model, tea.Cmd) {
	// Ignore Enter entirely while a full-screen attach is starting or live
	// (#1530): the transition hands stdout/stdin to tmux, so any Enter that
	// arrives in that window must not kick off a second attach flow (or any
	// other pane action). beginAttachTransition guards the attach funnel too;
	// this stops the re-entry one step earlier, at the key handler. Both flags
	// clear on detach.
	if m.attachTransitioning || m.attached.Load() {
		return m, nil
	}
	if m.panePreviewTxn != nil {
		// Committing a sidebar tab-row preview is a tree-navigation action (the
		// preview only exists while the tree cursor sits on a tab row), so this
		// Enter selects/commits — it must NOT type into the agent (nil replay),
		// exactly like the tree select path below (#1576).
		//
		// enterPane routes the committed target correctly for either kind: an
		// embeddable tab enters interactive mode in place, while a remote /
		// non-embeddable tab (liveSessionName == "") falls back to
		// handleEnterPane's full-screen attach. Don't short-circuit the remote
		// case here — that skipped the attach and forced a second Enter/`o`
		// (#1601); letting enterPane run makes the first Enter attach, matching
		// the focused-pane and tree paths.
		p, commitCmd := m.commitPanePreviewReplace()
		if p == nil {
			return m, commitCmd
		}
		mod, interactCmd := m.enterPane(p, nil)
		return mod, tea.Batch(commitCmd, interactCmd)
	}
	if p := m.focusedOpenPane(); p != nil {
		// Enter on an already-focused pane: from the pane's point of view this
		// is the first in-pane keystroke, so forward it into the agent on entry
		// rather than swallowing it (#1576). Only this branch replays — the
		// preview-commit above and the tree select below are navigation Enters.
		enterKey := tea.KeyMsg{Type: tea.KeyEnter}
		return m.enterPane(p, &enterKey)
	}

	sel := m.sidebar.GetSelection()

	// Toggle expandable section headers (Instances and the Archived folder). The
	// bottom Projects section is a focus-ring peer, not a tree row, so its Enter
	// is handled in handleProjectsFocus, not here (#1588 follow-up).
	if sel.IsHeader && (sel.Kind == ui.SectionInstances || sel.Kind == ui.SectionArchived) {
		m.sidebar.ToggleSection()
		return m, m.selectionChanged()
	}
	// Only instance/tab rows enter a pane; anything else (e.g. an automations
	// row) is a no-op, as before.
	if sel.Kind != ui.SectionInstances && sel.Kind != ui.SectionArchived {
		return m, nil
	}
	// Instance selected — in either the Instances tree or the Archived folder
	// (#1028). An archived row is not embeddable: interactiveGuard surfaces the
	// "restore it first" error before any pane/attach path is reached.
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if err := interactiveGuard(selected); err != nil {
		return m, m.handleError(err)
	}
	if liveSessionName(selected, m.store.ActiveTab()) == "" {
		// Not embeddable (remote): the old full-screen attach flow.
		return m.attachSelected(selected)
	}
	// Open (or focus) the selection's pane — the `s`/open semantics — then
	// enter it. The pane pointer is captured here, at Enter-press time (#716),
	// for the deferred activation.
	_, cmd := m.openOrFocusPane(selected, m.store.ActiveTab())
	p := m.store.FindOpenPane(selected, m.store.ActiveTab())
	if p == nil {
		return m, cmd
	}
	mod, interactCmd := m.requestInteractive(p, nil)
	return mod, tea.Batch(cmd, interactCmd)
}

// interactiveGuard returns the user-facing error that fences Enter off a
// session in a state it cannot be entered or attached in — shared by the
// interactive and full-screen paths (the #935 dead-tmux error, the Deleting
// fence, the #1108 Lost fence). A nil error does NOT mean embeddable (see
// liveSessionName); nil instance and Loading are the caller's silent no-op
// cases.
func interactiveGuard(inst *session.Instance) error {
	if inst == nil || inst.IsCreating() {
		return nil
	}
	if inst.IsTearingDown() {
		return fmt.Errorf("session '%s' is being deleted", inst.Title)
	}
	if inst.GetInFlightOp() == session.OpRestoring {
		// Restore in flight (#1210/#1300): archived rows are re-homed into
		// Instances while the daemon moves/spawns, and Lost/Dead rows keep their
		// unavailable liveness until recovery completes.
		return fmt.Errorf("session '%s' is being restored", inst.Title)
	}
	if inst.GetLiveness() == session.LiveLost {
		// Lost (#1108): the backing tmux session vanished with no kill on
		// record. Entering or attaching is impossible right now; say what
		// happened — same explicit-feedback contract as the Deleting path
		// (#935). Checked before TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' was lost — restore it first (af sessions restore %s)", inst.Title, inst.Title)
	}
	if inst.GetLiveness() == session.LiveDead {
		return fmt.Errorf("session '%s' is no longer running — restore it first (af sessions restore %s)", inst.Title, inst.Title)
	}
	if inst.GetLiveness() == session.LiveArchived {
		// Archived (#1028): the user tore the session down and its worktree was
		// moved to the global archive dir; there is no tmux to enter or attach.
		// Point at the off-ramp (restore) rather than a bare "not running" —
		// same explicit-feedback contract as Lost/Deleting. Checked before
		// TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' is archived — restore it first (af sessions restore %s)", inst.Title, inst.Title)
	}
	if !inst.TmuxAlive() {
		return fmt.Errorf("session '%s' is no longer running", inst.Title)
	}
	return nil
}

// handleAttach is the full-screen attach verb (`o`; the pre-#1089-PR-2
// Enter): attach the FOCUSED pane's (instance, tab) full-screen (#1088), or
// the tree selection's. The attach path itself is untouched: the same
// attachOverlayCallbackFn seam, `attached` gate, and SIGKILL-bounded detach.
func (m *home) handleAttach() (tea.Model, tea.Cmd) {
	if p := m.focusedOpenPane(); p != nil {
		return m.handleEnterPane(p)
	}
	sel := m.sidebar.GetSelection()
	// Accept both the Instances tree and the Archived folder (#1028): an
	// archived row is non-attachable, and interactiveGuard surfaces the
	// "restore it first" error rather than a silent no-op.
	if sel.IsHeader || (sel.Kind != ui.SectionInstances && sel.Kind != ui.SectionArchived) {
		return m, nil
	}
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil || selected.IsCreating() {
		return m, nil
	}
	if err := interactiveGuard(selected); err != nil {
		return m, m.handleError(err)
	}
	return m.attachSelected(selected)
}

// attachSelected runs the tree-selection full-screen attach flow for a
// guarded (non-nil, non-Loading, non-Deleting, tmux-alive) instance.
//
// The instance is captured at keypress time — the synchronous moment the
// selection is provably current. For first-time attachers the attach is
// deferred until the help overlay is dismissed, and a background refresh can
// drift the selection onto a different instance in the meantime; the
// callbacks must attach to this captured instance, not re-read the live
// selection (#716).
func (m *home) attachSelected(selected *session.Instance) (tea.Model, tea.Cmd) {
	return m.attachInstanceTab(selected, m.store.ActiveTab(), "handleEnter-sidebar", "handleEnter-terminal")
}

// attachInstanceTab runs the full-screen attach flow for a captured
// instance+tab binding. The terminal tab attaches a local tmux session for
// local instances, but a remote instance's terminal_cmd PTY for remote ones
// (#843), so the post-detach handling must key off the instance's real
// remote-ness (#889). Callers pass the tab index captured at keypress time
// (#716), before help-overlay deferral can let selection or active tab drift.
func (m *home) attachInstanceTab(instance *session.Instance, tabIdx int, agentLabel, terminalLabel string) (tea.Model, tea.Cmd) {
	remote := instance.Capabilities().Workspace == session.WorkspaceRemote
	label := agentLabel
	if tabIdx != 0 {
		label = terminalLabel
	}
	// Pick the attach byte source by LOCALITY — a capability check, not a
	// concrete-backend type assertion (#1592 Phase 1). A local session (agent tab
	// 0 or a shell/process tab) attaches CLIENT-side as a WS PTY subscriber over
	// the daemon socket (apiclient.AttachStream, #1592 Phase 2 PR7), replacing the
	// retired tmux-server-mediated attach driver. A remote session attaches its
	// hook attach_cmd (agent tab) / terminal_cmd (terminal tab) PTY in-process
	// through the uniform Backend interface. Both return a chan closed on detach,
	// and both now scribble the terminal (see attachOverlayCallback's uniform
	// reassert). instance + repoID are captured here at keypress so a deferred
	// attach (help overlay open) targets the captured session, not a drifted
	// selection (#716).
	repoID := m.repoID
	attach := func() (chan struct{}, error) {
		if remote {
			if tabIdx != 0 {
				return ui.AttachTerminalTab(instance, tabIdx)
			}
			return m.store.AttachInstance(instance)
		}
		c, err := apiclient.NewTargeted()
		if err != nil {
			return nil, err
		}
		return c.AttachStream(context.Background(), instance.Title, repoID, tabIdx)
	}
	return m.showHelpScreen(helpTypeInstanceAttach{}, func() tea.Cmd {
		return m.beginAttachTransition(func() tea.Cmd {
			return attachOverlayCallbackFn(m, instance.Title, label, "", attach)
		})
	})
}

// handleNewTab spawns a new shell tab in the selected instance and selects it
// (#930 PR 4). Single keypress, no prompt: the tab runs $SHELL in the instance's
// worktree. Remote instances have no local worktree and the hook protocol has no
// run-arbitrary-command verb, so new-tab is unsupported there: a remote session's
// only terminal tab is the one derived from remote_hooks.terminal_cmd (#930 PR 6).
//
// The spawn+persist is routed through the daemon's CreateTab RPC (#960): the
// daemon — the single writer — owns the new tab so its authoritative view holds
// it and the TUI no longer originates a tab write at all (#959). The TUI reflects
// the daemon-created tab locally via AttachShellTab for instant display (it
// reconnects to the session the daemon spawned, never a second colliding spawn);
// the snapshot reconcile (PR 3) keeps it mirrored thereafter. The daemon's soft
// cap (max tabs) error is surfaced verbatim.
func (m *home) handleNewTab() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.HasInFlightOp() {
		return m, nil
	}
	if !selected.Capabilities().TabManagement {
		return m, m.handleError(fmt.Errorf("remote sessions don't support new process tabs; their terminal tab comes from remote_hooks.terminal_cmd and arbitrary remote processes aren't supported"))
	}

	name, err := createShellTabThroughDaemon(selected.Title, m.repoID)
	if err != nil {
		return m, m.handleError(err)
	}
	// The daemon spawned and persisted the tab; reflect it locally for instant
	// display without a second spawn. The daemon write is authoritative, so the
	// TUI never saves (#960 PR 4).
	if _, attachErr := selected.AttachShellTab(name); attachErr != nil {
		return m, m.handleError(attachErr)
	}

	// Select the fresh tab in the tree and open it as a pane (#1088): the
	// pre-N-pane behavior showed the new tab in the workspace immediately,
	// and the issue's canonical flow — agent pane + terminal pane side by
	// side for one instance — is exactly `s` then `t`.
	newIdx := len(tree.TabLabels(selected)) - 1
	m.store.SetActiveTab(newIdx)
	m.menu.SetActiveTab(newIdx)
	m.sidebar.SyncCursorToActiveTab()
	return m.openOrFocusPane(selected, newIdx)
}

// handleCloseTab closes the active tab of the selected instance and selects the
// previous (left) tab (#930 PR 4). The agent tab (index 0) is unclosable — w on
// it is a gentle no-op message pointing at D for killing the whole session. A
// remote instance's tabs (agent + optional terminal_cmd terminal) are fixed by
// its hook config, not user-managed, so closing any of them is refused.
//
// The kill+persist is routed through the daemon's CloseTab RPC (#960): the
// daemon — the single writer — kills the tab's tmux and persists the shrunk
// list, so the TUI no longer originates a tab write at all (#959). The agent-tab
// and remote rules are still enforced TUI-side so the friendly message shows
// without a round-trip (the RPC enforces them too). The TUI drops the now-dead
// tab locally via DropClosedTab — a no-kill removal, since the daemon already
// tore the tmux session down.
func (m *home) handleCloseTab() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	idx := m.store.ActiveTab()
	if idx <= 0 {
		return m, m.handleError(fmt.Errorf("the agent tab can't be closed; use D to kill the session"))
	}
	if !selected.Capabilities().TabManagement {
		return m, m.handleError(fmt.Errorf("remote session tabs can't be closed"))
	}
	tabs := selected.GetTabs()
	if idx >= len(tabs) {
		return m, m.handleError(fmt.Errorf("tab cannot be closed"))
	}
	tabName := tabs[idx].Name
	// Capture the slot→name list before the drop: reconcilePanesForTabs maps
	// the open panes' bindings across the change by tab name (#1088).
	oldNames := paneTabNames(selected)

	if err := closeTabThroughDaemon(selected.Title, m.repoID, tabName); err != nil {
		return m, m.handleError(err)
	}
	// The daemon killed the tmux and persisted the shrunk list; drop the
	// now-dead tab locally without re-killing. The daemon write is
	// authoritative, so the TUI never saves (#960 PR 4).
	if dropErr := selected.DropClosedTab(idx); dropErr != nil {
		return m, m.handleError(dropErr)
	}

	// The kill shifts every higher tab slot down by one, so the open panes
	// bound to this instance must follow (#1088): the killed tab's pane
	// leaves the workspace, higher-slot panes re-bind so they keep showing
	// the same tab. Shared with the daemon-snapshot reconcile, which applies
	// the identical semantics when a tab disappears out-of-band.
	if m.reconcilePanesForTabs(selected, oldNames) {
		m.relayout()
	}

	// Prefer the left/previous neighbor (idx >= 1, so idx-1 >= 0).
	m.store.SetActiveTab(idx - 1)
	m.clampSelectionTab()
	m.menu.SetActiveTab(m.store.ActiveTab())
	m.sidebar.SyncCursorToActiveTab()
	return m, m.selectionChanged()
}

// handleTabJump jumps to a 1-based tab number (the 1-9 number keys). With a
// pane focused, the pane's own binding changes tab; with tree focus, the
// sidebar selection's active tab changes. Out-of-range numbers are a no-op
// (#930 PR 4). When the sidebar cursor rests on one of the selected instance's
// tab rows, it follows the tree-focus jump so the tree and active tab agree.
func (m *home) handleTabJump(oneBased int) (tea.Model, tea.Cmd) {
	idx := oneBased - 1
	if p := m.focusedOpenPane(); p != nil {
		w := m.paneWindows[p.ID()]
		if m.panePreviewTxn != nil && m.panePreviewTxn.ownerPaneID == p.ID() {
			m.cancelPanePreview(false)
		}
		if w == nil || !w.JumpToTab(idx) {
			return m, nil
		}
		// The pane's tab changed, so its live binding key changed — rebind it.
		m.syncLiveTermPane()
		return m, m.selectionChanged()
	}
	if idx < 0 || idx >= len(tree.TabLabels(m.store.GetSelectedInstance())) {
		return m, nil
	}
	m.store.SetActiveTab(idx)
	m.menu.SetActiveTab(idx)
	m.sidebar.SyncCursorToActiveTab()
	return m, m.selectionChanged()
}

// noPRForSessionErr is the actionable message surfaced when p/P is pressed on a
// session that has no PR yet, so the key press is never a silent no-op (#1170).
var noPRForSessionErr = fmt.Errorf("no PR for this session yet — push a branch / open a PR first")

// handleOpenPR opens the PR URL in the browser.
func (m *home) handleOpenPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.GetPRInfo() == nil {
		return m, m.handleError(noPRForSessionErr)
	}
	url := selected.GetPRInfo().URL
	var openCmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		openCmd = exec.Command("open", url)
	} else {
		openCmd = exec.Command("xdg-open", url)
	}
	if err := openCmd.Start(); err != nil {
		return m, m.handleError(fmt.Errorf("failed to open PR: %w", err))
	}
	// Reap the opener when it exits so it doesn't linger as a zombie for the
	// life of the TUI (#816).
	go func() {
		_ = openCmd.Wait()
	}()
	return m, nil
}

// handleCopyPR copies the PR URL to the clipboard.
func (m *home) handleCopyPR() (tea.Model, tea.Cmd) {
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, nil
	}
	if selected.GetPRInfo() == nil {
		return m, m.handleError(noPRForSessionErr)
	}
	url := selected.GetPRInfo().URL
	if err := copyToClipboard(url); err != nil {
		return m, m.handleError(fmt.Errorf("%w; PR URL: %s", err, url))
	}
	return m, nil
}
