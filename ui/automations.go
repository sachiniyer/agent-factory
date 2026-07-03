package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

var automationsTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(AccentColor)

var automationsTitleDimStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.AdaptiveColor{Light: "#A49FA5", Dark: "#777777"})

var automationsEnabledStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#36CFC9"))

var automationsDisabledStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#9C9494"))

var automationsSelectedStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("#FFCC00"))

var automationsHintStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#7F7A7A"))

// fitLine truncates plain text to w cells, marking a cut with a trailing "…"
// (dropped when even the ellipsis cannot fit) — the same treatment the tree
// rows apply, replacing the bare hard cut ClampToRect would make.
func fitLine(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	tail := "…"
	if w < runewidth.StringWidth(tail) {
		tail = ""
	}
	return runewidth.Truncate(s, w, tail)
}

// AutomationsPane is the bottom section of the left rail (#1087 revised RFC
// §2.1's bottom strip): compact task rows — enabled glyph, name, trigger,
// next/last run — pinned under the instances tree behind a horizontal rule.
// The full TaskPane manager (list + edit/create form) is NOT hosted in the
// rail: it opens as a centered modal overlay (like the hooks editor), so its
// form is never clamped into the narrow rail. The pane OWNS the TaskPane the
// overlay hosts; the compact rows render straight off the store projection so
// the section and the manager can never show different task sets after a
// reload.
//
// It implements layout.Pane. While focused the compact rows carry a cursor
// (up/down/j/k via HandleKey); the root opens the manager overlay on Enter/S
// and moves the focus ring on Esc.
type AutomationsPane struct {
	proj     *store.Projection
	taskPane *TaskPane

	rect    layout.Rect
	compact bool
	focused bool

	// selected is the focused section's cursor over the projection's task
	// list (clamped on every read); offset is the scroll window start so the
	// cursor stays visible in the few in-rail rows.
	selected int
	offset   int

	// now returns the current time for next-run derivation; a fixed value in
	// tests so rendered "next" columns are deterministic.
	now func() time.Time
}

// NewAutomationsPane creates the section over the given projection.
func NewAutomationsPane(proj *store.Projection) *AutomationsPane {
	return &AutomationsPane{
		proj:     proj,
		taskPane: NewTaskPane(),
		now:      time.Now,
	}
}

// TaskPane returns the task manager this section owns (hosted by the root's
// tasks overlay).
func (a *AutomationsPane) TaskPane() *TaskPane {
	return a.taskPane
}

// SetRect implements layout.Pane.
func (a *AutomationsPane) SetRect(r layout.Rect) {
	a.rect = r
}

// SetCompact selects the 1-line summary rendering (degradation ladder <80
// cols / <20 rows, RFC §2.6).
func (a *AutomationsPane) SetCompact(compact bool) {
	a.compact = compact
}

// Focused implements layout.Pane.
func (a *AutomationsPane) Focused() bool { return a.focused }

// Focus implements layout.Pane: the section shows a cursor over its compact
// rows. The task manager itself opens as an overlay (root-driven), never
// in-rail.
func (a *AutomationsPane) Focus() {
	a.focused = true
}

// Blur implements layout.Pane.
func (a *AutomationsPane) Blur() {
	a.focused = false
}

// SelectedTaskIndex returns the cursor's task index (clamped; -1 when there
// are no tasks) — the task the manager overlay preselects on open.
func (a *AutomationsPane) SelectedTaskIndex() int {
	n := len(a.proj.GetTasks())
	if n == 0 {
		return -1
	}
	return clampInt(a.selected, 0, n-1)
}

// HandleKey implements layout.Pane: the focused section owns only its cursor
// movement; everything else (Enter → manager overlay, Esc → focus ring) is
// root-routed so it stays in one place.
func (a *AutomationsPane) HandleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if !a.focused {
		return nil, false
	}
	switch msg.String() {
	case "up", "k":
		a.ScrollUp()
		return nil, true
	case "down", "j":
		a.ScrollDown()
		return nil, true
	}
	return nil, false
}

// HandleMouse implements layout.Pane. Mouse support is #1024 PR 6.
func (a *AutomationsPane) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// ScrollUp moves the section cursor up (wheel/key routing).
func (a *AutomationsPane) ScrollUp() {
	if a.selected > 0 {
		a.selected--
	}
}

// ScrollDown moves the section cursor down.
func (a *AutomationsPane) ScrollDown() {
	if a.selected < len(a.proj.GetTasks())-1 {
		a.selected++
	}
}

// nextRunSummary derives the "next/last" column of a compact row: a cron
// task's next fire time (from its schedule), a watch task's supervision
// state, plus the last run when one is recorded.
func (a *AutomationsPane) nextRunSummary(tsk task.Task) string {
	var parts []string
	if tsk.IsWatch() {
		parts = append(parts, watchTaskStatus(tsk))
	} else if tsk.Enabled && tsk.CronExpr != "" {
		if sched, err := task.ParseCron(tsk.CronExpr); err == nil {
			parts = append(parts, "next "+sched.Next(a.now()).Format("Jan 02 15:04"))
		}
	}
	if tsk.LastRunAt != nil {
		parts = append(parts, "last "+tsk.LastRunAt.Format("Jan 02 15:04"))
	}
	return strings.Join(parts, " · ")
}

// compactRow renders one section row: enabled glyph, name, trigger,
// next/last — ellipsized to the rail width, with the focused cursor's row
// marked "▸".
func (a *AutomationsPane) compactRow(tsk task.Task, selected bool) string {
	glyph, style := "[✓]", automationsEnabledStyle
	if !tsk.Enabled {
		glyph, style = "[✗]", automationsDisabledStyle
	}
	marker := " "
	if selected {
		marker = "▸"
		style = automationsSelectedStyle
	}
	parts := []string{glyph}
	if tsk.Name != "" {
		parts = append(parts, tsk.Name)
	}
	trigger := tsk.CronExpr
	if tsk.IsWatch() {
		trigger = "watch: " + tsk.WatchCmd
	}
	if trigger != "" {
		parts = append(parts, trigger)
	}
	if next := a.nextRunSummary(tsk); next != "" {
		parts = append(parts, next)
	}
	return style.Render(fitLine(marker+strings.Join(parts, "  "), a.rect.W))
}

// enabledCount returns how many of the projection's tasks are enabled.
func (a *AutomationsPane) enabledCount() int {
	n := 0
	for _, tsk := range a.proj.GetTasks() {
		if tsk.Enabled {
			n++
		}
	}
	return n
}

// titleLine renders the section header width-aware: segments drop
// right-to-left ("H hooks" first, then the task counts) and finally the name
// shrinks with an ellipsis — the "S manage" affordance is the last thing cut,
// so the key to the manager stays visible even at the 22-col rail minimum
// (#1090 width).
func (a *AutomationsPane) titleLine(name string, nameStyle lipgloss.Style) string {
	w := a.rect.W
	const shortName = " Automations "
	const manage = "· S manage"
	const hooks = " · H hooks"
	if runewidth.StringWidth(name+manage+hooks) <= w {
		return nameStyle.Render(name) + automationsHintStyle.Render(manage+hooks)
	}
	if runewidth.StringWidth(name+manage) <= w {
		return nameStyle.Render(name) + automationsHintStyle.Render(manage)
	}
	if runewidth.StringWidth(shortName+manage) <= w {
		return nameStyle.Render(shortName) + automationsHintStyle.Render(manage)
	}
	avail := w - runewidth.StringWidth(manage)
	if avail < 2 {
		// Too narrow even for the affordance: ellipsize the whole line.
		return nameStyle.Render(fitLine(name+manage, w))
	}
	return nameStyle.Render(fitLine(shortName, avail)) + automationsHintStyle.Render(manage)
}

// View implements layout.Pane: exactly rect-sized.
func (a *AutomationsPane) View() string { return a.String() }

func (a *AutomationsPane) String() string {
	if a.rect.Empty() {
		return ""
	}

	tasks := a.proj.GetTasks()
	if a.selected >= len(tasks) {
		a.selected = len(tasks) - 1
	}
	if a.selected < 0 {
		a.selected = 0
	}

	// 1-line degraded summary (RFC §2.6, <80 cols).
	if a.compact || a.rect.H <= 1 {
		name := fmt.Sprintf(" Automations: %d (%d on) ", len(tasks), a.enabledCount())
		style := automationsTitleDimStyle
		if a.focused {
			style = automationsTitleStyle
		}
		return layout.ClampToRect(a.titleLine(name, style), a.rect)
	}

	nameStyle := automationsTitleDimStyle
	if a.focused {
		nameStyle = automationsTitleStyle
	}
	title := a.titleLine(fmt.Sprintf(" Automations (%d) ", len(tasks)), nameStyle)
	lines := []string{title}
	if len(tasks) == 0 {
		lines = append(lines, automationsDisabledStyle.Render(
			fitLine("  no tasks — press S, then n to create one", a.rect.W)))
	}

	// Window the rows around the cursor so a focused selection below the fold
	// scrolls into view instead of moving invisibly.
	visible := a.rect.H - 1
	if visible < 0 {
		visible = 0
	}
	if a.offset > a.selected {
		a.offset = a.selected
	}
	if visible > 0 && a.selected >= a.offset+visible {
		a.offset = a.selected - visible + 1
	}
	if max := len(tasks) - visible; a.offset > max {
		a.offset = max
	}
	if a.offset < 0 {
		a.offset = 0
	}
	for i := a.offset; i < len(tasks); i++ {
		if len(lines) >= a.rect.H {
			break
		}
		lines = append(lines, a.compactRow(tasks[i], a.focused && i == a.selected))
	}
	return layout.ClampToRect(strings.Join(lines, "\n"), a.rect)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
