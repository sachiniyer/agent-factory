package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// AlarmInfo is one delivery-failure alarm's display data. The app maps it from
// the daemon snapshot's DeliveryAlarm (#1238) so the ui package needs no daemon
// dependency.
type AlarmInfo struct {
	TaskName string
	Target   string
	Pending  int
	Since    time.Time
}

// AlarmBanner is the persistent top-of-screen alarm shown while one or more
// watch tasks have been failing to deliver events to their target session
// (#1238 fix c). It is deliberately impossible to miss — a full-width red bar
// stacked above everything else — so a silently dead delivery pipeline (the
// 2026-07-05 outage was visible only in the daemon log for ~23 minutes)
// becomes visible without navigating. Empty alarm set = inactive = renders
// nothing and reserves no row.
type AlarmBanner struct {
	rect   layout.Rect
	alarms []AlarmInfo
}

// alarmStyle is rebuilt from the active theme so alerts remain distinct under
// user-configured palettes.
var alarmStyle = lipgloss.NewStyle().
	Background(activeTheme.Error).
	Foreground(activeTheme.SelectionForeground).
	Bold(true)

func NewAlarmBanner() *AlarmBanner { return &AlarmBanner{} }

// SetAlarms replaces the banner's alarm set.
func (b *AlarmBanner) SetAlarms(alarms []AlarmInfo) { b.alarms = alarms }

// Alarms returns the current alarm set (for change detection).
func (b *AlarmBanner) Alarms() []AlarmInfo { return b.alarms }

// Active reports whether any alarm is raised. The app reserves the banner row
// in the grid exactly when this is true.
func (b *AlarmBanner) Active() bool { return len(b.alarms) > 0 }

// SetRect places the banner. Height is honored from the reserved grid rect.
func (b *AlarmBanner) SetRect(r layout.Rect) { b.rect = r }

// View renders the banner to its rect, or "" when inactive or unsized. With
// multiple failing tasks it names the first and counts the rest, keeping the
// bar to a single line.
func (b *AlarmBanner) View() string {
	if !b.Active() || b.rect.Empty() {
		return ""
	}
	a := b.alarms[0]
	target := a.Target
	if target == "" {
		// A task with no target_session creates a fresh session per event, so
		// there is no named target — describe it rather than print an empty "".
		target = "a new session per event"
	}
	msg := fmt.Sprintf("⚠ events not delivering to %q — %d pending since %s (task: %s)",
		target, a.Pending, a.Since.Format("15:04"), a.TaskName)
	if extra := len(b.alarms) - 1; extra > 0 {
		msg += fmt.Sprintf("  (+%d more)", extra)
	}
	if runewidth.StringWidth(msg) > b.rect.W {
		msg = runewidth.Truncate(msg, b.rect.W, "…")
	}
	line := alarmStyle.Width(b.rect.W).Render(msg)
	return layout.ClampToRect(strings.TrimRight(line, "\n"), b.rect)
}
