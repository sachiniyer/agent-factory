package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"

	tea "github.com/charmbracelet/bubbletea"
)

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
		// A resting (Lost/Dead) row's Enter must RESTORE it, even with a workspace
		// pane open (#2489). selectionChanged builds a preview txn for the cursor's
		// instance with no liveness gate, so this branch — not the row path below —
		// is where Enter lands in the common pane-open steady state; without this the
		// commit would only surface the guard error and the restore would never fire.
		// Same restoreIfResting the tree-row and `o` paths use, so all three agree.
		if cmd, handled := m.restoreIfResting(m.sidebar.GetSelectedInstance()); handled {
			m.cancelPanePreview(false)
			return m, cmd
		}
		// Committing a sidebar tab-row preview is a tree-navigation action (the
		// preview only exists while the tree cursor sits on a tab row), so this
		// Enter selects/commits — it must NOT type into the agent (nil replay),
		// exactly like the tree select path below (#1576, #4722).
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
		// Enter on an already-focused pane enters interactive mode. The key is
		// consumed by the mode switch and never reaches the pane's PTY — a
		// forwarded Enter would submit whatever sits on the input line (#4722).
		// #1576's "first keystroke reaches the pane" affordance now lives with
		// printable keys: an unbound letter at a focused pane enters interactive
		// mode AND types itself (dispatchKeyAction's keymap-miss branch).
		return m.enterPane(p, nil)
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
	// row) answers that the cursor resolved no row (#7700).
	if sel.Kind != ui.SectionInstances && sel.Kind != ui.SectionArchived {
		return m, m.noRowTargetNotice()
	}
	// Instance selected — in either the Instances tree or the Archived folder
	// (#1028). A resting (Lost/Dead/archived) row is not embeddable: rather than
	// surfacing the guard's "press <key> to restore" fence, Enter restores it — with a
	// confirm first when that would re-provision a remote sandbox (#2489). Any other
	// non-embeddable state still surfaces interactiveGuard's error. (This is the
	// no-preview path — an archived row cancels its preview; the Lost/Dead pane-open
	// case is handled by the preview branch above.)
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, m.noRowTargetNotice()
	}
	if selected.IsCreating() {
		return m, m.handleNotice(inFlightOpNotice(selected))
	}
	if cmd, handled := m.restoreIfResting(selected); handled {
		return m, cmd
	}
	if err := interactiveGuard(selected); err != nil {
		return m, m.handleGuardResult(err)
	}
	if err := webTabAttachGuard(selected, m.store.ActiveTab()); err != nil {
		return m, m.handleNotice(err)
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

// restoreKeyHint is the configured restore key, named in copy so a
// Lost/Dead/archived session's off-ramp points at the in-TUI action rather than
// a shell command the user would have to leave the interface to run (#2479). It
// reads the live binding, so a rebound restore key names itself.
func restoreKeyHint() string {
	return keys.GlobalKeyBindings[keys.KeyRestore].Help().Key
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
	if inst.UserKilled() {
		return fmt.Errorf("session '%s' was killed and is pending deletion", inst.Title)
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
		//
		// Restore is a key IN this interface, so the off-ramp names the key, not
		// a shell command the user would have to leave the TUI to run (#2479).
		return fmt.Errorf("session '%s' was lost — press %s to restore", inst.Title, restoreKeyHint())
	}
	if inst.GetLiveness() == session.LiveDead {
		return fmt.Errorf("session '%s' is no longer running — press %s to restore", inst.Title, restoreKeyHint())
	}
	if inst.GetLiveness() == session.LiveArchived {
		// Archived (#1028): the user tore the session down and its worktree was
		// moved to the global archive dir; there is no tmux to enter or attach.
		// Point at the off-ramp (restore) rather than a bare "not running" —
		// same explicit-feedback contract as Lost/Deleting. Checked before
		// TmuxAlive so the specific message wins.
		return fmt.Errorf("session '%s' is archived — press %s to restore", inst.Title, restoreKeyHint())
	}
	if !inst.TmuxAlive() {
		// Not a designed refusal, unlike every fence above. Those all describe a
		// state the projection already knows about and offer an off-ramp; this one
		// fires when a row still projects as RUNNING and the fresh probe disagrees
		// — the session vanished with no kill on record, or (TmuxAlive collapses
		// IsAlive's tri-state) tmux could not be asked at all. This guard is the
		// component that DISCOVERS that, so it must reach ERROR monitoring rather
		// than be filed as ordinary user feedback.
		return livenessDiscovery{cause: fmt.Errorf("session '%s' is no longer running", inst.Title)}
	}
	return nil
}

// errSessionLivenessUnexpected marks the one interactiveGuard result that is a
// discovered failure rather than a designed fence, so callers can route it by
// KIND instead of re-deriving the distinction at each site.
var errSessionLivenessUnexpected = errors.New("session liveness contradicted the projection")

// livenessDiscovery carries that mark WITHOUT putting it in front of the user.
// A plain `fmt.Errorf("%w: …", sentinel, …)` would prepend the sentinel's text
// to the transient status line and the log, so the operator would read an
// internal classification phrase instead of the specific, actionable sentence
// the guard composed. Error() delegates to the cause; only errors.Is sees the
// marker, via the multi-error Unwrap.
type livenessDiscovery struct{ cause error }

func (e livenessDiscovery) Error() string   { return e.cause.Error() }
func (e livenessDiscovery) Unwrap() []error { return []error{e.cause, errSessionLivenessUnexpected} }

// handleGuardResult routes an interactiveGuard/webTabAttachGuard error to the
// severity its KIND deserves: a fence the user ran into is a notice, a liveness
// contradiction the guard just discovered is an error. Every guard call site
// goes through here so the two can never drift apart per site (#2575).
func (m *home) handleGuardResult(err error) tea.Cmd {
	if errors.Is(err, errSessionLivenessUnexpected) {
		return m.handleError(err)
	}
	return m.handleNotice(err)
}

// handleAttach is the full-screen attach verb (`o`; the pre-#1089-PR-2
// Enter): attach the FOCUSED pane's (instance, tab) full-screen (#1088), or
// the tree selection's. The attach path itself is untouched: the same
// attachOverlayCallbackFn seam, `attached` gate, and SIGKILL-bounded detach.
//
// A focused pane that owns a transient #1321 preview renders the preview's
// TARGET, not its committed binding. `o` acts on "the thing on screen", so it
// resolves the preview first — mirroring handleEnter's preview branch — rather
// than reading the raw binding like handleEnterPane does. Without this the two
// attach-flavored verbs disagreed on identical state: a pane committed to alpha
// but visibly previewing beta attached alpha on `o` while Enter committed the
// preview and entered beta.
func (m *home) handleAttach() (tea.Model, tea.Cmd) {
	// Re-entry guard mirroring handleEnter/enterPane (#1530): a second `o` in the ~20ms beginAttachTransition window must not rebind the pane before the attach funnel rejects it.
	if m.attachTransitioning || m.attached.Load() {
		return m, nil
	}
	if p := m.focusedOpenPane(); p != nil {
		// Resolve a transient preview before attaching so `o` attaches the
		// (instance, tab) the pane is VISIBLY showing, not the committed
		// binding — the #1321 wrong-target class Enter closes by committing
		// the preview first. A resting target restores; a no-preview pane
		// attaches as before.
		if m.paneIsPreviewing(p) {
			if cmd, handled := m.restoreIfResting(m.sidebar.GetSelectedInstance()); handled {
				m.cancelPanePreview(false)
				return m, cmd
			}
			prevNotice := m.paneAutoHideNoticeID
			committed, commitCmd := m.commitPanePreviewReplace()
			if committed == nil {
				return m, commitCmd
			}
			mod, attachCmd := m.handleEnterPane(committed)
			return mod, m.rearmAutoHideNoticeClear(prevNotice, attachCmd) // drop commitCmd's contending capture; retain the notice's clear timer (#2639)
		}
		return m.handleEnterPane(p)
	}
	sel := m.sidebar.GetSelection()
	// Accept both the Instances tree and the Archived folder (#1028): a resting
	// (Lost/Dead/archived) row is non-attachable, so `o` restores it — with a
	// confirm first when that would re-provision a remote sandbox (#2489) — rather
	// than surfacing the guard's "press <key> to restore" fence; any other non-attachable
	// state surfaces interactiveGuard's error rather than a silent no-op.
	if sel.IsHeader || (sel.Kind != ui.SectionInstances && sel.Kind != ui.SectionArchived) {
		return m, m.noRowTargetNotice()
	}
	selected := m.sidebar.GetSelectedInstance()
	if selected == nil {
		return m, m.noRowTargetNotice()
	}
	if selected.IsCreating() {
		return m, m.handleNotice(inFlightOpNotice(selected))
	}
	if cmd, handled := m.restoreIfResting(selected); handled {
		return m, cmd
	}
	if err := interactiveGuard(selected); err != nil {
		return m, m.handleGuardResult(err)
	}
	if err := webTabAttachGuard(selected, m.store.ActiveTab()); err != nil {
		return m, m.handleNotice(err)
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
		return fmt.Errorf("this is a web tab (%s) — view it in the web UI or open the URL in a browser",
			agentproto.RedactAccessTokenURL(tabs[tabIdx].URL))
	case session.TabKindVSCode:
		return fmt.Errorf("this is a VS Code tab — view it in the web UI; a terminal can't render the editor")
	case session.TabKindDocs:
		return fmt.Errorf("this is a Proposal tab — the proposal editor is web only; view it in the web UI")
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
	target := captureSessionActionTarget(instance, repoID)
	attach := func() (chan struct{}, error) {
		// Address the attach by the tab's stable id (#1738) so a reorder/close can't
		// misroute the full-screen stream; empty falls back to the ordinal. The
		// session axis is addressed the same way — by stable id when the record
		// has one — so a same-title replacement between keypress and dial does
		// not receive the user's keystrokes.
		tabID, _ := instance.TabIDAt(tabIdx)
		return attachStreamFn(context.Background(), streamAddress(instance, repoID), tabID, tabIdx)
	}
	return m.showHelpScreen(helpAttach(instance, tabIdx), func() tea.Cmd {
		return m.beginAttachTransition(func() tea.Cmd {
			return attachOverlayCallbackFn(m, target, label, "", attach)
		})
	})
}
