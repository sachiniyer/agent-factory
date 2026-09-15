package task

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// WatchRateDropStatus is the last-run outcome recorded when the watcher
// discards a source event at its per-minute cap. It is not "errored": the
// watcher remains supervised and can deliver later events, but "sent" would
// falsely describe an event that never reached its target.
const WatchRateDropStatus = "dropped: event rate limit exceeded"

// RecordWatchRateDrops advances a task's cumulative rate-drop count to total
// without changing LastRunAt. It records the dropped outcome only when the drop
// is not older than the latest successful delivery, so a shutdown flush cannot
// overwrite newer "sent" evidence. total is absolute, so a delayed checkpoint
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
			if tasks[i].LastRunAt == nil || !droppedAt.Before(*tasks[i].LastRunAt) {
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
