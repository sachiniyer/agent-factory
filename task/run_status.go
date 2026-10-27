package task

import "time"

// RunKey identifies ONE run of a task: the session the delivery created, ID'd
// by its instance id and ordered by its creation time (assigned at delivery
// start, before the slow startup work). Every keyed last-run status write
// carries the run it describes, so a writer holding an older run can never
// overwrite a newer one (#5645, #5661, #5662).
//
// The zero RunKey is an UNKEYED event — a delivery into an existing target
// session, a watch event, a pre-delivery error. It has no run to order against,
// so it writes through; see RecordRunStatus.
type RunKey struct {
	ID        string
	StartedAt time.Time
}

// IsZero reports an unkeyed event.
func (k RunKey) IsZero() bool { return k.ID == "" && k.StartedAt.IsZero() }

// RunStartedStatus is the status a run holds from the moment its delivery
// starts until something terminal says otherwise.
const RunStartedStatus = "started"

// RunFailedUndeliveredStatus is the terminal status of a run whose initial
// prompt delivery was proven undelivered (#5278). It carries the "errored:"
// prefix the TUI keys on to render a failed run.
const RunFailedUndeliveredStatus = "errored: prompt not delivered"

// runOrder compares two keys: negative when a is older than b.
func runOrder(a, b RunKey) int {
	// The same id is the same run, whatever precision a persistence round trip
	// left on its start time.
	if a.ID != "" && a.ID == b.ID {
		return 0
	}
	switch {
	case a.StartedAt.Before(b.StartedAt):
		return -1
	case a.StartedAt.After(b.StartedAt):
		return 1
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}

func (t Task) lastRunKey() RunKey {
	k := RunKey{ID: t.LastRunID}
	if t.LastRunStartedAt != nil {
		k.StartedAt = *t.LastRunStartedAt
	}
	return k
}

// runStatusTransition is the whole state machine for a task's last-run status.
//
//	started ──► ok / parked / aborted / … (any terminal)
//	   │
//	   └──────► failed (RunFailedUndeliveredStatus)
//
// Rules, for a write of status s by run k against the committed row (cur, with
// run key c):
//
//  1. Unkeyed (k zero): write through, and advance the row's run clock to the
//     write time so an older keyed run can no longer claim the row.
//  2. k older than c: no-op. A stale writer never overwrites a newer run.
//  3. k newer than c (or the row has no key): adopt k as the current run and
//     write.
//  4. k == c (same run): "started" only ever initializes — it is a no-op once
//     the run holds any other status; a failed run is terminal for failure
//     (re-stamping it is a no-op, so LastRunAt is not rewritten); anything
//     else overwrites.
//
// It reports whether to write.
func runStatusTransition(cur Task, k RunKey, status string) bool {
	if k.IsZero() {
		return true
	}
	c := cur.lastRunKey()
	if c.IsZero() {
		return true
	}
	switch o := runOrder(k, c); {
	case o < 0:
		return false
	case o > 0:
		return true
	}
	if status == RunStartedStatus {
		return cur.LastRunStatus == "" || cur.LastRunStatus == RunStartedStatus
	}
	if status == RunFailedUndeliveredStatus {
		return cur.LastRunStatus != RunFailedUndeliveredStatus
	}
	return true
}

// RecordRunStatus is THE write path for a run's outcome on its task: every
// writer of "this run started / finished / failed" goes through it, so the
// ordering decision and the write are one critical section under the task-file
// lock and cannot be separated by a concurrent writer. It returns the committed
// row and whether the write was applied (false = a no-op by the machine above).
// lastRunAt nil leaves LastRunAt untouched, as UpdateTaskStatus does.
func RecordRunStatus(taskID string, k RunKey, lastRunAt *time.Time, status string) (Task, bool, error) {
	return updateTaskStatusLocked(taskID, lastRunAt, status, func(cur Task) bool {
		return runStatusTransition(cur, k, status)
	}, func(t *Task) {
		switch {
		case k.IsZero():
			if lastRunAt != nil {
				// The clock advances without an identity, so a keyed run that
				// started earlier is ordered behind this write.
				at := *lastRunAt
				t.LastRunID, t.LastRunStartedAt = "", &at
			}
		default:
			started := k.StartedAt
			t.LastRunID, t.LastRunStartedAt = k.ID, &started
		}
	})
}
