package app

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	tea "github.com/charmbracelet/bubbletea"
)

// handleDefaultKeyPress handles key events in stateDefault (main interaction state).
func (m *home) handleDefaultKeyPress(msg tea.KeyMsg, name keys.KeyName) (tea.Model, tea.Cmd) {
	switch name {
	case keys.KeyHelp:
		return m.showHelpScreen(helpTypeGeneral{}, nil)
	case keys.KeyErrorDetails:
		// Reaching here means openNoticeDetails declined the key: the bar
		// advertises no details key in this state, so none may open them.
		return m, nil

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
		return m.startNewInstanceAtBackend()

	case keys.KeyNew:
		return m.startNewInstance()

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

	// Scrolling (each pane scrolls its own view, #1088). With nothing
	// scrollable the key answers rather than reading as dead (#7700).
	case keys.KeyShiftUp:
		m.syncPaneScrollOwners()
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollHalfPageUp()
			return m, m.selectionChanged()
		}
		return m, m.handleNotice(errNothingToScroll)
	case keys.KeyShiftDown:
		m.syncPaneScrollOwners()
		if pane, _ := m.focusedContentPane(); pane != nil {
			pane.ScrollHalfPageDown()
			return m, m.selectionChanged()
		}
		return m, m.handleNotice(errNothingToScroll)

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
	case keys.KeyRenameTab:
		return m.showRenameTabPrompt()
	case keys.KeyJumpTabPrompt: // unbounded jump; see handle_jump_tab.go (#3021)
		return m.showJumpTabPrompt()
	// </> permute the roster through the daemon's ReorderTab (#1813) — the same
	// path the web's drag reorder and `af sessions tab-reorder` take.
	case keys.KeyMoveTabLeft:
		return m.handleMoveTab(-1)
	case keys.KeyMoveTabRight:
		return m.handleMoveTab(1)

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

// errNothingToScroll is the answer the scroll keys owe when no pane can take
// them (#7700): neither a focused pane nor a visible pane bound to the
// selection's active tab exists — the same dead case focusedContentPane's
// (nil, nil) return used to swallow.
var errNothingToScroll = errors.New("nothing to scroll — no pane is showing the selected tab")

// inFlightOpNotice names the non-teardown operation fencing a row's lifecycle
// verbs (#7551): the op can arrive between the last painted frame and the key
// press — the 750ms snapshot reconcile adopts daemon-owned ops onto the row
// (adoptSnapshotOp) — so the fence was not on screen when the verb read as
// offered. Naming what the row is doing beats the silence an unguarded press
// got before, and beats the daemon's own "busy; try again" refusal a
// dispatched RPC would surface instead. Teardown ops report through
// IsTearingDown's own notices before this is reached.
func inFlightOpNotice(inst *session.Instance) error {
	switch inst.GetInFlightOp() {
	case session.OpCreating:
		return fmt.Errorf("session '%s' is still being created", inst.Title)
	case session.OpRestoring:
		return fmt.Errorf("session '%s' is already being restored", inst.Title)
	case session.OpReplacing:
		return fmt.Errorf("session '%s' is mid-handoff", inst.Title)
	case session.OpRespawning:
		return fmt.Errorf("session '%s' is resuming", inst.Title)
	}
	return nil
}

// noRowTargetNotice is the answer a cursor-row verb — o, Enter, F, c — owes
// when its own row resolves no session at all (#7700). The row-verb rules
// still apply: while the cursor rests on a section header marked with a
// resting row's ▾, that row answers for itself through interactiveGuard —
// the same fence the verb would have hit on the row itself. Every other
// unresolvable row (a foreign section, the rail separator, an empty rail, or
// a live-bound header the row-verb rules refuse to adopt) names the bare
// absence, matching handleKill's nil case (#5373).
func (m *home) noRowTargetNotice() tea.Cmd {
	if sel := m.sidebar.RowVerbTarget(); sel != nil {
		if err := interactiveGuard(sel); err != nil {
			return m.handleGuardResult(err)
		}
	}
	return m.handleNotice(errors.New("No session under the cursor"))
}

// handleKill handles the kill/delete session action against the sidebar's
// row-verb target — the cursor's row, or the ▾-marked resting row while the
// cursor rests on its section header (#4755). The confirmation only
// flips the row to Deleting; the slow teardown (remote delete_cmd over ssh,
// tmux kill, worktree removal) runs in killInstanceCmd's background goroutine
// so the event loop never blocks on it (#844).
func (m *home) handleKill() (tea.Model, tea.Cmd) {
	selected := m.sidebar.RowVerbTarget()
	if selected == nil {
		return m, m.handleNotice(errors.New("No session under the cursor"))
	}
	// IsTearingDown BEFORE CanKill: a teardown-in-progress row now reports
	// CanKill()==false (#2500), so checking CanKill first would fold this
	// informative "already being deleted" message into the silent no-op below.
	// The gate hides the keybind; this still answers a key pressed anyway.
	if selected.IsTearingDown() {
		return m, m.handleNotice(fmt.Errorf("session '%s' is already being deleted", selected.Title))
	}
	if !selected.CanKill() {
		// A non-teardown in-flight op (create/restore/handoff/respawn) is the
		// transient fence #7551's rapid verb hit; an id-less row cannot
		// address the destructive API at all. Either way the press must say
		// so rather than no-op silently (#5373 convention).
		if notice := inFlightOpNotice(selected); notice != nil {
			return m, m.handleNotice(notice)
		}
		return m, m.handleNotice(fmt.Errorf("session '%s' can't be deleted", selected.Title))
	}

	// Capture stable identity at confirmation-open time. A title may be reused
	// while the modal is open, so every later phase resolves this target instead
	// of whichever current row happens to carry the display title (#2358).
	selectedTitle := selected.Title
	target := m.captureProjectActionTarget(selected)

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

	// Kill force-removes the worktree and may force-delete the branch, so the
	// confirmation names what that destroys (killLossAssessment). The git reads
	// behind it take tens of ms and up to ~30s when git is wedged, so they never
	// run here, on the event loop (#4848): the dialog opens at once with a static
	// pending note and its confirm withheld, and killLossCheckedMsg completes it.
	// Both checks skip for backends without a local worktree (e.g. remote hook
	// sessions), which get their final dialog straight away.
	reserved := session.IsReservedTitle(selectedTitle)
	var cleanupImpact *session.WorktreeCleanupImpact
	var loss killLossAssessment
	// An unreachable row keeps Kill available — that is the deliberate escape
	// hatch for a wedged remote — but the confirmation must say what the flag
	// means: af cannot verify the teardown lands, and the sandbox may keep
	// running with everything it never pushed (#5571 W1.3). The flag is
	// in-memory, so this warning costs no I/O and rides every kill path below,
	// pending and direct alike.
	if u := selected.UnreachableSnapshot(); u != nil {
		age := time.Since(u.Since)
		if age < 0 {
			age = 0
		}
		loss.warnings = append(loss.warnings, fmt.Sprintf(
			"Warning: af cannot reach this session's sandbox — unreachable for %s (%s). "+
				"The sandbox may still be running and holding unpushed work, and af cannot confirm the remote teardown.",
			age.Round(time.Second), u.Error))
	}
	if selected.Capabilities().Workspace == session.WorkspaceRemote {
		loss.remote = true
		// An archived row that kept its workspace on its host (W8.5) still owns it,
		// and kill removes it. Nothing is measured for a row with no agent-server, so
		// the dialog says what is known rather than staying silent.
		if w := selected.ArchivedWorkspace(); selected.IsArchived() && w != nil && !w.Pushed && w.Dir != "" {
			loss.warnings = append(loss.warnings, fmt.Sprintf(
				"Warning: This session was archived without pushing. Its workspace at %s still holds its work, and killing the session deletes it.", w.Dir))
		}
	}
	if selected.Capabilities().Workspace == session.WorkspaceLocalWorktree {
		// GetWorktreeCleanupImpact is an in-memory snapshot, intentionally not
		// gated on started (unlike GetGitWorktree), so a session that HAS a
		// worktree but was never started — e.g. a restore-failed session — still
		// gets the unmerged-work warning (#2029). It is the authority for
		// ownership: external worktrees survive kill, and a reused user branch
		// survives even when its linked worktree does not.
		impact, hasWorktree := selected.GetWorktreeCleanupImpact()
		if hasWorktree {
			cleanupImpact = &impact
		}
		if hasWorktree && impact.RemoveWorktree && impact.Path != "" {
			return m, m.openPendingKillConfirm(selectedTitle, reserved, cleanupImpact, loss, killAction)
		}
	}
	// An off-box workspace is measured where it lives: the daemon asks the
	// session's agent-server, the dialog stays pending meanwhile, and what comes
	// back is stated the way a local worktree's loss is (#5571 W8.2). An
	// unreachable row skips the measurement: its host cannot answer, and the
	// notice above already says the loss cannot be known.
	if selected.Capabilities().Workspace == session.WorkspaceRemote && !selected.IsArchived() && selected.UnreachableSnapshot() == nil {
		return m, m.openPendingRemoteKillConfirm(selectedTitle, reserved, target, loss, killAction)
	}
	return m, m.openKillConfirm(selectedTitle, reserved, cleanupImpact, loss, killAction)
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
		// A wedged daemon took the kill request and went quiet
		// (errDaemonUnresponsive), so the teardown may have landed with only the
		// reply lost — the outcome is unknown. A LOCAL daemon has an in-interface
		// recovery: offer to restart it (#2479) instead of surfacing a shell
		// command. A REMOTE daemon is on another machine, so the local restart
		// cannot help; route it to the "could not be confirmed" message below
		// rather than the "session is retained" recovery, which would invite a
		// second kill of a session that may already be gone. This matches on
		// errDaemonUnresponsive directly because killSessionThroughDaemon rewraps
		// the timeout with a single %w, dropping context.DeadlineExceeded from
		// the chain that mutationOutcomeUnknown (below) would otherwise key on —
		// so a remote timeout cannot be recognized as uncertain there and must
		// be routed here (#4824).
		if errors.Is(msg.err, errDaemonUnresponsive) {
			// The offer is a modal, so not while the user is leaving this
			// project (#7379); the outcome-unknown notice below says it instead.
			if !isRemoteTarget() && !m.actionElsewhere(msg.target.repoID) {
				return m, m.offerDaemonRestart(msg.target.title)
			}
			action, where := m.outcomeScope(fmt.Sprintf("killing session '%s'", msg.target.title), msg.target.repoID, msg.target.repoRoot)
			return m, m.handleError(mutationOutcomeError(action, where, msg.err))
		}
		// The daemon may have torn the session down with only the reply lost
		// (#4824). The fence still reverts above — holding it would strand the
		// row if the kill never ran — but the row is not "retained" as far as
		// anyone knows: the next snapshot removes it if the kill landed. Say so,
		// rather than invite a second kill of a session that may be gone.
		if mutationOutcomeUnknown(msg.err) {
			action, where := m.outcomeScope(fmt.Sprintf("killing session '%s'", msg.target.title), msg.target.repoID, msg.target.repoRoot)
			return m, m.handleError(mutationOutcomeError(action, where, msg.err))
		}
		return m, m.showSessionRecovery(msg.target.title, msg.target.repoID, msg.target.repoRoot, "Cannot kill session", "The session is retained. "+msg.err.Error(), "Press any key to return to the session.", fmt.Errorf("failed to kill session '%s': %w", msg.target.title, msg.err))
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
		return m, m.handleNotice(errors.New("No session under the cursor"))
	}
	// IsTearingDown BEFORE the LifecycleAction gate: a teardown-in-progress row now
	// reports LifecycleAction()==None (#2500), so gating first would fold this
	// informative message into the silent no-op below.
	if selected.IsTearingDown() {
		return m, m.handleNotice(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	// A non-teardown in-flight op (create/restore/handoff/respawn) is the
	// transient fence #7551's rapid verb hit: adopted onto the row between the
	// painted frame and the press, it reports LifecycleAction()==None. Answer
	// the press by name rather than dropping it silently (#5373 convention).
	if notice := inFlightOpNotice(selected); notice != nil {
		return m, m.handleNotice(notice)
	}
	lifecycleAction := selected.LifecycleAction()
	if lifecycleAction == session.LifecycleActionNone {
		return m, m.handleNotice(fmt.Errorf("session '%s' can't be archived", selected.Title))
	}
	title := selected.Title
	target := m.captureProjectActionTarget(selected)

	// A resting (Archived/Lost/Dead) row has no live worktree/tmux to tear down;
	// `a` archives nothing there — say so rather than no-op silently, and point
	// at the restore key that owns that row's transition (#5373 convention).
	if lifecycleAction != session.LifecycleActionArchive {
		return m, m.handleNotice(fmt.Errorf("session '%s' is not live — restore it with %s", title, restoreKeyHint()))
	}

	// Live row → archive. Fail fast on the unarchivable session kinds for a
	// snappy message (the daemon rejects them too).
	if !selected.Capabilities().Archive {
		return m, m.handleNotice(fmt.Errorf("cannot archive remote session '%s': it has no local worktree to relocate", title))
	}
	if selected.IsExternalWorktree() {
		return m, m.handleNotice(fmt.Errorf("cannot archive in-place session '%s': archive relocates the worktree, which it doesn't own", title))
	}

	message := fmt.Sprintf("Archive session %q?\n\nLocal: stop terminals and move the worktree to the archive.\nSandboxes: publish work, then remove the sandbox.\nRestore with %s.", title, restoreKeyHint())
	return m, m.confirmAction(message, func() tea.Msg {
		// Raise the optimistic OpArchiving op so the row visibly shows archiving
		// while the RPC runs (#1195). It composes to Deleting for rendering; the
		// reconcile clears it once the daemon liveness settles on Archived, and the
		// completion handler finalizes it — so the row can never strand (#1187).
		inst := m.resolveSessionActionTarget(target)
		if inst == nil {
			return nil
		}
		// BeginArchive is non-total (s.op == OpNone && s.liveness != LiveArchived),
		// so a background snapshot settling the row to LiveArchived while this
		// overlay is open refuses here. The row is already where the user asked it
		// to go — the row's own press-time gate (lifecycleAction != Archive ⇒
		// no-op) reaches the same answer for an Archived row — so a refused
		// transition on LiveArchived is a silent no-op: suppress the redundant
		// archive RPC the daemon would reject with ErrAlreadyArchived (the
		// contradictory "Cannot archive session … already archived" recovery
		// modal). The busy-op refusal (OpRestoring/OpKilling/OpRespawning from
		// another client) still falls through and lets the daemon authoritatively
		// refuse with its "busy; try again" modal — that feedback is accurate, not
		// contradictory, so its UX is preserved unchanged.
		if err := inst.Transition(session.BeginArchive()); err != nil {
			if inst.GetLiveness() == session.LiveArchived {
				return nil
			}
		}
		return startArchiveMsg{target: target}
	})
}

// handleRestore is the restore verb (`r`, #1605): on an Archived/Lost/Dead row it
// restores the session (non-destructive — no confirm). The target is the
// sidebar's row-verb target — the cursor's row, or the ▾-marked resting row
// while the cursor rests on the expanded Archived header (#4755). On a live
// row there is nothing to restore, so `r` is a no-op — archive stays on `a`
// (handleArchive).
//
// No confirmation: restore only moves the worktree back and re-spawns the agent.
// Raise the optimistic OpRestoring op (mirroring how archive raises OpArchiving):
// it re-homes the row into the live Instances section AT ONCE via ShownArchived
// (#1210) and fences a double-restore, while leaving liveness LiveArchived so the
// snapshot reconcile still observes the Archived→live transition and runs its
// rebuild/re-Start (#1203). The reconcile rebuild clears the op by replacing the
// row; a restore failure clears it in handleInstanceRestored.
func (m *home) handleRestore() (tea.Model, tea.Cmd) {
	selected := m.sidebar.RowVerbTarget()
	if selected == nil {
		// No restorable row resolves — a collapsed "▶ Archived" header, a
		// section header whose bound row is live or off-screen, an empty rail.
		// `r` still does nothing, but it is no longer silent (#5373); the
		// folder stays folded and the cursor stays put.
		return m, m.handleNotice(errors.New("No archived session under the cursor"))
	}
	// IsTearingDown BEFORE the LifecycleAction gate: a teardown-in-progress row now
	// reports LifecycleAction()==None (#2500), so gating first would fold this
	// informative message into the silent no-op below.
	if selected.IsTearingDown() {
		return m, m.handleNotice(fmt.Errorf("session '%s' is being deleted", selected.Title))
	}
	// OpRestoring is intentionally projected without a lifecycle action, but
	// the restore verb still explains why a repeated request is fenced.
	if selected.GetInFlightOp() == session.OpRestoring {
		return m, m.handleNotice(fmt.Errorf("session '%s' is already being restored", selected.Title))
	}
	lifecycleAction := selected.LifecycleAction()
	if lifecycleAction == session.LifecycleActionNone {
		// A row with no lifecycle verb — creating, replacing, fenced by
		// startup-unknown, a kill tombstone — cannot take `r` (#5373: say so
		// rather than no-op silently).
		return m, m.handleNotice(fmt.Errorf("Session '%s' can't be restored", selected.Title))
	}
	target := m.captureProjectActionTarget(selected)

	// Only a resting (Archived/Lost/Dead) row can be restored; on a live row `r`
	// restores nothing (archive is on `a`) — and says so (#5373).
	if lifecycleAction != session.LifecycleActionRestore {
		return m, m.handleNotice(fmt.Errorf("Session '%s' is live — nothing to restore", selected.Title))
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
		if _, _, err := archive(target.archiveRequest()); err != nil {
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
	committedWarning := msg.err != nil && apiclient.IsMutationCommitted(msg.err)
	if msg.err != nil && !committedWarning {
		// Archive failed or may have: clear the optimistic op so the row reverts
		// to its underlying daemon liveness rather than stranding as archiving.
		// The next snapshot then shows it Archived if the daemon did archive it.
		if inst != nil && inst.GetInFlightOp() == session.OpArchiving {
			_ = inst.Transition(session.ClearOp())
		}
		// An archive whose reply was lost is not "retained" (#4824): the
		// snapshot decides, and the user checks before archiving again.
		if mutationOutcomeUnknown(msg.err) {
			action, where := m.outcomeScope(fmt.Sprintf("archiving session '%s'", msg.target.title), msg.target.repoID, msg.target.repoRoot)
			return m, m.handleError(mutationOutcomeError(action, where, msg.err))
		}
		return m, m.showSessionRecovery(msg.target.title, msg.target.repoID, msg.target.repoRoot, "Cannot archive session", "The session is retained. "+msg.err.Error(), "Press any key to return to the session.", fmt.Errorf("failed to archive session '%s': %w", msg.target.title, msg.err))
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
	if committedWarning {
		return m, tea.Batch(m.selectionChanged(), m.handleError(msg.err))
	}
	return m, m.selectionChanged()
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
// A failure clears the overlay; a committed warning keeps the live projection and shows its report.
func (m *home) handleInstanceRestored(msg instanceRestoredMsg) (tea.Model, tea.Cmd) {
	inst := m.resolveSessionActionTarget(msg.target)
	committedWarning := msg.err != nil && apiclient.IsMutationCommitted(msg.err)
	if msg.err != nil && !committedWarning {
		// Clearing the overlay is right for an unknown outcome too: it leaves
		// liveness alone, so a restore that did land still reaches the
		// reconcile's Archived→live rebuild on the next snapshot (#1203).
		if inst != nil && inst.GetInFlightOp() == session.OpRestoring {
			_ = inst.Transition(session.ClearOp())
		}
		if mutationOutcomeUnknown(msg.err) {
			action, where := m.outcomeScope(fmt.Sprintf("restoring session '%s'", msg.target.title), msg.target.repoID, msg.target.repoRoot)
			return m, m.handleError(mutationOutcomeError(action, where, msg.err))
		}
		action, _ := m.outcomeScope(fmt.Sprintf("restore session '%s'", msg.target.title), msg.target.repoID, msg.target.repoRoot)
		return m, m.handleError(fmt.Errorf("failed to %s: %w", action, msg.err))
	}
	if inst != nil && (inst.GetLiveness() == session.LiveLost || inst.GetLiveness() == session.LiveDead) {
		_ = inst.Transition(session.ConfirmLive())
	}
	refresh := m.selectionChanged()
	if committedWarning {
		return m, tea.Batch(refresh, m.handleError(msg.err))
	}
	return m, refresh
}
