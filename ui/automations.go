package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
	"github.com/sachiniyer/agent-factory/ui/store"
	"github.com/sachiniyer/agent-factory/ui/tree"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

var automationsTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(AccentColor)

var automationsTitleDimStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(activeTheme.ForegroundMuted)

var automationsEnabledStyle = lipgloss.NewStyle().
	Foreground(activeTheme.Info)

var automationsDisabledStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

// automationItemTitleStyle paints an automation's title in the SAME adaptive
// color the instances tree uses for instance titles (tree.InstanceTitleColor),
// so the automations rail and the instance list above it read as one stacked
// list rather than two differently-colored ones (#1126).
var automationItemTitleStyle = lipgloss.NewStyle().
	Foreground(tree.InstanceTitleColor)

// automationDetailStyle renders an expanded row's cron/watch/status detail
// line — the recede gray the tree uses for its branch/description lines, so the
// detail reads as secondary to the title it hangs under (#1126).
var automationDetailStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundMuted)

var automationsHintStyle = lipgloss.NewStyle().
	Foreground(activeTheme.ForegroundDim)

// AutomationsPane is the bottom section of the left rail (#1087 revised RFC
// §2.1's bottom strip): one row per task — the enabled glyph and the task
// title in the instances-list title color — pinned under the instances tree
// behind a horizontal rule. Rows are collapsed by default (title only, #1126
// dropped the always-on trailing cron/next/last text); the focused cursor's
// row expands to reveal its trigger and next/last-run detail on a dim indented
// line beneath the title. The full TaskPane manager (list + edit/create form)
// is NOT hosted in the rail: it opens as a centered modal overlay (like the
// hooks editor), so its form is never clamped into the narrow rail. The pane
// OWNS the TaskPane the overlay hosts; the rows render straight off the store
// projection so the section and the manager can never show different task sets
// after a reload.
//
// It implements layout.Pane. While focused the rows carry a cursor
// (up/down/j/k via HandleKey); the root opens the manager overlay on Enter or
// the global task-manager key and moves the focus ring on Esc.
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

	// zones is the shared mouse hit-test registry (#1024 R4); String()
	// registers the section plus its task rows every frame. Nil skips.
	zones *zones.Registry
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

// HandleMouse implements layout.Pane. Mouse dispatch is zone-id-based at the
// root (#1024 R4): the section/task-row zones registered by String() resolve
// to focus/select actions there, so the pane-local fallback consumes nothing.
func (a *AutomationsPane) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// SetZoneRegistry wires the shared mouse hit-test registry (#1024 R4).
func (a *AutomationsPane) SetZoneRegistry(reg *zones.Registry) {
	a.zones = reg
}

// SelectTaskByID moves the section cursor onto the task with the given id —
// the click action for a task row. Reports whether the task was found.
func (a *AutomationsPane) SelectTaskByID(id string) bool {
	for i, tsk := range a.proj.GetTasks() {
		if tsk.ID == id {
			a.selected = i
			return true
		}
	}
	return false
}

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

// itemPrefixWidth is the fixed lead of a collapsed row — marker (1) + the
// enabled glyph "[✓]" (3) + a 2-cell gap — that the title starts after and the
// expanded detail line indents to, so a row's detail aligns under its title.
const itemPrefixWidth = 6

// titleRow renders one collapsed automation row: the enabled/disabled glyph and
// the task title in the instance-title color — and nothing else (#1126: no
// always-on trailing cron/next/last text). The focused, expanded row is marked
// "▾" and its title bolded; every other row leads with a blank marker.
func (a *AutomationsPane) titleRow(tsk task.Task, expanded bool) string {
	glyph, glyphStyle := "[✓]", automationsEnabledStyle
	if !tsk.Enabled {
		glyph, glyphStyle = "[✗]", automationsDisabledStyle
	}
	marker := " "
	nameStyle := automationItemTitleStyle
	if expanded {
		marker = "▾"
		nameStyle = nameStyle.Bold(true)
	}
	name := tsk.Name
	if name == "" {
		name = "(unnamed)"
	}
	w := a.rect.W
	if w <= itemPrefixWidth {
		// Too narrow to split the styled segments cleanly: fall back to one
		// fitted plain line so the row never overflows the rail.
		return automationItemTitleStyle.Render(fitLine(marker+glyph+"  "+name, w))
	}
	return marker + glyphStyle.Render(glyph) + "  " +
		nameStyle.Render(fitLine(name, w-itemPrefixWidth))
}

// rowDetail is the text an expanded row reveals: the trigger (cron expression
// or watch command) and the next/last-run or supervision summary — the details
// that used to trail every collapsed row (#1126). Empty when a task has neither.
func (a *AutomationsPane) rowDetail(tsk task.Task) string {
	var parts []string
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
	return strings.Join(parts, " · ")
}

// detailRow renders the expanded row's detail as a dim line indented under the
// title, ellipsized to the rail width. Returns "" when the task has no detail.
func (a *AutomationsPane) detailRow(tsk task.Task) string {
	detail := a.rowDetail(tsk)
	if detail == "" {
		return ""
	}
	indent := strings.Repeat(" ", itemPrefixWidth)
	return automationDetailStyle.Render(fitLine(indent+detail, a.rect.W))
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

func automationHelpKey(name keys.KeyName) string {
	return keys.GlobalKeyBindings[name].Help().Key
}

func automationsManageHint() string {
	return "· " + automationHelpKey(keys.KeyTaskList) + " manage"
}

func automationsActionHint(name keys.KeyName, desc string) string {
	return automationHelpKey(name) + " " + desc
}

// titleLine renders the section header width-aware: segments drop right-to-left
// (hooks first, then the task counts) and finally the name shrinks with an
// ellipsis. The manage affordance is the last thing cut, so the key to the
// manager stays visible even at the 22-col rail minimum (#1090 width).
func (a *AutomationsPane) titleLine(name string, nameStyle lipgloss.Style) string {
	w := a.rect.W
	const shortName = " Automations "
	manage := automationsManageHint()
	hooks := " · " + automationsActionHint(keys.KeyHooks, "hooks")
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
	// The section's base zone: any click inside it that lands on no task row
	// focuses the automations region. Task rows register on top (later wins).
	if a.zones != nil {
		a.zones.Register(zones.AutoBG, a.rect)
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
			fitLine(fmt.Sprintf("  no tasks — press %s, then n to create one", automationHelpKey(keys.KeyTaskList)), a.rect.W)))
	}

	// Reserve the last rail row as a blank bottom margin so the workspace
	// frame's bottom border never abuts the section's last row (#1560), the way
	// the sidebar's leading blank row keeps the frame's TOP border off the
	// rail. The grid sizes every full-mode section at least layout.AutomationsRows
	// tall (floor / grow-to-content / half-cap all include this margin), so
	// reserving one row costs no visible capacity in the app. Guard the
	// reservation on the section being at least that floor tall: a direct caller
	// that hands the pane a tighter, content-exact rect (below any size the grid
	// ever produces) keeps every content row rather than silently losing one —
	// the margin only matters where a workspace frame is actually drawn beside
	// the section, which is always at the grid's real (>= floor) sizes.
	contentH := a.rect.H
	if a.rect.H >= layout.AutomationsRows {
		contentH = a.rect.H - 1
	}

	// Window the rows around the cursor so a focused selection below the fold
	// scrolls into view instead of moving invisibly. The focused selection
	// expands to a 2-line row (title + detail), so reserve a line for the
	// detail when scrolling the selection to the bottom keeps it fully visible
	// rather than clipping its detail off the fold.
	visible := contentH - 1
	if visible < 0 {
		visible = 0
	}
	effVisible := visible
	if a.focused && effVisible > 1 {
		effVisible--
	}
	if a.offset > a.selected {
		a.offset = a.selected
	}
	if effVisible > 0 && a.selected >= a.offset+effVisible {
		a.offset = a.selected - effVisible + 1
	}
	if max := len(tasks) - visible; a.offset > max {
		a.offset = max
	}
	if a.offset < 0 {
		a.offset = 0
	}
	for i := a.offset; i < len(tasks); i++ {
		if len(lines) >= contentH {
			break
		}
		expanded := a.focused && i == a.selected
		rowStart := len(lines)
		lines = append(lines, a.titleRow(tasks[i], expanded))
		if expanded && len(lines) < contentH {
			if detail := a.detailRow(tasks[i]); detail != "" {
				lines = append(lines, detail)
			}
		}
		if a.zones != nil {
			a.zones.Register(zones.AutoTask(tasks[i].ID), layout.Rect{
				X: a.rect.X, Y: a.rect.Y + rowStart, W: a.rect.W, H: len(lines) - rowStart,
			})
		}
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
