package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// TestAlarmBanner_PendingUnknownRendersHonestly pins the #3242 banner contract:
// a queue whose on-disk state could not be loaded must render as an unknown
// count, never as "0 pending" — the fabricated zero would tell the operator
// nothing is stuck while an unreadable file holds the backlog.
func TestAlarmBanner_PendingUnknownRendersHonestly(t *testing.T) {
	b := NewAlarmBanner()
	since := time.Date(2026, 8, 13, 14, 2, 0, 0, time.UTC)

	b.SetAlarms([]AlarmInfo{{TaskName: "watch-x", Target: "root", Pending: 0, Since: since}})
	b.SetRect(layout.Rect{W: 120, H: 1})
	if v := b.View(); !strings.Contains(v, "0 pending") {
		t.Fatalf("a known-empty backlog renders its count; view = %q", v)
	}

	b.SetAlarms([]AlarmInfo{{TaskName: "watch-x", Target: "root", Pending: 0, PendingUnknown: true, Since: since}})
	v := b.View()
	if !strings.Contains(v, "pending count unknown") {
		t.Fatalf("an unloadable backlog must render as unknown; view = %q", v)
	}
	if strings.Contains(v, "0 pending") {
		t.Fatalf("an unloadable backlog must not render the fabricated zero; view = %q", v)
	}
}
