package ui

import (
	"github.com/sachiniyer/agent-factory/keys"
	"strings"

	"github.com/sachiniyer/agent-factory/session"

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

var actionGroupStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("99"))

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

	// keyDown is the key which is pressed. The default is -1.
	keyDown keys.KeyName
}

var defaultMenuOptions = []keys.KeyName{keys.KeyNew, keys.KeyNewRemote, keys.KeyAttach, keys.KeySearch, keys.KeyHelp, keys.KeyQuit}
var newInstanceMenuOptions = []keys.KeyName{keys.KeySubmitName, keys.KeyChangeProgram}

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

func (m *Menu) ClearKeydown() {
	m.keyDown = -1
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

// SetSidebarContext updates menu options based on sidebar selection context.
func (m *Menu) SetSidebarContext(sectionKind SidebarSectionKind, isHeader bool) {
	if m.state == StateNewInstance {
		return
	}
	// For instance items, use the normal instance-based menu
	if sectionKind == SectionInstances && !isHeader && m.instance != nil {
		m.state = StateDefault
		m.updateOptions()
		return
	}
	// For non-instance selections, show the empty/default menu
	m.state = StateEmpty
	m.updateOptions()
}

// SetActiveTab updates the currently active tab
func (m *Menu) SetActiveTab(tab int) {
	m.activeTab = tab
	m.updateOptions()
}

// updateOptions updates the menu options based on current state and instance
func (m *Menu) updateOptions() {
	switch m.state {
	case StateEmpty:
		m.options = defaultMenuOptions
		// Groups: creation (n, N, a) | search (/) | system (?, q)
		m.groups = []menuGroup{
			{start: 0, end: 3, isAction: true},
			{start: 3, end: 4, isAction: false},
			{start: 4, end: 6, isAction: false},
		}
	case StateDefault:
		if m.instance != nil {
			// When there is an instance, show that instance's options
			m.addInstanceOptions()
		} else {
			// When there is no instance, show the empty state
			m.options = defaultMenuOptions
			m.groups = []menuGroup{
				{start: 0, end: 3, isAction: true},
				{start: 3, end: 4, isAction: false},
				{start: 4, end: 6, isAction: false},
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
	if m.instance != nil && m.instance.Status == session.Loading {
		m.options = []keys.KeyName{keys.KeyNew, keys.KeyHelp, keys.KeyQuit}
		m.groups = []menuGroup{
			{start: 0, end: 3, isAction: false},
		}
		return
	}

	// Instance management group
	mgmtGroup := []keys.KeyName{keys.KeyNew, keys.KeyAttach, keys.KeyKill}

	// Action group
	actionGroup := []keys.KeyName{keys.KeyEnter}

	// Navigation group (when in terminal tab)
	if m.activeTab == TerminalTab {
		actionGroup = append(actionGroup, keys.KeyShiftUp)
	}

	// System group
	systemGroup := []keys.KeyName{keys.KeyTab, keys.KeyHelp, keys.KeyQuit}

	// Combine all groups and compute boundaries
	mgmtEnd := len(mgmtGroup)
	actionEnd := mgmtEnd + len(actionGroup)
	systemEnd := actionEnd + len(systemGroup)

	options := make([]keys.KeyName, 0, systemEnd)
	options = append(options, mgmtGroup...)
	options = append(options, actionGroup...)
	options = append(options, systemGroup...)

	m.options = options
	m.groups = []menuGroup{
		{start: 0, end: mgmtEnd, isAction: false},
		{start: mgmtEnd, end: actionEnd, isAction: true},
		{start: actionEnd, end: systemEnd, isAction: false},
	}
}

// SetSize sets the width of the window. The menu will be centered horizontally within this width.
func (m *Menu) SetSize(width, height int) {
	m.width = width
	m.height = height
}

func (m *Menu) String() string {
	var s strings.Builder

	for i, k := range m.options {
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

		inActionGroup := false
		for _, g := range m.groups {
			if g.isAction && i >= g.start && i < g.end {
				inActionGroup = true
				break
			}
		}

		if inActionGroup {
			s.WriteString(localActionStyle.Render(binding.Help().Key))
			s.WriteString(" ")
			s.WriteString(localActionStyle.Render(binding.Help().Desc))
		} else {
			s.WriteString(localKeyStyle.Render(binding.Help().Key))
			s.WriteString(" ")
			s.WriteString(localDescStyle.Render(binding.Help().Desc))
		}

		// Add appropriate separator
		if i != len(m.options)-1 {
			isGroupEnd := false
			for _, g := range m.groups {
				if i == g.end-1 {
					s.WriteString(sepStyle.Render(verticalSeparator))
					isGroupEnd = true
					break
				}
			}
			if !isGroupEnd {
				s.WriteString(sepStyle.Render(separator))
			}
		}
	}

	centeredMenuText := menuStyle.Render(s.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, centeredMenuText)
}
