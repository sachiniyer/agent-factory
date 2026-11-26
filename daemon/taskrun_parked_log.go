package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// parkedDeliveryReminder caps how often a held delivery repeats a heartbeat
// line while a re-check loop keeps observing it parked (#7843).
const parkedDeliveryReminder = time.Hour

// parkedDeliveryMark is one announced park episode: the session the task's
// deliveries park against, how many re-checks it has suppressed, and when the
// last notice went out.
type parkedDeliveryMark struct {
	target   string
	checks   int
	lastNote time.Time
}

// parkedDeliveryTracker records which tasks have already announced a parked
// delivery, so the notice is logged per state transition instead of per
// re-check (#7843). The watch drainer retries a parked head every
// drainBaseBackoff (~10s) and the cron loop re-fires, which used to write the
// identical line ~8K times a day per parked task.
//
// Marks are keyed by task because a task delivers to exactly one canonical
// target at a time; the mark remembers which target so a retarget ends the old
// episode rather than being swallowed by it. A mark is retired on every exit
// from parked — a delivery verdict that is not parked, a dropped queued event,
// or the task being removed, disabled, or retargeted — so a later park is
// always a fresh, announced transition. A failed re-check keeps the mark: it
// disproved nothing about the park itself, and the drainer already logs the
// failure on its own cadence.
type parkedDeliveryTracker struct {
	mu    sync.Mutex
	marks map[string]parkedDeliveryMark
}

// taskParkedDeliveries is process-wide because a parked delivery outlives the
// code path that observed it: the drainer's re-checks, later cron fires, task
// edits, and queue drops all have to see the same "already announced" answer.
var taskParkedDeliveries = &parkedDeliveryTracker{marks: make(map[string]parkedDeliveryMark)}

// noteParked records that taskID's delivery to target came back parked at now.
// announce reports the transition into a park — log it. remind reports the
// episode is still holding parkedDeliveryReminder after the last notice —
// log a heartbeat naming the suppressed re-check count.
func (p *parkedDeliveryTracker) noteParked(taskID, target string, now time.Time) (announce, remind bool, checks int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mark, ok := p.marks[taskID]
	if !ok || mark.target != target {
		p.marks[taskID] = parkedDeliveryMark{target: target, lastNote: now}
		return true, false, 0
	}
	mark.checks++
	if now.Sub(mark.lastNote) >= parkedDeliveryReminder {
		mark.lastNote = now
		remind = true
	}
	p.marks[taskID] = mark
	return false, remind, mark.checks
}

// end retires taskID's announced park and returns the mark so the caller can
// say what ended. ok=false means nothing was announced — no transition, no log.
func (p *parkedDeliveryTracker) end(taskID string) (mark parkedDeliveryMark, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	mark, ok = p.marks[taskID]
	if ok {
		delete(p.marks, taskID)
	}
	return mark, ok
}

// noteExit retires an announced park outside the delivery-verdict path — a
// dropped queued event or a task that left the armed set — and logs the
// transition once. A task with no announced park is a no-op.
func (p *parkedDeliveryTracker) noteExit(taskID, reason string) {
	mark, ok := p.end(taskID)
	if !ok {
		return
	}
	log.InfoLog.Printf("task %s: parked delivery to target session %q ended: %s", taskID, mark.target, reason)
}

// noteRetarget ends an announced park when the task now delivers somewhere
// else: the old episode ended even though the task itself still runs. It runs
// before every delivery attempt so a retarget that happens between re-checks
// is caught at the verdict, not only at the task-write sweep.
func (p *parkedDeliveryTracker) noteRetarget(taskID, target string) {
	p.mu.Lock()
	mark, ok := p.marks[taskID]
	if ok && mark.target != target {
		delete(p.marks, taskID)
	} else {
		ok = false
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	if target == "" {
		log.InfoLog.Printf("task %s: parked delivery to target session %q ended: the task no longer targets a session", taskID, mark.target)
		return
	}
	log.InfoLog.Printf("task %s: parked delivery to target session %q ended: the task was retargeted to %q", taskID, mark.target, target)
}

// sweep retires announced parks whose task can no longer reach the parked
// target — it was removed, disabled, or retargeted. It runs on every armed-set
// reload so out-of-band tasks.json edits settle the same way CRUD writes do,
// and so a re-park under a reused task ID is never swallowed by a stale mark.
func (p *parkedDeliveryTracker) sweep(tasks []task.Task) {
	byID := make(map[string]task.Task, len(tasks))
	for _, t := range tasks {
		if _, dup := byID[t.ID]; !dup {
			byID[t.ID] = t
		}
	}
	type ended struct {
		taskID string
		mark   parkedDeliveryMark
		reason string
	}
	var exits []ended
	p.mu.Lock()
	for id, mark := range p.marks {
		t, ok := byID[id]
		reason := ""
		switch {
		case !ok:
			reason = "the task was removed"
		case !t.Enabled:
			reason = "the task was disabled"
		default:
			if target := task.CanonicalTargetSession(t.TargetSession); target != mark.target {
				reason = "the task no longer targets a session"
				if target != "" {
					reason = fmt.Sprintf("the task was retargeted to %q", target)
				}
			}
		}
		if reason != "" {
			delete(p.marks, id)
			exits = append(exits, ended{id, mark, reason})
		}
	}
	p.mu.Unlock()
	for _, e := range exits {
		log.InfoLog.Printf("task %s: parked delivery to target session %q ended: %s", e.taskID, e.mark.target, e.reason)
	}
}

// parkedLimitResetSuffix best-effort reads the parked target's recorded reset
// time so the one park line can name when the hold should lift (#7843). It
// runs only on the announced transition — suppressed re-checks never reach
// here — so its filesystem read costs nothing per tick.
func parkedLimitResetSuffix(t *task.Task, target string) string {
	repoID := t.RepoID
	if repoID == "" {
		repo, err := config.RepoFromPath(t.ProjectPath)
		if err != nil {
			return ""
		}
		repoID = repo.ID
	}
	rows, err := loadRepoInstanceData(repoID)
	if err != nil {
		return ""
	}
	for i := range rows {
		if rows[i].Title == target && !rows[i].LimitResetAt.IsZero() {
			return "; the recorded limit resets at " + rows[i].LimitResetAt.Format(time.RFC3339)
		}
	}
	return ""
}
