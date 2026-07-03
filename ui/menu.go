package ui

import (
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"

	"github.com/charmbracelet/lipgloss"
)

var keyStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
	Light: "#655F5F",
	Dark:  "#7F7A7A",
})

var descStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
	Light: "#7A7474",
	Dark:  "#9C9494",
})

var sepStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{
	Light: "#DDDADA",
	Dark:  "#3C3C3C",
})

var actionGroupStyle = lipgloss.NewStyle().Foreground(AccentColor)

var separator = " • "
var verticalSeparator = " │ "

var menuStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("205"))

// MenuState represents different states the menu can be in
type MenuState int

const (
	StateDefault MenuState = iota
	StateEmpty
	StateNewInstance
)

// menuGroup defines a contiguous range of menu options that belong together.
type menuGroup struct {
	start    int
	end      int
	isAction bool // whether this group should render with actionGroupStyle
}

type Menu struct {
	options       []keys.KeyName
	groups        []menuGroup
	height, width int
	state         MenuState
	instance      *session.Instance
	activeTab     int

	// focusRegion is the focus-ring region the hints are rendered for
	// (layout.RegionTree / RegionPaneA / RegionAutomations). The status bar is
	// context-sensitive per RFC §2.1: hints follow focus, so the automations
	// strip advertises its manager keys while the tree/workspace show the
	// session actions.
	focusRegion string

	// keyDown is the key which is pressed. The default is -1.
	keyDown keys.KeyName
}

var defaultMenuOptions = []keys.KeyName{keys.KeyNew, keys.KeyNewRemote, keys.KeySearch, keys.KeyHelp, keys.KeyQuit}
var newInstanceMenuOptions = []keys.KeyName{keys.KeySubmitName, keys.KeyChangeProgram}

// automationsMenuOptions are the status-bar hints while the automations strip
// has focus; the expanded TaskPane renders its own detailed key line, so the
// bar shows only the cross-region verbs.
var automationsMenuOptions = []keys.KeyName{keys.KeyTab, keys.KeyHooks, keys.KeyHelp, keys.KeyQuit}

func NewMenu() *Menu {
	m := &Menu{
		options:   defaultMenuOptions,
		state:     StateEmpty,
		activeTab: 0,
		keyDown:   -1,
	}
	m.updateOptions()
	return m
}

func (m *Menu) Keydown(name keys.KeyName) {
	m.keyDown = name
}

func (m *Menu) ClearKeydownIfMatch(name keys.KeyName) {
	if m.keyDown == name {
		m.keyDown = -1
	}
}

// SetState updates the menu state and options accordingly
func (m *Menu) SetState(state MenuState) {
	m.state = state
	m.updateOptions()
}

// SetInstance updates the current instance and refreshes menu options
func (m *Menu) SetInstance(instance *session.Instance) {
	m.instance = instance
	// Only change the state if we're not in a special state (NewInstance or Prompt)
	if m.state != StateNewInstance {
		if m.instance != nil {
			m.state = StateDefault
		} else {
			m.state = StateEmpty
		}
	}
	m.updateOptions()
}

// SetFocusRegion switches the hints to the given focus-ring region (a
// layout.Region* id). The status bar is context-sensitive per focus (#1024
// PR 4): the automations strip gets its own option set; the tree and
// workspace share the instance/default sets driven by SetInstance.
func (m *Menu) SetFocusRegion(region string) {
	m.focusRegion = region
	m.updateOptions()
}

// SetActiveTab updates the currently active tab
func (m *Menu) SetActiveTab(tab int) {
	m.activeTab = tab
	m.updateOptions()
}

// updateOptions updates the menu options based on current state, focus
// region, and instance
func (m *Menu) updateOptions() {
	// The automations strip owns the hints while focused, regardless of the
	// selected instance — except during naming, whose submit/change-program
	// hints must always win (the form has the keyboard).
	if m.focusRegion == layout.RegionAutomations && m.state != StateNewInstance {
		m.options = automationsMenuOptions
		m.groups = []menuGroup{
			{start: 0, end: 1, isAction: true},
			{start: 1, end: len(automationsMenuOptions), isAction: false},
		}
		return
	}
	switch m.state {
	case StateEmpty:
		m.options = defaultMenuOptions
		// Groups: creation (n, N) | search (/) | system (?, q)
		m.groups = []menuGroup{
			{start: 0, end: 2, isAction: true},
			{start: 2, end: 3, isAction: false},
			{start: 3, end: 5, isAction: false},
		}
	case StateDefault:
		if m.instance != nil {
			// When there is an instance, show that instance's options
			m.addInstanceOptions()
		} else {
			// When there is no instance, show the empty state
			m.options = defaultMenuOptions
			m.groups = []menuGroup{
				{start: 0, end: 2, isAction: true},
				{start: 2, end: 3, isAction: false},
				{start: 3, end: 5, isAction: false},
			}
		}
	case StateNewInstance:
		m.options = newInstanceMenuOptions
		m.groups = []menuGroup{
			{start: 0, end: len(newInstanceMenuOptions), isAction: true},
		}
	}
}

func (m *Menu) addInstanceOptions() {
	// Loading instances only get minimal options
	if m.instance != nil && m.instance.GetStatus() == session.Loading {
		m.options = []keys.KeyName{keys.KeyNew, keys.KeyHelp, keys.KeyQuit}
		m.groups = []menuGroup{
			{start: 0, end: 3, isAction: false},
		}
		return
	}

	// Instance management group
	mgmtGroup := []keys.KeyName{keys.KeyNew, keys.KeyKill}

	// Action group
	actionGroup := []keys.KeyName{keys.KeyEnter}

	// Navigation group: every tab is a captured tmux session and supports
	// scroll mode (#930 PR 2 — the agent "Preview" tab and the terminal tab
	// both scroll), so the shift-scroll keys always show for an instance.
	actionGroup = append(actionGroup, keys.KeyShiftUp)
	actionGroup = append(actionGroup, keys.KeyShiftDown)

	// Tab group: create, close, and number-jump (#930 PR 4). The tab CYCLE key
	// is gone — Tab now cycles the focus ring (#1024 PR 4); tabs are reached
	// via the tree and the 1-9 jump keys. Remote instances block `t` (new tab)
	// and `w` (close tab) — those handlers reject IsRemote() with an error — so
	// only advertise the tab keys that actually work: number-jump (#988).
	tabGroup := []keys.KeyName{keys.KeyNewTab, keys.KeyCloseTab, keys.KeyJumpTab}
	if m.instance != nil && m.instance.IsRemote() {
		tabGroup = []keys.KeyName{keys.KeyJumpTab}
	}

	// System group: the focus-ring cycle plus help/quit.
	systemGroup := []keys.KeyName{keys.KeyTab, keys.KeyHelp, keys.KeyQuit}

	// Combine all groups and compute boundaries
	mgmtEnd := len(mgmtGroup)
	actionEnd := mgmtEnd + len(actionGroup)
	tabEnd := actionEnd + len(tabGroup)
	systemEnd := tabEnd + len(systemGroup)

	options := make([]keys.KeyName, 0, systemEnd)
	options = append(options, mgmtGroup...)
	options = append(options, actionGroup...)
	options = append(options, tabGroup...)
	options = append(options, systemGroup...)

	m.options = options
	m.groups = []menuGroup{
		{start: 0, end: mgmtEnd, isAction: false},
		{start: mgmtEnd, end: actionEnd, isAction: true},
		{start: actionEnd, end: tabEnd, isAction: false},
		{start: tabEnd, end: systemEnd, isAction: false},
	}
}

// SetSize sets the width of the window. The menu will be centered horizontally within this width.
func (m *Menu) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// hintDropOrder lists the options that may be dropped when the hint row is
// wider than the status bar, least valuable first; options in the same inner
// slice drop together (a lone "⇧↓ scroll" without its ⇧↑ twin reads like a
// bug). The full instance row is ~108 cells, so on narrow terminals something
// has to go — and before this priority existed the CLAMP decided, silently
// cutting the RIGHT edge, i.e. `? help` and `q quit` first: exactly the hints
// a lost user needs (#1083 play-test). Help and quit are deliberately absent
// from this list — they are never dropped — as are the naming-flow options
// (that row is short).
var hintDropOrder = [][]keys.KeyName{
	{keys.KeyShiftUp, keys.KeyShiftDown},
	{keys.KeyJumpTab},
	{keys.KeyCloseTab},
	{keys.KeyNewTab},
	{keys.KeySearch},
	{keys.KeyNewRemote},
	{keys.KeyHooks},
	{keys.KeyTab},
	{keys.KeyEnter},
	{keys.KeyKill},
	{keys.KeyNew},
}

func (m *Menu) String() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}

	// Render the full hint row; while it exceeds the bar width, drop options
	// in priority order and re-render. Whatever still doesn't fit after the
	// drop list is exhausted is clamped by the status bar as before.
	drop := make(map[keys.KeyName]bool)
	line := m.renderHints(drop)
	for _, ks := range hintDropOrder {
		if lipgloss.Width(line) <= m.width {
			break
		}
		for _, k := range ks {
			drop[k] = true
		}
		line = m.renderHints(drop)
	}

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, line)
}

// renderHints renders the option row, skipping dropped options. Separators
// follow group membership of the options actually rendered: a bullet within a
// group, a vertical bar between groups.
func (m *Menu) renderHints(drop map[keys.KeyName]bool) string {
	groupOf := func(i int) int {
		for gi, g := range m.groups {
			if i >= g.start && i < g.end {
				return gi
			}
		}
		return -1
	}

	var s strings.Builder
	prevGroup := -1
	first := true
	for i, k := range m.options {
		if drop[k] {
			continue
		}
		binding := keys.GlobalKeyBindings[k]

		var (
			localActionStyle = actionGroupStyle
			localKeyStyle    = keyStyle
			localDescStyle   = descStyle
		)
		if m.keyDown == k {
			localActionStyle = localActionStyle.Underline(true)
			localKeyStyle = localKeyStyle.Underline(true)
			localDescStyle = localDescStyle.Underline(true)
		}

		group := groupOf(i)
		inActionGroup := group >= 0 && m.groups[group].isAction

		if !first {
			if group != prevGroup {
				s.WriteString(sepStyle.Render(verticalSeparator))
			} else {
				s.WriteString(sepStyle.Render(separator))
			}
		}
		first = false
		prevGroup = group

		if inActionGroup {
			s.WriteString(localActionStyle.Render(binding.Help().Key))
			s.WriteString(" ")
			s.WriteString(localActionStyle.Render(binding.Help().Desc))
		} else {
			s.WriteString(localKeyStyle.Render(binding.Help().Key))
			s.WriteString(" ")
			s.WriteString(localDescStyle.Render(binding.Help().Desc))
		}
	}

	return menuStyle.Render(s.String())
}
