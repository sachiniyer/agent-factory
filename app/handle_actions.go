package app

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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
	case keys.KeyConfigAgent:
		return m.handleConfigAgent()

	// Global config editor (",")
	case keys.KeyConfigEditor:
		return m.showConfigEditor()

	// PR actions
	case keys.KeyOpenPR:
		return m.handleOpenPR()
	case keys.KeyCopyPR:
		return m.handleCopyPR()

	// Scrolling (each pane scrolls its own view, #1088)
	case keys.KeyShiftUp:
		m.syncPaneScrollOwners()
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollUp()
		}
		return m, m.selectionChanged()
	case keys.KeyShiftDown:
		m.syncPaneScrollOwners()
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

	// Tab lifecycle (#930 PR 4): `t` chooses and creates, `w` kills the selection's
	// active tab. Hiding a PANE is `x` — `w` keeps its kill meaning
	// everywhere (§2.3).
	case keys.KeyNewTab:
		return m.showNewTabPicker()
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
	case keys.KeyHandoff:
		return m.handleHandoff()
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
	if selected == nil || !selected.CanKill() {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is already being deleted", selected.Title))
	}

	// Capture stable identity at confirmation-open time. A title may be reused
	// while the modal is open, so every later phase resolves this target instead
	// of whichever current row happens to carry the display title (#2358).
	selectedTitle := selected.Title
	target := captureSessionActionTarget(selected, m.repoID)

	// Runs synchronously in the confirmation overlay's OnConfirm, on the
	// event loop — keep it fast. Raising the optimistic OpKilling op here (not in
	// the background cmd) guarantees the row is visibly deleting and kill/attach
	// are fenced off before any other event can be processed. OpKilling is a local
	// overlay (the daemon does not emit a kill op; it drops the record), so it
	// composes to Deleting for rendering and survives non-terminal reconciles.
	killAction := func() tea.Msg {
		inst := m.resolveSessionActionTarget(target)
		if inst != nil {
			_ = inst.Transition(session.BeginKill())
			return startKillMsg{target: target}
		}
		// The row was removed (e.g. by an external kill picked up by the
		// background refresh) while the dialog was open; nothing to do.
		return nil
	}

	// Assess what killing this session would destroy. Two independent losses,
	// each with its own copy: a dirty worktree (uncommitted changes, #815) and
	// local-only commits (committed-but-unmerged-and-unpushed work, #2022). The
	// second is unrecoverable — kill force-deletes the branch with `git branch
	// -D` — so it escalates the confirmation to the critical-content guarantee
	// (#1973) AND a distinct confirm key, exactly as the reserved-root kill
	// (#1238) does. A session can be both dirty and carry unmerged commits, so
	// the warnings accumulate rather than replace one another. Both checks skip
	// for backends without a local worktree (e.g. remote hook sessions).
	var warnings []string
	severeLine := ""
	if selected.Capabilities().Workspace == session.WorkspaceLocalWorktree {
		// Both loss checks run under the SAME worktree-path gate so they cover the
		// same session states. GetWorktreePath, GetWorktreeBranch, and
		// GetBaseCommitSHA are intentionally not gated on started (unlike
		// GetGitWorktree), so a session that HAS a
		// worktree but was never started — e.g. a restore-failed session — still gets
		// the unmerged-work warning the old GetGitWorktree gate skipped for it
		// (#2029). The cleanup-impact snapshot is the authority for ownership:
		// external worktrees survive kill, and a reused user branch survives even
		// when its linked worktree does not.
		impact, hasWorktree := selected.GetWorktreeCleanupImpact()
		if hasWorktree && impact.RemoveWorktree && impact.Path != "" {
			wt := impact.Path
			if w := killConfirmationWarning(wt); w != "" {
				warnings = append(warnings, w)
			}
			prState := ""
			if pr := selected.GetPRInfo(); pr != nil && pr.Branch == impact.Branch {
				prState = pr.State
			}
			if line, severe := unmergedCommitWarning(wt, impact.Branch, impact.BaseCommitSHA, prState, impact.DeleteBranch); line != "" {
				if severe {
					severeLine = line
				} else {
					// Fail-closed (unverifiable): warn, but do not force the extra
					// keystroke — we have not established that work is being lost.
					warnings = append(warnings, line)
				}
			}
		}
	}

	reserved := session.IsReservedTitle(selectedTitle)

	if severeLine != "" {
		// The severe consequence and any dirty-worktree warning are the critical
		// content the user is consenting to; they must render in full or the
		// overlay refuses the confirm (#1973). Only the recovery hint — genuine
		// elaboration — goes in the clippable detail.
		critical := killConfirmMessage(selectedTitle, joinWarnings(append([]string{severeLine}, warnings...)), reserved)
		archiveKey := keys.GlobalKeyBindings[keys.KeyArchive].Help().Key
		detail := fmt.Sprintf("Archive (%s) preserves the worktree and its refs — kill removes the worktree for good.", archiveKey)
		cmd := m.confirmActionWithDetail(critical, detail, killAction)
		if m.confirmationOverlay != nil {
			m.confirmationOverlay.SetConfirmKey(unmergedKillConfirmKey)
		}
		return m, cmd
	}

	message := killConfirmMessage(selectedTitle, joinWarnings(warnings), reserved)
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

// killInstanceCmd returns a tea.Cmd that performs the actual session teardown
// off the event loop. The daemon call blocks for the whole teardown — for
// remote instances delete_cmd often runs over ssh and takes tens of seconds —
// which is exactly why this must not run on the Update goroutine (#844). The
// teardown itself (delete_cmd → tmux kill → worktree removal, #802 ordering)
// is unchanged: it goes through the killSessionThroughDaemon seam — now the HTTP
// apiclient's KillSession (#1592 Phase 2 PR3) — which keeps the title blocked
// against reuse until the teardown completes.
func (m *home) killInstanceCmd(target sessionActionTarget) tea.Cmd {
	// Capture the kill seam on the event loop, before the goroutine: it is a
	// package var swapped by test seams, so reading it inside the cmd goroutine
	// would race a sibling parallel test's swap (#960 PR 4 race-fix class).
	kill := killSessionThroughDaemon
	return func() tea.Msg {
		// The shell tab's tmux session is owned by the instance and torn down by
		// LocalBackend.Kill (looping all tabs) inside the daemon teardown — there
		// is no longer a UI-side terminal cache to clean up (#930 PR 2).
		if err := kill(target.killRequest()); err != nil {
			log.ErrorLog.Printf("could not kill instance: %v", err)
			return instanceKilledMsg{target: target, err: err}
		}
		return instanceKilledMsg{target: target}
	}
}

// handleInstanceKilled finalizes an async kill on the event loop. On success
// the row is removed from the sidebar (the daemon already deleted the disk
// record). On failure the row flips back to Ready so the user can retry the
// kill, and the underlying error — the evidence, per #797 — lands in the
// error box.
func (m *home) handleInstanceKilled(msg instanceKilledMsg) (tea.Model, tea.Cmd) {
	inst := m.resolveSessionActionTarget(msg.target)
	if msg.err != nil {
		if inst != nil && inst.GetInFlightOp() == session.OpKilling {
			// Clear the optimistic kill op: the row reverts to its underlying
			// daemon liveness so the user can retry the kill.
			_ = inst.Transition(session.RevertKill())
		}
		return m, m.handleError(fmt.Errorf("failed to kill session '%s': %w", msg.target.title, msg.err))
	}

	// A refresh may already have removed the row, or replaced it with a new
	// same-title session while the RPC was in flight. Remove only the captured
	// identity; both missing cases are intentional no-ops.
	if inst != nil {
		m.store.RemoveInstance(inst)
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
	if selected == nil {
		return m, nil
	}
	lifecycleAction := selected.LifecycleAction()
	if lifecycleAction == session.LifecycleActionNone {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	title := selected.Title
	target := captureSessionActionTarget(selected, m.repoID)

	// A resting (Archived/Lost/Dead) row has no live worktree/tmux to tear down;
	// `a` does nothing. Restore is on its own key now (`r`).
	if lifecycleAction != session.LifecycleActionArchive {
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
		inst := m.resolveSessionActionTarget(target)
		if inst == nil {
			return nil
		}
		_ = inst.Transition(session.BeginArchive())
		return startArchiveMsg{target: target}
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
	if selected == nil {
		return m, nil
	}
	lifecycleAction := selected.LifecycleAction()
	if lifecycleAction == session.LifecycleActionNone {
		return m, nil
	}
	if selected.IsTearingDown() {
		return m, m.handleError(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	title := selected.Title
	target := captureSessionActionTarget(selected, m.repoID)

	// Only a resting (Archived/Lost/Dead) row can be restored; on a live row `r`
	// does nothing (archive is on `a`).
	if lifecycleAction != session.LifecycleActionRestore {
		return m, nil
	}
	if selected.GetInFlightOp() == session.OpRestoring {
		return m, m.handleError(fmt.Errorf("session '%s' is already being restored", title))
	}
	_ = selected.Transition(session.MarkRestoring())
	return m, m.restoreInstanceCmd(target)
}

// archiveInstanceCmd runs the daemon archive (tmux teardown + worktree move) off
// the event loop (#1028), mirroring killInstanceCmd — the RPC blocks for the
// whole operation, so it must not run on the Update goroutine.
func (m *home) archiveInstanceCmd(target sessionActionTarget) tea.Cmd {
	archive := archiveSessionThroughDaemon
	return func() tea.Msg {
		if _, err := archive(target.archiveRequest()); err != nil {
			log.ErrorLog.Printf("could not archive instance %q: %v", target.title, err)
			return instanceArchivedMsg{target: target, err: err}
		}
		return instanceArchivedMsg{target: target}
	}
}

// restoreInstanceCmd runs the daemon restore/recovery off the event loop.
func (m *home) restoreInstanceCmd(target sessionActionTarget) tea.Cmd {
	restore := restoreSessionThroughDaemon
	return func() tea.Msg {
		if _, err := restore(target.restoreRequest()); err != nil {
			log.ErrorLog.Printf("could not restore instance %q: %v", target.title, err)
			return instanceRestoredMsg{target: target, err: err}
		}
		return instanceRestoredMsg{target: target}
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
	inst := m.resolveSessionActionTarget(msg.target)
	if msg.err != nil {
		// Archive failed: clear the optimistic op so the row reverts to its
		// underlying daemon liveness rather than stranding as archiving.
		if inst != nil && inst.GetInFlightOp() == session.OpArchiving {
			_ = inst.Transition(session.ClearOp())
		}
		return m, m.handleError(fmt.Errorf("failed to archive session '%s': %w", msg.target.title, msg.err))
	}
	if inst != nil {
		// SetArchived is an unconditional projection-mirror: it copies the
		// daemon's already-committed Archived state (started=false,
		// liveness=Archived, op cleared) onto the read-only row. It is NOT the
		// fenced CommitArchive transition (that's the daemon's I2 enforcement
		// point) — the TUI row may be in any state here (optimistic OpArchiving,
		// or already Archived if the reconcile settled first), so it stays a
		// direct unconditional mirror rather than a fenced edge (#1195 Phase 2d).
		inst.SetArchived()
	}
	return m, m.selectionChanged()
}

// limitRetriedMsg reports completion of an async usage-limit manual retry
// (#1146, `c`). On success the daemon cleared the limit and re-delivered the
// prompt; a non-nil err is surfaced in the error box.
type limitRetriedMsg struct {
	target sessionActionTarget
	err    error
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
	target := captureSessionActionTarget(selected, m.repoID)
	return m, m.resumeFromLimitCmd(target)
}

// resumeFromLimitCmd runs the daemon resume (re-spawn if needed + re-deliver the
// prompt + clear the limit state) off the event loop (#1146), mirroring
// restoreInstanceCmd.
func (m *home) resumeFromLimitCmd(target sessionActionTarget) tea.Cmd {
	resume := resumeFromLimitThroughDaemon
	return func() tea.Msg {
		if err := resume(target.resumeFromLimitRequest()); err != nil {
			log.ErrorLog.Printf("could not resume limited session %q: %v", target.title, err)
			return limitRetriedMsg{target: target, err: err}
		}
		return limitRetriedMsg{target: target}
	}
}

// handleLimitRetried finalizes an async usage-limit retry. On success the daemon
// has already cleared the limit + set Running and persisted; clear the local row
// optimistically for instant feedback (the badge disappears without waiting for
// the next snapshot reconcile). On failure the error lands in the error box.
func (m *home) handleLimitRetried(msg limitRetriedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf("failed to resume session '%s': %w", msg.target.title, msg.err))
	}
	if inst := m.resolveSessionActionTarget(msg.target); inst != nil {
		inst.ClearLimitReached()
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
	inst := m.resolveSessionActionTarget(msg.target)
	if msg.err != nil {
		if inst != nil && inst.GetInFlightOp() == session.OpRestoring {
			_ = inst.Transition(session.ClearOp())
		}
		return m, m.handleError(fmt.Errorf("failed to restore session '%s': %w", msg.target.title, msg.err))
	}
	if inst != nil && (inst.GetLiveness() == session.LiveLost || inst.GetLiveness() == session.LiveDead) {
		_ = inst.Transition(session.ConfirmLive())
	}
	return m, m.selectionChanged()
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
	if err := webTabAttachGuard(selected, m.store.ActiveTab()); err != nil {
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
		return fmt.Errorf("session '%s' was lost — restore it first (%s)", inst.Title, shellsuggest.Command("af", "sessions", "restore", inst.Title))
	}
	if inst.GetLiveness() == session.LiveDead {
		return fmt.Errorf("session '%s' is no longer running — restore it first (%s)", inst.Title, shellsuggest.Command("af", "sessions", "restore", inst.Title))
	}
	if inst.GetLiveness() == session.LiveArchived {
		// Archived (#1028): the user tore the session down and its worktree was
		// moved to the global archive dir; there is no tmux to enter or attach.
		// Point at the off-ramp (restore) rather than a bare "not running" —
		// same explicit-feedback contract as Lost/Deleting. Checked before
		// TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' is archived — restore it first (%s)", inst.Title, shellsuggest.Command("af", "sessions", "restore", inst.Title))
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
	if err := webTabAttachGuard(selected, m.store.ActiveTab()); err != nil {
		return m, m.handleError(err)
	}
	return m.attachSelected(selected)
}

// webTabAttachGuard fences attach/enter off a browser-only tab (web or vscode):
// neither has a PTY to attach or embed, so instead of a doomed WS-PTY
// subscription the TUI surfaces a message pointing the user at the web UI. A nil
// error means the tab is attachable.
func webTabAttachGuard(inst *session.Instance, tabIdx int) error {
	if inst == nil {
		return nil
	}
	tabs := inst.GetTabs()
	if tabIdx < 0 || tabIdx >= len(tabs) {
		return nil
	}
	switch tabs[tabIdx].Kind {
	case session.TabKindWeb:
		return fmt.Errorf("this is a web tab (%s) — view it in the web UI or open the URL in a browser", tabs[tabIdx].URL)
	case session.TabKindVSCode:
		return fmt.Errorf("this is a VS Code tab — view it in the web UI; a terminal can't render the editor")
	default:
		return nil
	}
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
// instance+tab binding. Callers pass the tab index captured at keypress time
// (#716), before help-overlay deferral can let selection or active tab drift.
func (m *home) attachInstanceTab(instance *session.Instance, tabIdx int, agentLabel, terminalLabel string) (tea.Model, tea.Cmd) {
	label := agentLabel
	if tabIdx != 0 {
		label = terminalLabel
	}
	// EVERY session — local or remote, agent tab 0 or a shell/process tab —
	// attaches CLIENT-side as a WS PTY subscriber over the daemon socket
	// (apiclient.AttachStream, #1592 Phase 2 PR7). The daemon resolves the byte
	// source via instance.AgentServer(), which is a local broker for a local
	// session and a remoteAgentServer proxy for a docker/ssh/hook one, so
	// locality is the DAEMON's concern and the client needs no branch on it.
	//
	// Do not reintroduce a Capabilities().Workspace branch that routes remote
	// sessions through the Backend: that branch is what broke remote attach
	// outright (#1837), because #1592 Phase 4 PR7 had already turned every remote
	// backend's attach into a routing-invariant error. #1852 then deleted the
	// backend attach surface entirely, so there is no longer anything to branch
	// to — reintroducing one would not compile, which is the point.
	//
	// instance + repoID are captured here at keypress so a deferred attach (help
	// overlay open) targets the captured session, not a drifted selection (#716).
	repoID := m.repoID
	attach := func() (chan struct{}, error) {
		// Address the attach by the tab's stable id (#1738) so a reorder/close can't
		// misroute the full-screen stream; empty falls back to the ordinal.
		tabID, _ := instance.TabIDAt(tabIdx)
		return attachStreamFn(context.Background(), instance.Title, repoID, tabID, tabIdx)
	}
	return m.showHelpScreen(helpAttach(instance, tabIdx), func() tea.Cmd {
		return m.beginAttachTransition(func() tea.Cmd {
			return attachOverlayCallbackFn(m, instance.Title, label, "", attach)
		})
	})
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
