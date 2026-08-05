package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/require"
)

// A task refused at arming has never run, so it has no LastRunAt — and gating
// the summary on a timestamp would hide the one thing it has to report (#2929).
// But a watch task already reports the same prefix through watchTaskStatus, so
// saying it again renders "watch: … · errored · errored" (#2948).
func TestNextRunSummary_ErroredAppearsOnceAndWithoutATimestamp(t *testing.T) {
	pane := &AutomationsPane{now: func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) }}

	t.Run("cron task with no prior run still reports errored", func(t *testing.T) {
		summary := pane.nextRunSummary(task.Task{
			ID: "t", Enabled: true, CronExpr: "0 3 * * *",
			LastRunStatus: "errored: not armed — target session is archived",
		})
		require.Contains(t, summary, "errored",
			"a task that never ran is exactly the one whose refusal must show")
	})

	t.Run("watch task reports errored exactly once", func(t *testing.T) {
		summary := pane.nextRunSummary(task.Task{
			ID: "t", Enabled: true, WatchCmd: "tail -f log",
			LastRunStatus: "errored: watch command failed",
		})
		require.Equal(t, 1, strings.Count(summary, "errored"),
			"watchTaskStatus already reported it; a second copy renders as \"errored · errored\": %q", summary)
	})

	t.Run("a healthy cron task says nothing about errors", func(t *testing.T) {
		summary := pane.nextRunSummary(task.Task{
			ID: "t", Enabled: true, CronExpr: "0 3 * * *", LastRunStatus: "started",
		})
		require.NotContains(t, summary, "errored")
	})
}
