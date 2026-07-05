package app

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

// TestApplyDeliveryAlarms_RaisesAndClearsBanner drives the TUI half of #1238
// fix (c): a snapshot that reports a persistent delivery failure raises the
// top-of-screen alarm banner (reserving its own layout row and rendering the
// target/pending detail into the composed View), and a later clean snapshot
// clears it — the row disappears and the View shrinks back. This is the
// signal that turns the 23-minute silent event-pipeline window into an alarm
// visible without navigating.
func TestApplyDeliveryAlarms_RaisesAndClearsBanner(t *testing.T) {
	h := newTestHome(t)
	resizeHome(h, 120, 40)

	// Healthy steady state: no banner, no reserved row.
	require.False(t, h.alarmBanner.Active())
	require.True(t, h.lastLayout.Banner.Empty(), "no row reserved while healthy")
	baseView := h.View()
	requireViewSized(t, baseView, 120, 40)
	require.NotContains(t, baseView, "events not delivering")

	// A snapshot reports events failing to reach "root" since 10:39.
	since := time.Date(2026, 7, 5, 10, 39, 0, 0, time.Local)
	changed := h.applyDeliveryAlarms([]daemon.DeliveryAlarm{{
		TaskID:        "12e09daa",
		TaskName:      "captain-events",
		TargetSession: "root",
		Pending:       5,
		Consecutive:   14,
		Since:         since,
		LastError:     "root agent is being recreated",
	}})
	require.True(t, changed, "a newly-raised alarm is a visible change")
	require.True(t, h.alarmBanner.Active(), "the banner must be active")
	require.False(t, h.lastLayout.Banner.Empty(), "the alarm must reserve a layout row")

	alarmView := h.View()
	requireViewSized(t, alarmView, 120, 40)
	require.Contains(t, alarmView, "events not delivering to \"root\"",
		"the banner names the failing target")
	require.Contains(t, alarmView, "5 pending", "the banner shows the stuck-event count")
	require.Contains(t, alarmView, "10:39", "the banner shows since-when")
	require.Contains(t, alarmView, "captain-events", "the banner names the task")

	// Re-applying the same alarm set is not a visible change (no needless repaint).
	require.False(t, h.applyDeliveryAlarms([]daemon.DeliveryAlarm{{
		TaskID:        "12e09daa",
		TaskName:      "captain-events",
		TargetSession: "root",
		Pending:       5,
		Consecutive:   14,
		Since:         since,
		LastError:     "root agent is being recreated",
	}}), "an unchanged alarm set reports no change")

	// Delivery recovers: an empty alarm set clears the banner and its row.
	require.True(t, h.applyDeliveryAlarms(nil), "clearing the alarm is a visible change")
	require.False(t, h.alarmBanner.Active(), "the banner clears on recovery")
	require.True(t, h.lastLayout.Banner.Empty(), "the reserved row is released")

	clearedView := h.View()
	requireViewSized(t, clearedView, 120, 40)
	require.NotContains(t, clearedView, "events not delivering",
		"the banner is gone once delivery recovers")
	require.Equal(t, len(strings.Split(baseView, "\n")), len(strings.Split(clearedView, "\n")),
		"the view returns to its pre-alarm height")
}
