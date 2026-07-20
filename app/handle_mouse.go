package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// This file is the root mouse router (#1024 R4, RFC §2.5 — closes #1025).
// Every tea.MouseMsg is resolved to a zone id through the registry the last
// View() rebuilt, then dispatched to the same handlers the keyboard drives —
// the mouse is purely additive and the keyboard remains fully sufficient.
//
// Interactive mode (#1089, RFC §2.3) splits the screen in two: events over
// the live pane's terminal grid FORWARD into the embedded terminal (the
// emulator only passes them to the inner app if it enabled mouse tracking);
// events over the rest of the pane are suppressed — never a host action —
// and everywhere outside the pane the host gestures apply as usual (§2.5).
//
// While full-screen ATTACHED none of this runs: bubbletea's Update loop is
// blocked and tmux owns stdin, so tmux's own mouse mode applies — unchanged.

// doubleClickInterval is how close two presses on the same zone must land to
// read as a double click (interact on tree rows, open the manager on task
// rows).
const doubleClickInterval = 400 * time.Millisecond

// tabDragThresholdCells is the approved drag-start threshold: a tab-row press
// becomes a drag once motion moves at least this many terminal cells by
// Manhattan distance. Smaller movement replays a normal click on release.
const tabDragThresholdCells = 2

type tabDragState struct {
	title string
	tab   int
	// tabID is the grabbed tab's stable id (#1738) — the drag's real identity.
	// tab rides along as the ordinal fallback for the windows where there is no id
	// to key on; see dragTabIndex.
	tabID string
	// instance is the session pressed on, held ONLY to compare against the one the
	// drop re-resolves by title — never dereferenced, so it may safely dangle. A
	// same-title swap replaces the session between press and release, and that is
	// what tells the id apart from a stale pointer: see dragTabIndex.
	instance *session.Instance
	label    string
	zone     string

	startX int
	startY int
	x      int
	y      int

	active       bool
	targetRegion string
}

// wireZoneRegistry hands the shared registry to every long-lived
// zone-producing pane. Called once at construction (newHome / test wiring);
// pane windows are wired at creation (openPaneWindow) instead, since they
// come and go with the open-pane list.
func (m *home) wireZoneRegistry() {
	m.sidebar.SetZoneRegistry(m.zones)
	m.automations.SetZoneRegistry(m.zones)
	m.projects.SetZoneRegistry(m.zones)
	m.menu.SetZoneRegistry(m.zones)
}

// handleMouse routes a mouse event: wheel scrolls the region under the
// cursor, left press/candidate/release handles sidebar tab drag-drop before
// interactive forwarding, and the remaining left presses click the zone under
// them. Release/motion events matter to tab drags and to interactive terminal
// forwarding; other host gestures act on press.
func (m *home) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.handleTabDragMouse(msg); handled {
		if m.interactive && m.state == stateDefault {
			m.enforceInteractiveInvariant()
		}
		return m, cmd
	}
	if m.interactive && m.state == stateDefault {
		if done := m.forwardInteractiveMouse(msg); done {
			return m, nil
		}
		// The event landed outside the live pane: host gestures apply
		// (§2.5). A gesture that moves focus off the pane implicitly ends
		// the mode — enforceInteractiveInvariant runs after dispatch below.
		defer m.enforceInteractiveInvariant()
	}
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		return m, m.handleWheel(msg)
	case tea.MouseButtonLeft:
		return m.handleClick(msg)
	}
	return m, nil
}

func (m *home) handleTabDragMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		if m.tabDrag != nil {
			m.clearDragState()
		}
		return false, nil
	}
	if msg.Button != tea.MouseButtonLeft {
		if msg.Action == tea.MouseActionPress && m.tabDrag != nil {
			m.clearDragState()
		}
		return false, nil
	}

	switch msg.Action {
	case tea.MouseActionPress:
		return m.handleTabDragPress(msg), nil
	case tea.MouseActionMotion:
		return m.handleTabDragMotion(msg), nil
	case tea.MouseActionRelease:
		return m.handleTabDragRelease(msg)
	default:
		return false, nil
	}
}

func (m *home) handleTabDragPress(msg tea.MouseMsg) bool {
	if m.tabDrag != nil {
		m.clearDragState()
	}
	if m.state != stateDefault {
		return false
	}
	id, _, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		return false
	}
	title, idx, ok := zones.TreeTabParts(id)
	if !ok {
		return false
	}
	inst := m.store.GetInstanceByTitle(title)
	if inst == nil || idx < 0 || idx >= len(tree.TabLabels(inst)) {
		return false
	}
	tabID, _ := inst.TabIDAt(idx)
	m.tabDrag = &tabDragState{
		title:    title,
		tab:      idx,
		tabID:    tabID,
		instance: inst,
		label:    paneBindingLabel(paneBinding{instance: inst, tab: idx}),
		zone:     id,
		startX:   msg.X,
		startY:   msg.Y,
		x:        msg.X,
		y:        msg.Y,
	}
	return true
}

// dragTabIndex resolves the grabbed tab to its CURRENT slot. A drag captures its
// target at press and acts on it at release — a human-time window in which
// another client can permute the roster underneath the gesture. That window is
// newly reachable: #1813 is what first lets an out-of-band reorder reach a
// running TUI at all, and there is no TUI reorder gesture, so every reorder a
// drag sees is out-of-band. Keyed by the captured ORDINAL, the drop opens
// whatever slid into that slot while the drag ghost still renders the label of
// the tab the user actually grabbed.
//
// Keyed by the tab's stable id (#1738), the drag follows its own tab wherever it
// moved — the same identity the pane rebind and the tree's selection use, and the
// other half of the instance-by-title re-resolution this gesture already does.
//
// An id that resolves to nothing within that session means the grabbed tab is
// GONE (a concurrent close), and the answer is no target: falling back to the
// ordinal there would act on a different tab under the guise of being helpful,
// which is the exact failure this exists to prevent.
//
// The id is the identity only WITHIN the session that was pressed — the same
// split tabIdentityDomain draws for the pane rebind. A same-title kill/recreate
// (#765) can swap in an ENTIRELY NEW session mid-drag, whose tabs are freshly
// minted, so every id differs by construction; keyed by id the drop would resolve
// nothing and silently do away with the gesture. There the equivalent SLOT of the
// replacement is the right target — the drop's pre-existing, tested behavior
// (TestMouse_DragDropReresolvesInstanceAfterProjectionSwap), preserved by falling
// through to the ordinal.
//
// The ordinal is likewise the key where there is simply no id to key on: an
// AttachShellTab tab before the snapshot backfills the daemon's id, or a
// pre-#1738 roster row. Both fall-throughs keep the caller's original range guard.
func dragTabIndex(inst *session.Instance, drag *tabDragState) (int, bool) {
	if inst == nil || drag == nil {
		return 0, false
	}
	if drag.tabID != "" && inst == drag.instance {
		return inst.TabIndexByID(drag.tabID)
	}
	if drag.tab < 0 || drag.tab >= len(tree.TabLabels(inst)) {
		return 0, false
	}
	return drag.tab, true
}

func (m *home) handleTabDragMotion(msg tea.MouseMsg) bool {
	drag := m.tabDrag
	if drag == nil {
		return false
	}
	drag.x = msg.X
	drag.y = msg.Y
	if !drag.active && manhattanDistance(drag.startX, drag.startY, msg.X, msg.Y) >= tabDragThresholdCells {
		drag.active = true
	}
	if drag.active {
		drag.targetRegion = m.tabDragDropRegion(msg.X, msg.Y)
	}
	return true
}

func (m *home) handleTabDragRelease(msg tea.MouseMsg) (bool, tea.Cmd) {
	drag := m.tabDrag
	if drag == nil {
		return false, nil
	}
	if !drag.active {
		now := m.mouseClock()
		double := drag.zone == m.lastClickZone && now.Sub(m.lastClickAt) <= doubleClickInterval
		m.clearDragState()
		if !double {
			m.lastClickZone = drag.zone
			m.lastClickAt = now
		}
		// A click is a short drag, so it carries the same staleness — the press
		// captured the target and this release acts on it. Same identity rule.
		idx, ok := dragTabIndex(m.store.GetInstanceByTitle(drag.title), drag)
		if !ok {
			return true, nil
		}
		return true, m.handleTreeTabClick(drag.title, idx, double)
	}
	drag = m.clearDragState()
	region := m.tabDragDropRegion(msg.X, msg.Y)
	if region == "" {
		return true, nil
	}
	return true, m.dropTabOnPane(drag)
}

func (m *home) tabDragDropRegion(x, y int) string {
	id, _, ok := m.zones.Resolve(x, y)
	if !ok {
		return ""
	}
	region, _, isPane := zones.PaneZone(id)
	if !isPane {
		return ""
	}
	p, _ := m.paneByRegion(region)
	if p == nil {
		return ""
	}
	return region
}

func (m *home) tabDragDropTargetRegion() string {
	if m.tabDrag == nil || !m.tabDrag.active {
		return ""
	}
	return m.tabDrag.targetRegion
}

func (m *home) dropTabOnPane(drag *tabDragState) tea.Cmd {
	if drag == nil {
		return nil
	}
	inst := m.store.GetInstanceByTitle(drag.title)
	idx, ok := dragTabIndex(inst, drag)
	if !ok {
		return nil
	}
	m.focusRegionClick(layout.RegionTree)
	m.sidebar.SelectTabRow(drag.title, idx)
	_, openCmd := m.openOrFocusPane(inst, idx)
	return openCmd
}

func (m *home) clearDragState() *tabDragState {
	drag := m.tabDrag
	m.tabDrag = nil
	m.lastClickZone = ""
	m.lastClickAt = time.Time{}
	return drag
}

func manhattanDistance(x1, y1, x2, y2 int) int {
	dx := x1 - x2
	if dx < 0 {
		dx = -dx
	}
	dy := y1 - y2
	if dy < 0 {
		dy = -dy
	}
	return dx + dy
}

// forwardInteractiveMouse implements the RFC §2.5 in-pane ownership rule
// while interactive: any event over the live pane's rect belongs to the
// embedded terminal — grid-cell events forward through the emulator's
// mode-aware SendMouse (translated to pane-local coordinates by the zone
// resolve), and events over the pane's frame/header are suppressed so a
// stray click can never trigger a host action under the user's typing.
// Reports whether the event was consumed (forwarded or suppressed).
func (m *home) forwardInteractiveMouse(msg tea.MouseMsg) bool {
	lt, p := m.focusedLiveTerm()
	if lt == nil || p == nil {
		return false
	}
	id, local, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		// A miss is outside every pane; nothing to do either way.
		return true
	}
	region, kind, isPane := zones.PaneZone(id)
	if !isPane || region != layout.PaneRegion(p.ID()) {
		return false
	}
	if kind == zones.PaneKindTerm {
		// The wheel belongs to the inner app ONLY when it has enabled mouse
		// reporting (tmux semantics): otherwise it falls through to handleWheel so
		// the wheel scrolls the pane scrollback instead of being swallowed by a
		// program sitting at a prompt (#1024 wheel fix). Clicks/motion/release stay
		// forwarded regardless — this narrows the change to the wheel alone.
		if isWheelButton(msg.Button) && !lt.MouseTrackingEnabled() {
			return false
		}
		lt.SendMouse(msg, local.X, local.Y)
	}
	return true
}

// isWheelButton reports whether b is any mouse-wheel button. Wheel events are the
// only ones the interactive router hands back to the host when the inner app has
// not enabled mouse reporting (#1024 wheel fix).
func isWheelButton(b tea.MouseButton) bool {
	switch b {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown,
		tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight:
		return true
	}
	return false
}

// handleWheel scrolls the region UNDER THE CURSOR (RFC §2.5) — before this
// PR the wheel scrolled whatever the focus ring pointed at. Tree zones map
// to cursor movement (the tree's only scroll primitive, same as j/k), pane
// zones to that pane's capture scroll, the automations section to its task
// cursor. Modal overlays own the screen, so the wheel is inert under them.
func (m *home) handleWheel(msg tea.MouseMsg) tea.Cmd {
	if m.state != stateDefault {
		return nil
	}
	id, _, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		return nil
	}
	up := msg.Button == tea.MouseButtonWheelUp
	if region, _, isPane := zones.PaneZone(id); isPane {
		p, w := m.paneByRegion(region)
		if p == nil || w == nil || p.Instance() == nil {
			return nil
		}
		if up {
			w.ScrollUp()
		} else {
			w.ScrollDown()
		}
		// Scroll entry no longer captures on the event loop (#1637); dispatch an
		// off-loop refresh so the scrollback fill lands within a frame instead of
		// waiting up to a preview tick. panesRefresh bypasses its throttle while
		// the pane NeedsScrollFill.
		return m.panesRefresh(m.attached.Load())
	}
	switch {
	case strings.HasPrefix(id, "tree:"):
		if up {
			m.sidebar.Up()
		} else {
			m.sidebar.Down()
		}
		m.focusTreeForNav()
		return m.selectionChanged()
	case strings.HasPrefix(id, "auto:"):
		if up {
			m.automations.ScrollUp()
		} else {
			m.automations.ScrollDown()
		}
	}
	return nil
}

// handleClick dispatches a left press by zone id — the §2.5 gesture table.
func (m *home) handleClick(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	id, _, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		return m, nil
	}

	// Modal states: the overlay owns the screen, so only its buttons (and,
	// while naming, the status-bar hints that ARE the form's verbs) react.
	// Track the click AFTER this gate so a swallowed modal click never seeds
	// the double-click tracker — otherwise a click on the same zone within the
	// window after the modal closes reads as a false double click (#1731).
	if m.state != stateDefault {
		return m.handleModalClick(id)
	}

	double := m.trackClick(id)

	switch id {
	case zones.TreeHeader:
		// Click the Instances header: toggle it, like Enter on the row.
		m.sidebar.ClickHeader()
		m.focusRegionClick(layout.RegionTree)
		return m, m.selectionChanged()
	case zones.TreeHeaderArchived:
		// Click the Archived folder header (#1028): toggle the Archived folder
		// specifically, not the Instances section.
		m.sidebar.ClickHeaderKind(ui.SectionArchived)
		m.focusRegionClick(layout.RegionTree)
		return m, m.selectionChanged()
	case zones.TreeBG:
		m.focusRegionClick(layout.RegionTree)
		return m, nil
	case zones.AutoBG:
		m.focusRegionClick(layout.RegionAutomations)
		return m, nil
	case zones.ProjectsBG:
		// Click the Projects section background (not a row): focus the section,
		// like Tabbing into it.
		m.focusRegionClick(layout.RegionProjects)
		return m, nil
	}

	if title, ok := zones.TreeArrowTitle(id); ok {
		// Click ▸/▾: expand/collapse the instance's tab children.
		m.sidebar.ToggleInstanceTree(title)
		m.focusRegionClick(layout.RegionTree)
		return m, m.selectionChanged()
	}
	if title, ok := zones.TreeInstanceTitle(id); ok {
		// Click an instance row: select (retargets the workspace binding);
		// double-click interacts, exactly like Enter on the selected row.
		m.focusRegionClick(layout.RegionTree)
		if inst := m.store.GetInstanceByTitle(title); inst != nil {
			m.sidebar.SelectInstance(inst)
		}
		if double {
			selCmd := m.selectionChanged()
			_, enterCmd := m.handleEnter()
			return m, tea.Batch(selCmd, enterCmd)
		}
		return m, m.selectionChanged()
	}
	if title, idx, ok := zones.TreeTabParts(id); ok {
		return m, m.handleTreeTabClick(title, idx, double)
	}
	if root, ok := zones.ProjectRoot(id); ok {
		// Click a Projects-section row: focus the section and switch the rail to
		// that project (the row's primary action, like Enter on it).
		m.focusRegionClick(layout.RegionProjects)
		m.projects.SelectByRoot(root)
		return m.switchToProjectRoot(root)
	}
	if region, kind, ok := zones.PaneZone(id); ok {
		// Click a pane header (or an unfocused pane's body): focus that pane
		// — how the mouse picks one of the N panes. Click the FOCUSED pane's
		// body: enter it, exactly like Enter (§2.5).
		p, _ := m.paneByRegion(region)
		if p == nil {
			return m, nil
		}
		if kind == zones.PaneKindHeader || m.ring.Active() != region {
			m.focusRegionClick(region)
			return m, nil
		}
		// A click on the already-focused pane's body enters THAT pane — the
		// click named it, so enter it directly rather than the sidebar selection
		// keyboard Enter resolves (#1233, §2.5). No replay key: a click is not a
		// keystroke, so nothing is forwarded into the pane on entry (#1576).
		return m.enterPane(p, nil)
	}
	if taskID, ok := zones.AutoTaskID(id); ok {
		// Click a task row: focus the automations section and select the
		// task; double-click opens the task-manager overlay on it (§2.5).
		m.focusRegionClick(layout.RegionAutomations)
		if m.automations.SelectTaskByID(taskID) && double {
			return m.showTasksOverlay()
		}
		return m, nil
	}
	if key, ok := zones.StatusHintKey(id); ok {
		// Click a status-bar hint: press its key (the menu already knows its
		// bindings; the full handleKeyPress path keeps every gate identical).
		return m.handleHintClick(key)
	}
	return m, nil
}

func (m *home) handleTreeTabClick(title string, idx int, double bool) tea.Cmd {
	// Click a tab row: select it (drives the active tab); double-click
	// interacts with that tab.
	m.focusRegionClick(layout.RegionTree)
	m.sidebar.SelectTabRow(title, idx)
	if double {
		selCmd := m.selectionChanged()
		_, enterCmd := m.handleEnter()
		return tea.Batch(selCmd, enterCmd)
	}
	return m.selectionChanged()
}

func (m *home) dragStatusText() string {
	if m.tabDrag == nil || !m.tabDrag.active {
		return ""
	}
	label := m.tabDrag.label
	if label == "" {
		label = fmt.Sprintf("%s tab %d", m.tabDrag.title, m.tabDrag.tab+1)
	}
	if m.tabDrag.targetRegion == "" {
		return "Dragging… " + label
	}
	return "Dragging… " + label + " - drop to open pane"
}

// handleModalClick handles clicks while an overlay owns the screen: overlay
// buttons act as their keys; everything else is swallowed so a stray click
// can never mutate state behind a modal.
func (m *home) handleModalClick(id string) (tea.Model, tea.Cmd) {
	switch m.state {
	case stateConfirm:
		if m.confirmationOverlay == nil {
			return m, nil
		}
		var key string
		switch id {
		case zones.OverlayConfirmYes:
			key = m.confirmationOverlay.ConfirmKey
		case zones.OverlayConfirmNo:
			key = m.confirmationOverlay.CancelKey
		default:
			return m, nil
		}
		if msg, ok := keyMsgFromString(key); ok {
			return m.handleStateConfirm(msg)
		}
	case stateSelectProgram:
		if m.selectionOverlay == nil {
			return m, nil
		}
		if idx, ok := zones.OverlaySelectIdx(id); ok {
			m.selectionOverlay.SetSelectedIndex(idx)
			return m.handleStateSelectProgram(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case stateSelectTabKind:
		if m.selectionOverlay == nil {
			return m, nil
		}
		if idx, ok := zones.OverlaySelectIdx(id); ok {
			m.selectionOverlay.SetSelectedIndex(idx)
			return m.handleStateSelectTabKind(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case stateSearch:
		if m.searchOverlay == nil {
			return m, nil
		}
		if idx, ok := zones.OverlaySearchIdx(id); ok {
			m.searchOverlay.SetSelectedIndex(idx)
			return m.handleStateSearch(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case stateNew:
		// The naming form's status-bar hints (enter submit / tab change
		// program) stay clickable; handleKeyPress routes them to the form.
		if key, ok := zones.StatusHintKey(id); ok {
			return m.handleHintClick(key)
		}
	}
	return m, nil
}

// handleHintClick presses the key a status-bar hint advertises, through the
// full handleKeyPress path so state gating, interactive-mode routing (the
// ctrl+] hint), and the keydown highlight behave exactly as if the user had
// typed it.
func (m *home) handleHintClick(key string) (tea.Model, tea.Cmd) {
	msg, ok := keyMsgFromString(key)
	if !ok {
		return m, nil
	}
	return m.handleKeyPress(msg)
}

// clearStaleClickTrackerAfter drops the double-click tracker whenever this
// Update touched a non-default (modal/overlay) state — entering it, sitting in
// it, or leaving it. The tracker only ever pairs two clicks inside one
// uninterrupted stateDefault run; a modal excursion between them must not let a
// pre-modal press combine with a post-modal press into a false double click
// (#1731). A pure stateDefault→stateDefault Update leaves the tracker intact so
// genuine double clicks (which span two Update calls) still register.
func (m *home) clearStaleClickTrackerAfter(stateBefore state) {
	if stateBefore == stateDefault && m.state == stateDefault {
		return
	}
	m.lastClickZone = ""
	m.lastClickAt = time.Time{}
}

// trackClick records a press on zone id and reports whether it completed a
// double click. A completed double resets the tracker so a triple click
// can't read as two doubles.
func (m *home) trackClick(id string) bool {
	now := m.mouseClock()
	double := id == m.lastClickZone && now.Sub(m.lastClickAt) <= doubleClickInterval
	if double {
		m.lastClickZone = ""
		m.lastClickAt = time.Time{}
	} else {
		m.lastClickZone = id
		m.lastClickAt = now
	}
	return double
}

// focusRegionClick moves focus to region for a mouse click, mirroring the
// Tab-cycle contract (cycleFocus): the ring refuses hidden/unknown regions,
// and a click on the already-focused region changes nothing (so it can't
// trigger a needless relayout).
func (m *home) focusRegionClick(region string) {
	if m.ring.Active() == region {
		return
	}
	if !m.ring.Focus(region) {
		return
	}
	m.relayout()
}

// paneByRegion resolves a pane zone's region id back to the visible open
// pane and its window. Both nil when the pane vanished since the frame the
// zone was registered on (a click racing a close).
func (m *home) paneByRegion(region string) (*store.OpenPane, *ui.TabbedWindow) {
	for _, p := range m.visiblePanes {
		if layout.PaneRegion(p.ID()) == region {
			return p, m.paneWindows[p.ID()]
		}
	}
	return nil, nil
}

// keyMsgFromString synthesizes the tea.KeyMsg a click stands in for, from a
// binding's primary key string (keys.GlobalKeyBindings[…].Keys()[0]). ok is
// false for strings with no single-key equivalent.
func keyMsgFromString(s string) (tea.KeyMsg, bool) {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}, true
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}, true
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}, true
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}, true
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}, true
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}, true
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}, true
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}, true
	case "shift+up":
		return tea.KeyMsg{Type: tea.KeyShiftUp}, true
	case "shift+down":
		return tea.KeyMsg{Type: tea.KeyShiftDown}, true
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}, true
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}, true
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}, true
	case "ctrl+]":
		return tea.KeyMsg{Type: tea.KeyCtrlCloseBracket}, true
	}
	if r := []rune(s); len(r) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: r}, true
	}
	return tea.KeyMsg{}, false
}

// overlayOrigin computes where overlay.PlaceOverlay(0, 0, fg, bg, true) puts
// fg's top-left: centered on bg (or 0,0 when fg exceeds bg, matching
// PlaceOverlay's clamp). The overlays register their button zones relative
// to this origin.
func overlayOrigin(fg, bg string) layout.Point {
	fgW, fgH := lipgloss.Width(fg), lipgloss.Height(fg)
	bgW, bgH := lipgloss.Width(bg), lipgloss.Height(bg)
	x := (bgW - fgW) / 2
	y := (bgH - fgH) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return layout.Point{X: x, Y: y}
}
