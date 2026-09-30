package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/schedule"
)

// scheduleLines renders the picker and returns its visible lines.
func scheduleLines(p *schedulePicker) []string {
	return strings.Split(xansi.Strip(p.render()), "\n")
}

// seededTimePicker is an unfocused picker seeded with cron, cursor on the type.
func seededTimePicker(t *testing.T, cron string) *schedulePicker {
	t.Helper()
	p := newSchedulePicker()
	sc, ok := schedule.ParseCron(cron)
	require.True(t, ok, "cron %q must parse to a schedule", cron)
	p.seed(sc)
	p.setWidth(80)
	return p
}

// TestScheduleTimeLineReadsLikeItsSummary is #4958: the editable time field
// rendered "At 3 : 00  AM" over a summary reading "at 3:00 AM". The field must
// read exactly like the summary whichever of its cells holds the cursor, and
// for every preset that carries a time.
func TestScheduleTimeLineReadsLikeItsSummary(t *testing.T) {
	for _, tc := range []struct {
		cron, line, summary string
	}{
		{"0 3 * * *", "  At 3:00 AM", "Every day at 3:00 AM"},
		{"41 15 * * *", "  At 3:41 PM", "Every day at 3:41 PM"},
		{"0 9 * * 1,3", "  At 9:00 AM", "Every week on Mon, Wed at 9:00 AM"},
		{"30 14 15 * *", "  At 2:30 PM", "Every month on the 15th at 2:30 PM"},
	} {
		t.Run(tc.cron, func(t *testing.T) {
			p := seededTimePicker(t, tc.cron)
			lines := scheduleLines(p)
			require.GreaterOrEqual(t, len(lines), 2)
			assert.Equal(t, tc.line, lines[1], "unfocused field")
			assert.Contains(t, strings.Join(lines, "\n"), tc.summary)

			p.setFocused(true)
			for _, cell := range []scheduleCell{cellHour, cellMinute, cellMeridiem} {
				p.handleKey(keyType(tea.KeyDown))
				require.Equal(t, cell, p.activeCell())
				assert.Equal(t, tc.line, scheduleLines(p)[1], "cursor on cell %d", cell)
			}
		})
	}
}

// TestScheduleTimeLineKeepsCellFocusAndEditing: tightening the field must not
// cost the cursor. Each cell still carries the focus highlight on exactly its
// own text, and typing and toggling still edit the cell under the cursor.
func TestScheduleTimeLineKeepsCellFocusAndEditing(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	p := seededTimePicker(t, "0 3 * * *")
	p.setFocused(true)
	roles := CurrentTheme()
	focus := lipgloss.NewStyle().Bold(true).Underline(true).Background(roles.SurfaceRaised).Foreground(roles.Ink)

	for _, want := range []string{"3", "00", "AM"} {
		p.handleKey(keyType(tea.KeyDown))
		assert.Contains(t, p.renderTimeLine(lipgloss.NewStyle()), focus.Render(want),
			"the focused cell highlights its own text")
	}

	// Meridiem: space flips it in place.
	p.handleKey(keyType(tea.KeySpace))
	assert.Equal(t, "  At 3:00 PM", scheduleLines(p)[1])

	// Hour: clear it and type a new value. The blank mid-edit cell keeps one
	// visible cell so the highlight never collapses.
	p.handleKey(keyType(tea.KeyUp))
	p.handleKey(keyType(tea.KeyUp))
	require.Equal(t, cellHour, p.activeCell())
	p.handleKey(keyType(tea.KeyBackspace))
	assert.Equal(t, "  At  :00 PM", scheduleLines(p)[1])
	assert.Contains(t, p.renderTimeLine(lipgloss.NewStyle()), focus.Render(" "))
	p.handleKey(keyRunes("1"))
	p.handleKey(keyRunes("1"))
	assert.Equal(t, "  At 11:00 PM", scheduleLines(p)[1])
	assert.Equal(t, "0 23 * * *", p.Cron())
}
