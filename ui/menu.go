package ui

import (
	"math"
	"strings"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"

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
	// (layout.RegionTree / RegionAutomations / a layout.PaneRegion id). The
	// status bar is context-sensitive per RFC §2.1: hints follow focus, so the
	// automations strip advertises its manager keys, a focused pane its pane
	// verbs, while the tree shows the session actions.
	focusRegion string

	// interactive: every keystroke is forwarding into the focused pane's
	// terminal (#1089, RFC §2.3), so the bar shows only the escape hatch.
	interactive bool

	// splitPaneAvailable is true only while there is an active preview that
	// `S` can commit as another pane. Without that preview the key no-ops, so
	// the footer must not advertise it (#1419).
	splitPaneAvailable bool

	// keyDown is the key which is pressed. The default is -1.
	keyDown keys.KeyName

	// zones is the shared mouse hit-test registry (#1024 R4); String()
	// registers a clickable rect per rendered hint every frame. origin is the
	// menu's top-left on screen (the status-bar rect, via SetOrigin), since
	// the registry works in absolute cells.
	zones  *zones.Registry
	origin layout.Point
}

var defaultMenuOptions = []keys.KeyName{keys.KeyNew, keys.KeyNewRemote, keys.KeySearch, keys.KeyHelp, keys.KeyQuit}
var newInstanceMenuOptions = []keys.KeyName{keys.KeySubmitName, keys.KeyChangeProgram}

// automationsMenuOptions are the status-bar hints while the in-rail
// automations section has focus: Enter opens the task manager overlay (which
// renders its own detailed key line), so the bar shows the manage verb plus
// the cross-region ones.
var automationsMenuOptions = []keys.KeyName{
	keys.KeyManageAutomations, keys.KeyTab, keys.KeyHooks, keys.KeyHelp, keys.KeyQuit,
}

// interactiveMenuOptions is the whole bar while interactive (#1089, RFC
// §2.3): every other key — including these hints' own letters — forwards to
// the pane's terminal, so advertising anything else would be a lie.
var interactiveMenuOptions = []keys.KeyName{keys.KeyExitInteractive}

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

// SetInteractive switches the hints to (or back from) interactive mode: only
// the Ctrl-] escape hatch shows while keystrokes forward to the pane (#1089).
func (m *Menu) SetInteractive(on bool) {
	m.interactive = on
	m.updateOptions()
}

// SetSplitPaneAvailable controls whether the split-pane commit key is
// advertised. The key is still globally bound; this only keeps the footer
// honest when no preview exists to commit (#1419).
func (m *Menu) SetSplitPaneAvailable(available bool) {
	m.splitPaneAvailable = available
	m.updateOptions()
}

// updateOptions updates the menu options based on current state, focus
// region, and instance
func (m *Menu) updateOptions() {
	// Interactive mode outranks everything: the terminal owns the keyboard,
	// the bar owns nothing but the way out.
	if m.interactive {
		m.options = interactiveMenuOptions
		m.groups = []menuGroup{
			{start: 0, end: len(interactiveMenuOptions), isAction: true},
		}
		return
	}
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
	// A focused workspace pane advertises its own verbs (#1088):
	// attach/scroll on its binding, open another pane, hide this one. Same
	// naming-flow exception as the strip.
	if layout.IsPaneRegion(m.focusRegion) && m.state != StateNewInstance {
		m.setPaneFocusOptions()
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
	// Creating (Loading) instances only get minimal options
	if m.instance != nil && m.instance.IsCreating() {
		m.options = []keys.KeyName{keys.KeyNew, keys.KeyHelp, keys.KeyQuit}
		m.groups = []menuGroup{
			{start: 0, end: 3, isAction: false},
		}
		return
	}

	// Instance management group. The `a` verb is archive for live rows and
	// restore for Archived/Lost/Dead rows.
	mgmtGroup := []keys.KeyName{keys.KeyNew, keys.KeyKill, keys.KeyArchive}

	// Action group: enter interacts in-pane, o attaches full-screen (#1089).
	actionGroup := []keys.KeyName{keys.KeyEnter, keys.KeyAttach}

	// Navigation group: every tab is a captured tmux session and supports
	// scroll mode (#930 PR 2 — the Agent tab and the terminal tab
	// both scroll), so the scroll keys always show for an instance.
	actionGroup = append(actionGroup, keys.KeyShiftUp)
	actionGroup = append(actionGroup, keys.KeyShiftDown)

	// Usage-limit retry (#1146): advertised only when the selected session is
	// actually blocked at a limit wall — c re-spawns (if the agent exited) and
	// resumes it. Kept off the bar for every normal session so it never clutters
	// the hints.
	if m.instance != nil && m.instance.LimitReached() {
		actionGroup = append(actionGroup, keys.KeyLimitRetry)
	}

	// Tab group: create, close, and number-jump (#930 PR 4). The tab CYCLE key
	// is gone — Tab now cycles the focus ring (#1024 PR 4); tabs are reached
	// via the tree and the 1-9 jump keys. Remote instances block `t` (new tab)
	// and `w` (close tab) — those handlers reject IsRemote() with an error — so
	// only advertise the tab keys that actually work: number-jump (#988).
	tabGroup := []keys.KeyName{keys.KeyNewTab, keys.KeyCloseTab, keys.KeyJumpTab}
	if m.instance != nil && m.instance.IsRemote() {
		tabGroup = []keys.KeyName{keys.KeyJumpTab}
	}

	// Pane group (#1088/#1321): s opens the selected tab as a workspace pane
	// (or focuses its pane when already open); S commits a preview alongside
	// only while one exists (#1419).
	paneGroup := []keys.KeyName{keys.KeyOpenPane}
	if m.splitPaneAvailable {
		paneGroup = append(paneGroup, keys.KeySplitPane)
	}

	// System group: the focus-ring cycle plus help/quit.
	systemGroup := []keys.KeyName{keys.KeyTab, keys.KeyHelp, keys.KeyQuit}

	// Combine all groups and compute boundaries
	mgmtEnd := len(mgmtGroup)
	actionEnd := mgmtEnd + len(actionGroup)
	tabEnd := actionEnd + len(tabGroup)
	paneEnd := tabEnd + len(paneGroup)
	systemEnd := paneEnd + len(systemGroup)

	options := make([]keys.KeyName, 0, systemEnd)
	options = append(options, mgmtGroup...)
	options = append(options, actionGroup...)
	options = append(options, tabGroup...)
	options = append(options, paneGroup...)
	options = append(options, systemGroup...)

	m.options = options
	m.groups = []menuGroup{
		{start: 0, end: mgmtEnd, isAction: false},
		{start: mgmtEnd, end: actionEnd, isAction: true},
		{start: actionEnd, end: tabEnd, isAction: false},
		{start: tabEnd, end: paneEnd, isAction: false},
		{start: paneEnd, end: systemEnd, isAction: false},
	}
}

func (m *Menu) setPaneFocusOptions() {
	// Action group: enter interacts in-pane / o attaches full-screen (#1089),
	// and scroll acts on this pane's binding.
	actionGroup := []keys.KeyName{keys.KeyEnter, keys.KeyAttach, keys.KeyShiftUp, keys.KeyShiftDown}
	focusGroup := []keys.KeyName{keys.KeyPanePrev, keys.KeyPaneNext}
	paneGroup := []keys.KeyName{keys.KeyOpenPane}
	if m.splitPaneAvailable {
		paneGroup = append(paneGroup, keys.KeySplitPane)
	}
	paneGroup = append(paneGroup, keys.KeyHidePane)
	systemGroup := []keys.KeyName{keys.KeyTab, keys.KeyHelp, keys.KeyQuit}

	actionEnd := len(actionGroup)
	focusEnd := actionEnd + len(focusGroup)
	paneEnd := focusEnd + len(paneGroup)
	systemEnd := paneEnd + len(systemGroup)

	options := make([]keys.KeyName, 0, systemEnd)
	options = append(options, actionGroup...)
	options = append(options, focusGroup...)
	options = append(options, paneGroup...)
	options = append(options, systemGroup...)

	m.options = options
	m.groups = []menuGroup{
		{start: 0, end: actionEnd, isAction: true},
		{start: actionEnd, end: focusEnd, isAction: false},
		{start: focusEnd, end: paneEnd, isAction: false},
		{start: paneEnd, end: systemEnd, isAction: false},
	}
}

// SetSize sets the width of the window. The menu will be centered horizontally within this width.
func (m *Menu) SetSize(width, height int) {
	m.width = width
	m.height = height
}

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (m *Menu) SetZoneRegistry(reg *zones.Registry) {
	m.zones = reg
}

// SetOrigin records the menu's top-left screen cell for zone registration.
func (m *Menu) SetOrigin(p layout.Point) {
	m.origin = p
}

// centerStart is where centered content of the given size starts inside a
// box: lipgloss.Place(…, Center, …) computes left = gap - round(gap·0.5), and
// the hint zones must land on exactly the cells Place put the hints on.
func centerStart(box, content int) int {
	gap := box - content
	if gap <= 0 {
		return 0
	}
	return gap - int(math.Round(float64(gap)*0.5))
}

// hintDropOrder lists the options that may be dropped when the hint row is
// wider than the status bar, least valuable first; options in the same inner
// slice drop together (a lone "ctrl+d scroll" without its ctrl+u twin reads like a
// bug). The full instance row is ~108 cells, so on narrow terminals something
// has to go — and before this priority existed the CLAMP decided, silently
// cutting the RIGHT edge, i.e. `? help` and `q quit` first: exactly the hints
// a lost user needs (#1083 play-test). Help and quit are deliberately absent
// from this list — they are never dropped — as are the naming-flow options
// (that row is short).
var hintDropOrder = [][]keys.KeyName{
	{keys.KeyShiftUp, keys.KeyShiftDown},
	{keys.KeyAttach},
	{keys.KeySearch},
	{keys.KeyNewRemote},
	{keys.KeyHooks},
	{keys.KeyArchive},
	{keys.KeyKill},
	{keys.KeyNew},
	{keys.KeyEnter},
	{keys.KeyPanePrev, keys.KeyPaneNext},
	{keys.KeyOpenPane, keys.KeySplitPane, keys.KeyHidePane},
	{keys.KeyJumpTab},
	{keys.KeyCloseTab},
	{keys.KeyNewTab},
	{keys.KeyTab},
}

func (m *Menu) String() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}

	// Render the full hint row; while it exceeds the bar width, drop options
	// in priority order and re-render. Whatever still doesn't fit after the
	// drop list is exhausted is clamped by the status bar as before.
	drop := make(map[keys.KeyName]bool)
	line, spans := m.renderHints(drop)
	for _, ks := range hintDropOrder {
		if lipgloss.Width(line) <= m.width {
			break
		}
		for _, k := range ks {
			drop[k] = true
		}
		line, spans = m.renderHints(drop)
	}

	// Register a click zone per surviving hint (#1024 R4), on the exact
	// cells lipgloss.Place is about to center the row onto.
	if m.zones != nil {
		x0 := m.origin.X + centerStart(m.width, lipgloss.Width(line))
		y := m.origin.Y + centerStart(m.height, 1)
		for _, span := range spans {
			m.zones.Register(zones.StatusHint(span.key),
				layout.Rect{X: x0 + span.x, Y: y, W: span.w, H: 1})
		}
	}

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, line)
}

// hintSpan is one rendered hint's horizontal extent within the (un-centered)
// hint row: x is the printable-cell offset where its key text starts, w spans
// through the end of its description. key is the binding's primary key
// string — the StatusHint zone id payload, i.e. what a click "presses".
type hintSpan struct {
	key  string
	x, w int
}

// renderHints renders the option row, skipping dropped options, and reports
// each rendered hint's cell extent for the click zones. Separators follow
// group membership of the options actually rendered: a bullet within a group,
// a vertical bar between groups.
func (m *Menu) renderHints(drop map[keys.KeyName]bool) (string, []hintSpan) {
	groupOf := func(i int) int {
		for gi, g := range m.groups {
			if i >= g.start && i < g.end {
				return gi
			}
		}
		return -1
	}

	var s strings.Builder
	var spans []hintSpan
	col := 0
	write := func(chunk string) {
		s.WriteString(chunk)
		col += lipgloss.Width(chunk)
	}
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
				write(sepStyle.Render(verticalSeparator))
			} else {
				write(sepStyle.Render(separator))
			}
		}
		first = false
		prevGroup = group

		start := col
		if inActionGroup {
			write(localActionStyle.Render(binding.Help().Key))
			write(" ")
			write(localActionStyle.Render(binding.Help().Desc))
		} else {
			write(localKeyStyle.Render(binding.Help().Key))
			write(" ")
			write(localDescStyle.Render(binding.Help().Desc))
		}
		// KeyJumpTab's "1-9" chip names nine keys, not one action — it gets
		// no click zone. Everything else is clickable by its primary key.
		if k != keys.KeyJumpTab {
			if bkeys := binding.Keys(); len(bkeys) > 0 {
				spans = append(spans, hintSpan{key: bkeys[0], x: start, w: col - start})
			}
		}
	}

	return menuStyle.Render(s.String()), spans
}
