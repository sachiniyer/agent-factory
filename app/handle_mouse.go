package app

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// This file is the root mouse router (#1024 PR 6, RFC §2.5 — closes #1025).
// Every tea.MouseMsg is resolved to a zone id through the registry the last
// View() rebuilt, then dispatched to the same handlers the keyboard drives —
// the mouse is purely additive and the keyboard remains fully sufficient.
//
// While ATTACHED none of this runs: bubbletea's Update loop is blocked on
// `<-ch` and tmux owns stdin, so tmux's own mouse mode applies (§2.5) —
// unchanged by this PR.

// doubleClickInterval is how close two presses on the same zone must land to
// read as a double click (attach on tree rows, open editor on task rows).
const doubleClickInterval = 400 * time.Millisecond

// wireZoneRegistry hands the shared registry to every zone-producing pane.
// Called once at construction (newHome / test wiring).
func (m *home) wireZoneRegistry() {
	m.sidebar.SetZoneRegistry(m.zones)
	m.paneA.SetZoneRegistry(m.zones)
	m.paneB.SetZoneRegistry(m.zones)
	m.automations.SetZoneRegistry(m.zones)
	m.menu.SetZoneRegistry(m.zones)
}

// handleMouse routes a mouse event: wheel scrolls the region under the
// cursor, a left press clicks the zone under it. Motion and release events
// are ignored (cell-motion mode reports them, but every gesture here acts on
// the press).
func (m *home) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
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

// handleWheel scrolls the region UNDER THE CURSOR (RFC §2.5) — before this PR
// the wheel scrolled the content pane regardless of position. Tree zones map
// to cursor movement (the tree's only scroll primitive, same as j/k), pane
// zones to that pane's capture scroll, the automations strip to its task
// selection. Modal overlays own the screen, so the wheel is inert under them.
func (m *home) handleWheel(msg tea.MouseMsg) tea.Cmd {
	if m.state != stateDefault {
		return nil
	}
	id, _, ok := m.zones.Resolve(msg.X, msg.Y)
	if !ok {
		return nil
	}
	up := msg.Button == tea.MouseButtonWheelUp
	switch {
	case strings.HasPrefix(id, "tree:"):
		if up {
			m.sidebar.Up()
		} else {
			m.sidebar.Down()
		}
		return m.selectionChanged()
	case strings.HasPrefix(id, layout.RegionPaneA+":"):
		if m.store.GetSelectedInstance() == nil {
			return nil
		}
		if up {
			m.paneA.ScrollUp()
		} else {
			m.paneA.ScrollDown()
		}
	case strings.HasPrefix(id, layout.RegionPaneB+":"):
		if m.store.PaneBInstance() == nil {
			return nil
		}
		if up {
			m.paneB.ScrollUp()
		} else {
			m.paneB.ScrollDown()
		}
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
		// Click the section header: toggle it, like Enter on the row.
		m.sidebar.ClickHeader()
		return m, tea.Batch(m.focusRegionWithSave(layout.RegionTree), m.selectionChanged())
	case zones.TreeBG:
		return m, m.focusRegionWithSave(layout.RegionTree)
	case zones.AutoStrip:
		return m, m.focusRegionWithSave(layout.RegionAutomations)
	}

	if title, ok := zones.TreeArrowTitle(id); ok {
		// Click ▸/▾: expand/collapse the instance's tab children.
		m.sidebar.ToggleInstanceTree(title)
		return m, tea.Batch(m.focusRegionWithSave(layout.RegionTree), m.selectionChanged())
	}
	if title, ok := zones.TreeInstanceTitle(id); ok {
		// Click an instance row: select (retargets pane A); double-click
		// attaches, exactly like Enter on the selected row.
		focusCmd := m.focusRegionWithSave(layout.RegionTree)
		if inst := m.store.GetInstanceByTitle(title); inst != nil {
			m.sidebar.SelectInstance(inst)
		}
		if double {
			_, attachCmd := m.handleEnter()
			return m, tea.Batch(focusCmd, m.selectionChanged(), attachCmd)
		}
		return m, tea.Batch(focusCmd, m.selectionChanged())
	}
	if title, idx, ok := zones.TreeTabParts(id); ok {
		// Click a tab row: select it (drives pane A's active tab); double-
		// click attaches that tab.
		focusCmd := m.focusRegionWithSave(layout.RegionTree)
		m.sidebar.SelectTabRow(title, idx)
		m.menu.SetActiveTab(m.paneA.GetActiveTab())
		if double {
			_, attachCmd := m.handleEnter()
			return m, tea.Batch(focusCmd, m.selectionChanged(), attachCmd)
		}
		return m, tea.Batch(focusCmd, m.selectionChanged())
	}
	if region, header, ok := zones.PaneRegion(id); ok &&
		(region == layout.RegionPaneA || region == layout.RegionPaneB) {
		// Click a pane header (or an unfocused pane's body): focus that pane
		// — in a split this is how the mouse picks A or B. Click the FOCUSED
		// pane's body: attach, exactly like Enter (§2.5).
		if header || m.ring.Active() != region {
			return m, m.focusRegionWithSave(region)
		}
		return m.handleEnter()
	}
	if taskID, ok := zones.AutoTaskID(id); ok {
		// Click a task row: focus the automations strip and select the task;
		// double-click opens its editor.
		focusCmd := m.focusRegionWithSave(layout.RegionAutomations)
		sp := m.automations.TaskPane()
		if sp.SelectTaskByID(taskID) && double {
			sp.StartEditSelected()
		}
		return m, focusCmd
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
// full handleKeyPress path so state gating and the keydown highlight behave
// exactly as if the user had typed it.
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

// focusRegionWithSave moves focus to region for a mouse click, mirroring
// cycleFocus's contract: leaving the automations strip persists dirty task
// edits, and a failed save surfaces in the error box instead of dropping the
// edit silently. No-ops when the region already has focus or is hidden by
// the degradation ladder.
func (m *home) focusRegionWithSave(region string) tea.Cmd {
	leaving := m.ring.Active()
	if leaving == region || !m.ring.Focus(region) {
		return nil
	}
	var cmd tea.Cmd
	if leaving == layout.RegionAutomations {
		if err := m.saveContentPaneState(); err != nil {
			cmd = m.handleError(err)
		}
	}
	m.relayout()
	return cmd
}

// keyMsgFromString synthesizes the tea.KeyMsg a click stands in for, from a
// binding's primary key string (keys.GlobalKeyBindings […].Keys()[0]). ok is
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
	}
	if r := []rune(s); len(r) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: r}, true
	}
	return tea.KeyMsg{}, false
}

// overlayOrigin computes where overlay.PlaceOverlay(0, 0, fg, bg, true) puts
// fg's top-left: centered on bg (or 0,0 when fg exceeds bg, where
// PlaceOverlay returns fg alone). The overlays register their button zones
// relative to this origin.
func overlayOrigin(fg, bg string) layout.Point {
	fgW, fgH := lipgloss.Width(fg), lipgloss.Height(fg)
	bgW, bgH := lipgloss.Width(bg), lipgloss.Height(bg)
	if fgW > bgW || fgH > bgH {
		return layout.Point{}
	}
	return layout.Point{X: (bgW - fgW) / 2, Y: (bgH - fgH) / 2}
}
