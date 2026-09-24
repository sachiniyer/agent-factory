package app

import (
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"
)

// selectionChanged updates the selection binding and menu based on the
// sidebar selection, and drives the open panes' capture refresh. The
// preview/terminal tmux captures are dispatched via a tea.Cmd (goroutine)
// rather than run synchronously: each call shells out to `tmux capture-pane`
// (~3–5ms locally), and on the bubbletea Update goroutine that cost
// compounded — every previewTickMsg (100ms) blocked the event loop, and the
// first paint after detach paid the full cost on top of waiting up to a full
// tick cycle for the next msg (#579, #559 sibling). The TabPane guards its
// captured state with a mutex so the goroutine can mutate it while View()
// reads it. Synchronous fields touched here (selection binding, menu state)
// stay on the event loop.
func (m *home) selectionChanged() tea.Cmd {
	defer m.syncScrollHint()
	selectionStart := time.Now()
	detachTraceMark("selectionChanged-entry")
	sel := m.sidebar.GetSelection()
	// Advance the selection epoch when the cursor genuinely moved, so an explicit
	// pane jump's pinned intent survives its own trailing selectionChanged (the
	// cursor is unchanged) but stales the moment the user navigates (#1885).
	m.bumpSelectionEpochIfMoved(sel)

	// While attached, the workspace is hidden behind the tmux client and the
	// panes will be repainted by repaintAfterDetachMsg as soon as the user
	// detaches. Skip the refresh dispatches so they don't queue
	// capture-pane / gh pr view work behind the user's detach key (#598). The
	// synchronous mutations (binding, menu state) still run so sidebar nav
	// that happens between attach failures is consistent.
	attachedNow := m.attached.Load()

	var previewCmd tea.Cmd
	if sel.Kind == ui.SectionInstances && !sel.IsHeader {
		selected := m.sidebar.GetSelectedInstance()
		// Track the cursor's instance in the store's display selection — what
		// selection-scoped verbs (`s`, Enter-from-tree, tree-focus 1-9) act on —
		// then re-clamp the active tab index against the new instance's tab count.
		m.store.SetSelectedInstance(selected)
		m.clampSelectionTab()
		m.menu.SetInstance(selected)
		m.maybeAutoOpenInitialPane(selected)
		previewTab := m.store.ActiveTab()
		previewTabSpecific := previewTab != 0
		if sel.IsTab {
			previewTab = sel.TabIndex
			previewTabSpecific = true
		}
		previewCmd = m.updatePanePreview(selected, previewTab, previewTabSpecific, attachedNow)
		detachTrace(selectionStart, "selectionChanged-instance-branch-built-cmds")
	} else {
		// Header row: the menu drops the instance-specific hints; the open
		// panes are untouched (they are explicit bindings, not
		// selection-driven). The startup auto-open still gets its chance —
		// launch rests the cursor on the Instances header (launch selection
		// parity, #1024 PR 2), so with only the instance-branch call above a
		// cold start with restored sessions landed on the empty workspace
		// until the first cursor move (#1099 play-test).
		m.maybeAutoOpenInitialPane(nil)
		m.cancelPanePreview(false)
		m.panePreviewSuppression = nil
		// An archived row still drives the footer menu so the dedicated restore
		// key (`r`) is discoverable on it (#1605); a section header clears the
		// menu as before. Archived sessions own no live tmux, so the pane
		// preview/auto-open path above deliberately stays in its nil-instance
		// form — only the menu learns the selection.
		if sel.Kind == ui.SectionArchived && !sel.IsHeader {
			m.menu.SetInstance(m.sidebar.GetSelectedInstance())
		} else {
			m.menu.SetInstance(nil)
		}
		if selected := m.store.GetSelectedInstance(); selected != nil && !m.store.ContainsInstance(selected) {
			// The sticky binding dangles — its instance was removed (e.g. the
			// last instance killed while attached). Drop it so the pane verbs
			// can't target a dead session.
			m.store.SetSelectedInstance(nil)
		}
	}

	return tea.Batch(previewCmd, m.panesRefresh(attachedNow))
}

// clampSelectionTab bounds the selection's active tab index against the
// selected instance's tab count — the tree-selection half of the clamping the
// per-pane windows do for their own bindings (#930 PR 4 class).
func (m *home) clampSelectionTab() {
	n := len(tree.TabLabels(m.store.GetSelectedInstance()))
	if cur := m.store.ActiveTab(); cur >= n && n > 0 {
		m.store.SetActiveTab(n - 1)
	} else if cur < 0 {
		m.store.SetActiveTab(0)
	}
}

// maybeAutoOpenInitialPane opens the first selected instance's tab as the
// first pane, once per TUI run, so launch doesn't land on an empty workspace
// (#1088). Focus is NOT moved — the user is on the tree at startup. Once the
// latch is set the workspace is entirely verb-driven: hiding every pane
// leaves it empty until `s` opens one.
//
// A nil selected falls back to the first NON-reserved sidebar instance
// (firstAutoOpenCandidate): launch never auto-selects a row (the cursor rests
// on the Instances header — #1024 PR 2), so a cold start with restored sessions
// has no selection to auto-open from. Preferring a non-reserved row keeps root
// from being front-and-center after every relaunch (#1238). The fallback opens
// the pane without touching the selection, and because selectionChanged
// re-enters on every preview tick, it also re-fires once a restored instance
// leaves a transient status (#1099 play-test).
func (m *home) maybeAutoOpenInitialPane(selected *session.Instance) {
	if m.initialPaneOpened || m.store.NumOpenPanes() > 0 {
		return
	}
	if selected == nil {
		selected = firstAutoOpenCandidate(m.store.GetInstances())
		if selected == nil {
			return
		}
	}
	if selected.HasInFlightOp() {
		return
	}
	m.initialPaneOpened = true
	m.openPaneWindow(selected, m.store.ActiveTab())
	m.relayout()
}

// paneCaptureMinInterval floors each open pane's capture cadence. At the
// previewTick period (100ms) it admits one capture per pane per tick — the
// one-capture-per-pane budget of RFC §5.2 — and swallows the extra
// selectionChanged calls rapid tree navigation fires between ticks. If
// tmux-server contention ever resurfaces (#598 class), raising this ONE
// constant (e.g. to 250ms) degrades every pane's refresh without touching
// the tick.
const paneCaptureMinInterval = 100 * time.Millisecond

// panesRefresh keeps the open-pane list coherent and returns the visible
// panes' throttled capture cmds (nil when there is nothing to do). Runs on
// the event loop (#1088, generalizing the PR-5 paneBRefresh).
func (m *home) panesRefresh(attachedNow bool) tea.Cmd {
	// Prune panes whose instance left the projection (killed here, or
	// removed by an external kill the snapshot reconcile mirrored) rather
	// than keep rendering a dead session's last capture.
	if m.pruneDeadPanes() {
		m.relayout()
	}
	// All panes pause while attached (#598): no capture work may queue
	// behind the user's detach key. Auto-hidden panes don't capture either —
	// they are invisible.
	if attachedNow {
		return nil
	}
	var cmds []tea.Cmd
	for _, p := range m.visiblePanes {
		w := m.paneWindows[p.ID()]
		if w == nil {
			continue
		}
		// The pane's tab index can dangle when its instance's tab set shrank
		// (e.g. another view closed the tab this pane was showing).
		w.ClampActiveTab()
		// A pane rendering through a live termpane attachment doesn't poll
		// capture-pane (#1089): the attach client already streams the same
		// content, tmux-flow-limited. Capture resumes the tick after the
		// attachment closes.
		//
		// EXCEPT when it just entered scroll mode (#1704): scroll mode stops
		// rendering the live terminal and shows the capture viewport instead
		// (String()'s liveShowing gate is false while IsScrolling), so the
		// one-shot off-loop scrollback fill MUST run even for a live pane —
		// otherwise the viewport stays empty and the pane renders blank until
		// scroll mode is left. NeedsScrollFill is true only until that single
		// fill lands (the controller transitions loading→ready), so a live pane
		// resumes skipping capture for every subsequent scroll keystroke.
		needsFill := w.NeedsScrollFill()
		if w.HasLive() && !needsFill {
			continue
		}
		// A pane that just entered scroll mode has an empty scroll viewport
		// waiting for its off-loop scrollback capture (#1637): bypass the throttle
		// so the fill lands on this refresh instead of up to a tick later, which
		// would flash a blank viewport. Scroll-mode entry no longer captures on the
		// event loop, so this off-loop fill is the only place that populates it.
		if !needsFill && time.Since(m.lastPaneCapture[p.ID()]) < paneCaptureMinInterval {
			continue
		}
		m.lastPaneCapture[p.ID()] = time.Now()
		// Claim the fill before dispatching so the next refresh in the
		// dispatch→land window sees NeedsScrollFill go false and does not fire a
		// redundant capture (#1709). NeedsScrollFill re-arms if this capture fails
		// to publish (view changed mid-flight), so a genuinely-owed fill still runs.
		if needsFill {
			w.BeginScrollFill()
		}
		binding, seq := m.renderBindingForPane(p)
		cmds = append(cmds, refreshPaneBindingCmd(w, binding.instance, binding.tab, seq))
	}
	return tea.Batch(cmds...)
}

// focusedContentPane resolves which content pane scroll/attach keys act on:
// the focused pane when the focus ring points at one; with the tree or
// automations focused, the pane showing the selection's (instance, active
// tab) if that tab is open and visible. Returns (nil, nil) when no pane
// applies — the workspace may be empty (#1088).
func (m *home) focusedContentPane() (*ui.TabbedWindow, *session.Instance) {
	if p := m.focusedOpenPane(); p != nil {
		return m.paneWindows[p.ID()], p.Instance()
	}
	selected := m.store.GetSelectedInstance()
	if selected == nil {
		return nil, nil
	}
	if p := m.store.FindOpenPane(selected, m.store.ActiveTab()); p != nil {
		for _, vis := range m.visiblePanes {
			if vis == p {
				return m.paneWindows[p.ID()], p.Instance()
			}
		}
	}
	return nil, nil
}

// panesRefreshedMsg signals that the off-loop tab capture finished. The msg
// itself carries no payload — bubbletea calls View() after every Update return
// regardless of the msg type, and TabPane already published the captured
// content into its own mutex-guarded state inside the goroutine. Sending the
// msg back is what actually wakes the event loop so View() runs against the
// fresh content.
type panesRefreshedMsg struct{}

func refreshPaneBindingCmd(tw *ui.TabbedWindow, selected *session.Instance, activeTab int, seq uint64) tea.Cmd {
	return func() tea.Msg {
		cmdStart := time.Now()
		detachTraceMark("refreshPanesCmd-goroutine-entry")
		if err := tw.UpdateContentAt(selected, activeTab, seq); err != nil && !paneRefreshTearingDown(selected, err) {
			log.WarningLog.Printf("UpdateContent failed: %v", err)
		}
		detachTrace(cmdStart, "refreshPanesCmd-goroutine-exit")
		return panesRefreshedMsg{}
	}
}

// paneRefreshTearingDown reports the expected race where a capture dispatched
// before a session delete reaches the daemon after teardown has started.
func paneRefreshTearingDown(selected *session.Instance, err error) bool {
	return selected != nil && selected.IsTearingDown() && err.Error() == fmt.Sprintf("session %q is being deleted", selected.Title)
}

// repaintAfterDetachMsg is dispatched by the attach goroutine immediately
// after `<-ch` unblocks. Without it the first post-detach paint waits up
// to ~100ms for the next previewTickMsg (the goroutine sets stateDefault
// but bubbletea has no event queued, so View() does not re-run). The
// handler hands the actual refresh off to a tea.Cmd so the tmux
// capture-pane calls don't block the event loop (#579).
type repaintAfterDetachMsg struct{}

type keyupMsg struct {
	name keys.KeyName
}

func (m *home) keydownCallback(name keys.KeyName) tea.Cmd {
	m.menu.Keydown(name)
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
		return keyupMsg{name: name}
	}
}

func newlyAutoHiddenPane(previousVisible, nextVisible, openPanes []*store.OpenPane) *store.OpenPane {
	if len(previousVisible) == 0 || len(openPanes) <= len(nextVisible) {
		return nil
	}
	open := make(map[*store.OpenPane]bool, len(openPanes))
	for _, p := range openPanes {
		open[p] = true
	}
	visible := make(map[*store.OpenPane]bool, len(nextVisible))
	for _, p := range nextVisible {
		visible[p] = true
	}
	for _, p := range previousVisible {
		if p != nil && open[p] && !visible[p] {
			return p
		}
	}
	return nil
}

// setPaneAutoHideStatus reports the pane the terminal could not fit. It names
// the pane that was ACTUALLY displaced — `instance · tab`, the same identity the
// pane's own header shows — or says nothing about which pane when it cannot name
// one. The instance title alone is not a pane identity: since #930 an instance
// can own several panes, so "docs hidden" while a second `docs` pane is on
// screen tells the user something they can see is false (#1997).
//
// The bar truncates from the RIGHT, so the LAST fragment is the one that dies,
// and that fragment is the recovery hint — the `s` key, which is the cheaper of
// the two remedies because it needs no resize and is the one a user cannot
// discover by fiddling. #1973 bought room for it by dropping a word; #1997 then
// spent that room making the subject a full pane identity, and at 80 columns the
// line clipped to a dangling "resize wider or…" for even a five-character
// session name (#2580).
//
// So the reason clause is now the thing that pays: it says "too narrow" without
// counting the panes (the notice already names the one that went), which buys
// back enough cells for the hint to survive an ordinary title at 80 columns.
// Long titles still overflow — nothing fits an unbounded title — but the
// fragments stay ordered worst-first, so what survives is which pane went away,
// and since #2618 the clipped tail is readable in full with `E details`.
func (m *home) setPaneAutoHideStatus(p *store.OpenPane, paneCount int) {
	if p == nil || paneCount <= 1 {
		return
	}
	subject := "a pane is hidden"
	if label, ok := paneStatusLabel(p); ok {
		subject = label + " hidden"
	}
	msg := fmt.Sprintf("%s — too narrow; resize%s", subject, paneRecoveryStatusHint())
	m.pendingPaneAutoHideStatus = msg
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(msg))
}

// clearStaleAutoHideStatus drops the "N hidden: terminal too narrow" guidance
// the moment a relayout fits every open pane again — otherwise a resize wide
// enough to reveal the auto-hidden panes still leaves the narrow-width status
// on the bar, contradicting the now-visible panes (#1557). Keyed on the notice
// id so a newer, unrelated status that superseded ours is never wiped; the
// guidance's own transient timer still handles the case where it is the current
// notice but the panes never came back.
func (m *home) clearStaleAutoHideStatus() {
	if m.paneAutoHideNoticeID == 0 || m.store.NumOpenPanes() > len(m.visiblePanes) {
		return
	}
	if m.transientNoticeID == m.paneAutoHideNoticeID {
		m.errBox.Clear()
	}
	m.paneAutoHideNoticeID = 0
	m.pendingPaneAutoHideStatus = ""
}

// paneStatusLabel names a pane for a user-facing message the way the pane's own
// header names it — `instance · tab` (ui.TabbedWindow.renderHeader), reading the
// tab through the same disambiguated tree label source so the toast and the
// header can never disagree about what a pane is called.
//
// It reports false rather than guessing. An instance title alone is not a pane
// identity (#930), and tree.TabLabels answers with a placeholder "Agent" slot
// for an instance whose tabs have not materialized — so the tab is read through
// TabLabelAt, which distinguishes a real tab from "no tab list yet". A caller
// that cannot name the pane must say so instead of naming the wrong one (#1997).
func paneStatusLabel(p *store.OpenPane) (string, bool) {
	if p == nil || p.Instance() == nil || p.Instance().Title == "" {
		return "", false
	}
	label, ok := tree.TabLabelAt(p.Instance(), p.Tab())
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s · %s", p.Instance().Title, label), true
}

// paneRecoveryStatusHint names the key that brings the pane back without a
// resize. It drops the filler "use": every cell it costs comes out of its own
// survival at 80 columns, since it is the last fragment on a bar that truncates
// from the right (#2580).
func paneRecoveryStatusHint() string {
	if key := bindingKeyWithDesc("pane list"); key != "" {
		return fmt.Sprintf(" or `%s` pane list", key)
	}
	if binding, ok := keys.GlobalKeyBindings[keys.KeyOpenPane]; ok {
		help := binding.Help()
		if help.Key != "" && help.Desc != "" {
			return fmt.Sprintf(" or `%s` %s", help.Key, help.Desc)
		}
	}
	return ""
}

func bindingKeyWithDesc(desc string) string {
	for _, binding := range keys.GlobalKeyBindings {
		help := binding.Help()
		if help.Desc == desc && help.Key != "" {
			return help.Key
		}
	}
	return ""
}

func (m *home) consumePaneAutoHideStatus() tea.Cmd {
	if m.pendingPaneAutoHideStatus == "" {
		return nil
	}
	status := m.pendingPaneAutoHideStatus
	m.pendingPaneAutoHideStatus = ""
	m.paneAutoHideNoticeID = m.setTransientNotice(errors.New(status))
	return m.clearTransientMessageAfterDelay(m.paneAutoHideNoticeID)
}
