package app

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"

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

// wireZoneRegistry hands the shared registry to every long-lived
// zone-producing pane. Called once at construction (newHome / test wiring);
// pane windows are wired at creation (openPaneWindow) instead, since they
// come and go with the open-pane list.
func (m *home) wireZoneRegistry() {
	m.sidebar.SetZoneRegistry(m.zones)
	m.automations.SetZoneRegistry(m.zones)
	m.menu.SetZoneRegistry(m.zones)
}

// handleMouse routes a mouse event: wheel scrolls the region under the
// cursor, a left press clicks the zone under it, and interactive mode
// forwards in-pane events to the embedded terminal. Release/motion events
// matter only to the forwarding path (drags, button-up); host gestures all
// act on the press.
func (m *home) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
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

// forwardInteractiveMouse implements the RFC §2.5 in-pane ownership rule
// while interactive: any event over the live pane's rect belongs to the
// embedded terminal — grid-cell events forward through the emulator's
// mode-aware SendMouse (translated to pane-local coordinates by the zone
// resolve), and events over the pane's frame/header are suppressed so a
// stray click can never trigger a host action under the user's typing.
// Reports whether the event was consumed (forwarded or suppressed).
func (m *home) forwardInteractiveMouse(msg tea.MouseMsg) bool {
	if m.liveTerm == nil || m.livePane == nil {
		return false
	}
	id, local, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		// A miss is outside every pane; nothing to do either way.
		return true
	}
	region, kind, isPane := zones.PaneZone(id)
	if !isPane || region != layout.PaneRegion(m.livePane.ID()) {
		return false
	}
	if kind == zones.PaneKindTerm {
		m.liveTerm.SendMouse(msg, local.X, local.Y)
	}
	return true
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
		return nil
	}
	switch {
	case strings.HasPrefix(id, "tree:"):
		if up {
			m.sidebar.Up()
		} else {
			m.sidebar.Down()
		}
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
	double := m.trackClick(id)

	// Modal states: the overlay owns the screen, so only its buttons (and,
	// while naming, the status-bar hints that ARE the form's verbs) react.
	if m.state != stateDefault {
		return m.handleModalClick(id)
	}

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
		// Click a tab row: select it (drives the active tab); double-click
		// interacts with that tab.
		m.focusRegionClick(layout.RegionTree)
		m.sidebar.SelectTabRow(title, idx)
		if double {
			selCmd := m.selectionChanged()
			_, enterCmd := m.handleEnter()
			return m, tea.Batch(selCmd, enterCmd)
		}
		return m, m.selectionChanged()
	}
	if region, kind, ok := zones.PaneZone(id); ok {
		// Click a pane header (or an unfocused pane's body): focus that pane
		// — how the mouse picks one of the N panes. Click the FOCUSED pane's
		// body: enter it, exactly like Enter (§2.5).
		if p, _ := m.paneByRegion(region); p == nil {
			return m, nil
		}
		if kind == zones.PaneKindHeader || m.ring.Active() != region {
			m.focusRegionClick(region)
			return m, nil
		}
		return m.handleEnter()
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
