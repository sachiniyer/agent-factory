package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// WatchRateDropStatus is the last-run outcome recorded when the watcher
// discards a source event at its per-minute cap. It is not "errored": the
// watcher remains supervised and can deliver later events, but "sent" would
// falsely describe an event that never reached its target.
const WatchRateDropStatus = "dropped: event rate limit exceeded"

// isTerminalWatcherStatus reports whether status is a watcher terminal
// outcome that a drop flush must not overwrite. It mirrors the live-overlay
// terminal latch in daemon/watcher_drop_visibility.go (applyLiveDropState):
// "stopped" (a clean script exit) and any "errored:" outcome (the crash-loop
// breaker's failureSummary, or an arming refusal's "errored: not armed — …")
// are both newer-than-the-drop terminal outcomes that deliberately record
// without advancing LastRunAt, so a stop-time drop flush passing lastDroppedAt
// would otherwise satisfy the LastRunAt guard and clobber them. Unlike the
// live overlay this durable guard cannot rely on an in-memory terminalStatus
// latch, so it inspects the on-disk LastRunStatus directly.
func isTerminalWatcherStatus(status string) bool {
	return status == "stopped" || strings.HasPrefix(status, "errored:")
}

// RecordWatchRateDrops advances a task's cumulative rate-drop count to total
// without changing LastRunAt. The cumulative count advances unconditionally. It
// records the dropped outcome only when no newer terminal watcher status is
// already on disk and the drop is not older than the latest successful
// delivery, so a shutdown flush can neither overwrite newer "sent" evidence
// nor replace a newer "stopped"/"errored:" terminal outcome (terminal writes
// deliberately preserve LastRunAt, so a wall-clock droppedAt that predates the
// terminal exit would otherwise satisfy the LastRunAt guard and clobber the
// outcome the watcher persisted). total is absolute, so a delayed checkpoint
// cannot double-count a burst; a stale lower checkpoint is ignored under the
// task-file lock.
func RecordWatchRateDrops(taskID string, total int, droppedAt time.Time) (Task, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return Task{}, err
	}
	if total < 0 {
		return Task{}, fmt.Errorf("dropped event count must be non-negative")
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, err
	}

	var updated Task
	lockErr := config.WithFileLock(path, func() error {
		tasks, err := loadTasksLocked(path)
		if err != nil {
			return err
		}
		row := -1
		for i := range tasks {
			if tasks[i].ID != taskID {
				continue
			}
			row = i
			if total <= tasks[i].DroppedEvents {
				updated = tasks[i]
				return nil
			}
			tasks[i].DroppedEvents = total
			if !isTerminalWatcherStatus(tasks[i].LastRunStatus) &&
				(tasks[i].LastRunAt == nil || !droppedAt.Before(*tasks[i].LastRunAt)) {
				tasks[i].LastRunStatus = WatchRateDropStatus
			}
			break
		}
		if row < 0 {
			return fmt.Errorf("task with id %q not found", taskID)
		}

		generation, err := writeTasks(tasks)
		if err != nil {
			return err
		}
		updated = stampRowIdentity(tasks, generation)[row]
		return nil
	})
	if lockErr != nil {
		return Task{}, lockErr
	}
	return updated, nil
}
