package tree

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// renderClean renders an instance and returns its output with ANSI stripped.
func renderClean(t *testing.T, inst *session.Instance) string {
	t.Helper()
	r := NewInstanceRenderer()
	r.SetWidth(80)
	out := r.Render(inst, 1, false, false, false)
	return ansiEscape.ReplaceAllString(out, "")
}

// TestRender_LimitBadgeWithResetTime: a limit-blocked session renders the
// [limit] marker with its reset time and the limit glyph, never a plain Ready
// dot — the core #1146 surface invariant.
func TestRender_LimitBadgeWithResetTime(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	// 2:30pm local today, so the badge omits the date.
	now := time.Now()
	reset := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, time.Local)
	inst.SetLimitReached(reset)

	clean := renderClean(t, inst)
	require.Contains(t, clean, "[limit] resets 2:30pm")
	require.Contains(t, clean, strings.TrimSpace(limitIcon), "the limit glyph must render")
	require.NotContains(t, clean, strings.TrimSpace(readyIcon)+" worker", "a limit row must not show the ready dot")
}

// TestRender_LimitBadgeNoResetTime: a limit with no parseable reset shows the
// bare [limit] marker.
func TestRender_LimitBadgeNoResetTime(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetLimitReached(time.Time{})

	clean := renderClean(t, inst)
	require.Contains(t, clean, "[limit] worker")
	require.NotContains(t, clean, "resets")
}

// TestFormatLimitReset covers the badge time formatting: on-the-hour vs minutes,
// today (time only) vs a future day (date + time).
func TestFormatLimitReset(t *testing.T) {
	now := time.Date(2026, 7, 5, 10, 0, 0, 0, time.Local)
	cases := []struct {
		name  string
		reset time.Time
		want  string
	}{
		{"on the hour today", time.Date(2026, 7, 5, 14, 0, 0, 0, time.Local), "2pm"},
		{"with minutes today", time.Date(2026, 7, 5, 14, 30, 0, 0, time.Local), "2:30pm"},
		{"future day", time.Date(2026, 7, 12, 9, 0, 0, 0, time.Local), "Jul 12 9am"},
		{"midnight today", time.Date(2026, 7, 5, 0, 0, 0, 0, time.Local), "12am"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, formatLimitReset(c.reset, now))
		})
	}
}
