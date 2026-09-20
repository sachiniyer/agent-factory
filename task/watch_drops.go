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
	updated, _, err := recordWatchRateDrops(taskID, "", false, total, droppedAt)
	return updated, err
}

// RecordWatchRateDropsForGeneration applies the same checkpoint only while the
// task ID still names the incarnation the watcher supervised. A remove+re-add
// that reused the ID makes the write a clean refusal rather than a storage
// error, so a stopped predecessor's flush cannot stamp its drop count — or the
// "dropped: event rate limit exceeded" status — onto the replacement task
// (#4224 review). applied reports whether a write landed: a generation
// mismatch and a stale lower checkpoint both return false.
func RecordWatchRateDropsForGeneration(taskID, expectedGenerationID string, total int, droppedAt time.Time) (Task, bool, error) {
	return recordWatchRateDrops(taskID, expectedGenerationID, true, total, droppedAt)
}

func recordWatchRateDrops(taskID, expectedGenerationID string, requireGeneration bool, total int, droppedAt time.Time) (Task, bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return Task{}, false, err
	}
	if total < 0 {
		return Task{}, false, fmt.Errorf("dropped event count must be non-negative")
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, false, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, false, err
	}

	var updated Task
	applied := false
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
			if requireGeneration && tasks[i].GenerationID != expectedGenerationID {
				// The ID was rebound to a new incarnation: a clean refusal,
				// not a storage fault — the predecessor's count is not this
				// task's evidence.
				return nil
			}
			if total <= tasks[i].DroppedEvents {
				updated = tasks[i]
				return nil
			}
			tasks[i].DroppedEvents = total
			if !isTerminalWatcherStatus(tasks[i].LastRunStatus) &&
				(tasks[i].LastRunAt == nil || !droppedAt.Before(*tasks[i].LastRunAt)) {
				tasks[i].LastRunStatus = WatchRateDropStatus
			}
			applied = true
			break
		}
		if row < 0 {
			return fmt.Errorf("task with id %q not found", taskID)
		}
		if !applied {
			return nil
		}

		generation, err := writeTasks(tasks)
		if err != nil {
			return err
		}
		updated = stampRowIdentity(tasks, generation)[row]
		return nil
	})
	if lockErr != nil {
		return Task{}, false, lockErr
	}
	return updated, applied, nil
}

// ResetWatchRateDropsForGeneration clears a rebound task's inherited drop
// evidence: the durable mirror of the in-memory zeroing reconcile and restart
// apply when the row under a reused ID still carries the predecessor
// incarnation's DroppedEvents. Without it ListTasks keeps reporting the stale
// total, the replacement's lower checkpoints read as stale and are refused,
// and the next daemon restart reseeds the watcher from the count the reset
// meant to discard (#4224 review).
//
// The generation gate is the same clean refusal the checkpoint path uses: a
// row that moved to yet another incarnation belongs to that incarnation's
// accounting, not this one's. applied reports whether a write landed — a
// generation mismatch and an already-clean row both return false. The
// "dropped: event rate limit exceeded" status is cleared only when it is the
// value a drop record would have left; any other outcome is this
// incarnation's own and stays.
func ResetWatchRateDropsForGeneration(taskID, expectedGenerationID string) (Task, bool, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return Task{}, false, err
	}
	path, err := getTasksPathFn()
	if err != nil {
		return Task{}, false, err
	}
	if err := ensureTasksSchemaMigrated(path); err != nil {
		return Task{}, false, err
	}

	var updated Task
	applied := false
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
			if tasks[i].GenerationID != expectedGenerationID {
				return nil
			}
			if tasks[i].DroppedEvents == 0 && tasks[i].LastRunStatus != WatchRateDropStatus {
				updated = tasks[i]
				return nil
			}
			tasks[i].DroppedEvents = 0
			if tasks[i].LastRunStatus == WatchRateDropStatus {
				tasks[i].LastRunStatus = ""
			}
			applied = true
			break
		}
		if row < 0 {
			return fmt.Errorf("task with id %q not found", taskID)
		}
		if !applied {
			return nil
		}

		generation, err := writeTasks(tasks)
		if err != nil {
			return err
		}
		updated = stampRowIdentity(tasks, generation)[row]
		return nil
	})
	if lockErr != nil {
		return Task{}, false, lockErr
	}
	return updated, applied, nil
}
