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

var automationsHintStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("#7F7A7A"))

// AutomationsPane is the always-visible bottom strip of the workspace (RFC
// §2.1): compact task rows — enabled glyph, name, trigger, next/last run —
// that expand IN PLACE to the full TaskPane manager (list + edit form) when
// the strip takes focus. It replaces both the sidebar's Tasks section and the
// content pane's Tasks mode (#1024 PR 4). The pane OWNS the TaskPane it hosts;
// the compact rows render straight off the store projection so the strip and
// the manager can never show different task sets after a reload.
//
// It implements layout.Pane. Focus() forwards input focus to the hosted
// TaskPane so the manager's own key loop (j/k/n/enter/x/D/r, esc to leave) is
// live the moment the strip is focused; the TaskPane dropping its own focus
// (esc) is how the root learns to move the ring elsewhere.
type AutomationsPane struct {
	proj     *store.Projection
	taskPane *TaskPane

	rect    layout.Rect
	compact bool
	focused bool

	// now returns the current time for next-run derivation; a fixed value in
	// tests so rendered "next" columns are deterministic.
	now func() time.Time
}

// NewAutomationsPane creates the strip over the given projection.
func NewAutomationsPane(proj *store.Projection) *AutomationsPane {
	return &AutomationsPane{
		proj:     proj,
		taskPane: NewTaskPane(),
		now:      time.Now,
	}
}

// TaskPane returns the hosted task manager.
func (a *AutomationsPane) TaskPane() *TaskPane {
	return a.taskPane
}

// SetRect implements layout.Pane.
func (a *AutomationsPane) SetRect(r layout.Rect) {
	a.rect = r
	// Size the hosted manager to the strip's inner area (minus the row
	// padding); it renders whenever the strip is focused/expanded.
	w := r.W - 2
	if w < 0 {
		w = 0
	}
	h := r.H
	a.taskPane.SetSize(w, h)
}

// SetCompact selects the 1-line summary rendering (degradation ladder <80
// cols / <20 rows, RFC §2.6).
func (a *AutomationsPane) SetCompact(compact bool) {
	a.compact = compact
}

// Focused implements layout.Pane.
func (a *AutomationsPane) Focused() bool { return a.focused }

// Focus implements layout.Pane: the strip expands to the task manager and the
// manager takes input focus immediately.
func (a *AutomationsPane) Focus() {
	a.focused = true
	a.taskPane.SetFocus(true)
}

// Blur implements layout.Pane. The TaskPane keeps its dirty/deleted state
// across a blur (SetFocus(false) only cancels an in-progress form), so the
// root can save after moving focus away.
func (a *AutomationsPane) Blur() {
	a.focused = false
	a.taskPane.SetFocus(false)
}

// HandleKey implements layout.Pane: focused, the hosted TaskPane consumes its
// keys — except Tab/Shift-Tab outside a form, which bubble to the root's
// focus ring (inside the edit form they move between fields, as before).
func (a *AutomationsPane) HandleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if !a.focused {
		return nil, false
	}
	if (msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab) &&
		!a.taskPane.IsEditing() && !a.taskPane.IsCreating() {
		return nil, false
	}
	return nil, a.taskPane.HandleKeyPress(msg)
}

// HandleMouse implements layout.Pane. Mouse support is #1024 PR 6.
func (a *AutomationsPane) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// ScrollUp moves the task selection up (wheel/shift-scroll routing).
func (a *AutomationsPane) ScrollUp() { a.taskPane.ScrollUp() }

// ScrollDown moves the task selection down.
func (a *AutomationsPane) ScrollDown() { a.taskPane.ScrollDown() }

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

// compactRow renders one strip row: enabled glyph, name, trigger, next/last.
func (a *AutomationsPane) compactRow(tsk task.Task) string {
	glyph, style := "[✓]", automationsEnabledStyle
	if !tsk.Enabled {
		glyph, style = "[✗]", automationsDisabledStyle
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
	return style.Render(" " + strings.Join(parts, "  "))
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

// View implements layout.Pane: exactly rect-sized.
func (a *AutomationsPane) View() string { return a.String() }

func (a *AutomationsPane) String() string {
	if a.rect.Empty() {
		return ""
	}

	// Focused: the strip IS the task manager, expanded in place.
	if a.focused {
		return layout.ClampToRect(a.taskPane.String(), a.rect)
	}

	tasks := a.proj.GetTasks()

	// 1-line degraded summary (RFC §2.6, <80 cols).
	if a.compact || a.rect.H <= 1 {
		line := fmt.Sprintf(" Automations: %d (%d on) · S manage", len(tasks), a.enabledCount())
		return layout.ClampToRect(automationsTitleDimStyle.Render(line), a.rect)
	}

	title := automationsTitleStyle.Render(" Automations ") +
		automationsHintStyle.Render(fmt.Sprintf("(%d) · S manage · H hooks", len(tasks)))
	lines := []string{title}
	if len(tasks) == 0 {
		lines = append(lines, automationsDisabledStyle.Render("  no tasks — press s to create one"))
	}
	for _, tsk := range tasks {
		if len(lines) >= a.rect.H {
			break
		}
		lines = append(lines, a.compactRow(tsk))
	}
	return layout.ClampToRect(strings.Join(lines, "\n"), a.rect)
}
