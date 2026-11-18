package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// A held fire (#7518). A cron delivery sends its prompt, appends the session's
// row, and only then stamps "started" and clears the fire marker. A daemon that
// dies in between leaves a prompted row and a pending marker. If the next boot
// cannot tell whether that row's agent is still running, delivering the
// occurrence again could start a second run of it beside the first, so the fire
// is held: nothing delivers, the marker stays, and the session's own restore
// decides it. This file holds the verdict, what it records, and the replay that
// looks again until the restore settles.

// errTaskFireRuntimeUnknown matches a held fire (taskFireHeld) under errors.Is.
var errTaskFireRuntimeUnknown = errors.New("the fire is held")

// taskStatusHeldPrefix starts the run status a held fire records. It is not
// "errored:", which the TUI and the web show as a failed run: the run is most
// likely still working in the session it is held on.
const taskStatusHeldPrefix = "held: "

// taskFireHeld is the reconcile verdict that holds a fire. It is a reconcile
// failure (errTaskFireReconcile) in every rule that keeps a marker, so the
// marker survives it exactly as it survives an unanswered probe.
type taskFireHeld struct{ session string }

func (e *taskFireHeld) detail() string {
	return fmt.Sprintf("session %q, which this occurrence created, is Lost and nothing proves its agent stopped; the occurrence is not delivered again until that session's restore settles", e.session)
}

func (e *taskFireHeld) Error() string { return errTaskFireReconcile.Error() + ": " + e.detail() }

func (e *taskFireHeld) Is(target error) bool {
	return target == errTaskFireReconcile || target == errTaskFireRuntimeUnknown
}

// taskStatusAbortedHeldSessionRemoved records a held fire consumed because the
// session it was held on was removed (heldRowRemoved).
const taskStatusAbortedHeldSessionRemoved = "aborted: the session it was held on was removed"

// bindHeldRow records on the marker the row its fire is held on, the first
// time it holds there. The caller holds the task's flock. A failed write
// degrades only the removal check (heldRowRemoved) for this marker.
func bindHeldRow(taskID string, fire *taskFireMarker, id string) {
	if id == "" || fire.HeldInstanceID == id {
		return
	}
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.HeldInstanceID = id })
	fire.HeldInstanceID = id
}

// heldRowRemoved reports whether the fire was held on a row that is no longer
// persisted.
func heldRowRemoved(fire *taskFireMarker, rows []session.InstanceData) bool {
	if fire.HeldInstanceID == "" {
		return false
	}
	for i := range rows {
		if rows[i].ID == fire.HeldInstanceID {
			return false
		}
	}
	return true
}

// heldTaskFireStatus is the run status a held fire records, and whether err
// is one.
func heldTaskFireStatus(err error) (string, bool) {
	var held *taskFireHeld
	if !errors.As(err, &held) {
		return "", false
	}
	return taskStatusHeldPrefix + held.detail(), true
}

// taskFireRowSettlesFn answers taskFireRowSettles. runDaemon installs its
// manager's answer (lostRowSettles) once the restored map is live, and puts
// the previous one back on return. Unset, it answers true: holding delivers
// nothing, so it is the safe side of not knowing.
var taskFireRowSettlesFn atomic.Pointer[func(repoID string, row *session.InstanceData) bool]

// taskFireRowSettles reports whether something is still working to settle a
// held fire's Lost row, so a later look can learn what the hold waits on.
func taskFireRowSettles(repoID string, row *session.InstanceData) bool {
	if settles := taskFireRowSettlesFn.Load(); settles != nil {
		return (*settles)(repoID, row)
	}
	return true
}

// setTaskFireRowSettles installs the answer atomically and returns the one it
// replaced. A nil settles unsets it.
func setTaskFireRowSettles(settles func(repoID string, row *session.InstanceData) bool) (previous func(string, *session.InstanceData) bool) {
	var next *func(string, *session.InstanceData) bool
	if settles != nil {
		next = &settles
	}
	if old := taskFireRowSettlesFn.Swap(next); old != nil {
		return *old
	}
	return nil
}

// lostRowSettles reports whether this daemon is still working to settle a
// persisted Lost row. It is if it holds the row and any of these is true:
//   - an operation is in flight on it. A restore runs under OpRestoring while
//     the row still reads Lost, and canAutoRestoreLostSession refuses any
//     in-flight op, so asking it mid-restore would read "abandoned" and adopt
//     a row whose restore may be about to respawn its agent. A kill or archive
//     in flight decides the row too.
//   - the lost-restore loop will retry it (canAutoRestoreLostSession, which also
//     reads a durable give-up).
//   - the row has already left Lost in memory, and the record on disk has not
//     caught up.
//
// A row it does not hold, or holds Lost and idle with nothing retrying it,
// never settles on its own: a sandbox row whose reattach failed loads inert,
// and a local row can exhaust its restore budget.
func (m *Manager) lostRowSettles(repoID string, row *session.InstanceData) bool {
	m.mu.Lock()
	inst := m.instances[daemonInstanceKey(repoID, row.Title)]
	m.mu.Unlock()
	if inst == nil || !stableIDMatchesForDaemon(inst.ID, row.ID) {
		return false
	}
	view := inst.LifecycleView()
	if view.Liveness != session.LiveLost || view.InFlightOp != session.OpNone {
		return true
	}
	// Idle and Lost, the row settles only if the restore loop will still take
	// it. canAutoRestoreLostSession is that loop's own eligibility, and for an
	// idle row every gate it reads is durable intent: started, not tombstoned
	// or uncertain, no give-up, a backend that can recover, not the root. Its
	// one transient gate, an op in flight, is answered above as settling.
	return canAutoRestoreLostSession(view) && !inst.OwesPreservedWorkspace()
}

// errTaskRunActive is the per-task flock refusing a second run: an
// overlapping fire coalesces into the one already running.
var errTaskRunActive = errors.New("another run is already active")

// noteFireCoalescedBesideHeld says aloud what a coalesced fire costs when the
// task's fire is held. The overlap rule folds a fire into the run holding the
// lock, and that run is usually the held replay's own look, which delivers
// nothing. This occurrence is then not delivered at all.
func noteFireCoalescedBesideHeld(t *task.Task) {
	if !strings.HasPrefix(t.LastRunStatus, taskStatusHeldPrefix) {
		return
	}
	log.WarningLog.Printf("task %s: this fire coalesced away, because another run holds the task's lock, most likely the look of its held replay; a held fire delivers nothing, so this occurrence is not delivered (%s)", t.ID, t.LastRunStatus)
}

// taskFireRecheckDelay paces a held replay's next look (replayTaskFire). The
// session's restore is what settles a hold, so the replay asks again on the
// restore loop's own backoff. A var so tests can drive it.
var taskFireRecheckDelay = lostRestoreBackoff

// replayTaskFire replays one interrupted fire. A held fire delivers nothing
// and keeps its marker. The session's restore decides it, so the replay looks
// again as the restore loop retries rather than waiting for the next boot. A
// rebind leaves a live row the next look adopts, and a respawn proves the
// agent gone and delivers fresh.
//
// A look that finds the task's lock taken also looks again. The holder may be
// an untargeted fire, which reconciles this marker itself, so the next look
// finds it changed and stops. Or it may be a targeted one, which writes no
// marker and never would, and a task retargeted while its fire was held
// still resolves without waiting for the next boot.
//
// The hold is bounded outside this loop. It ends when the restore settles; when
// nothing will settle the row, since the reconcile then adopts it; when another
// fire discharges or replaces the marker, since a new untargeted fire supersedes
// a held one rather than wait on it; when the task is disabled or deleted; or
// when the daemon starts to stop, which leaves the marker for the next daemon's
// replay. Every look runs under the task's flock and re-reads the marker, so
// the fire has one deliverer at a time (#7468).
func replayTaskFire(m *taskFireMarker) {
	for attempt := 1; ; attempt++ {
		err := runTaskDelivery(m.TaskID, task.ProjectExpectation{}, m)
		if !errors.Is(err, errTaskFireRuntimeUnknown) && !errors.Is(err, errTaskRunActive) {
			if err != nil {
				log.WarningLog.Printf("replay of interrupted cron fire for task %s failed: %v", m.TaskID, err)
			}
			return
		}
		if attempt == 1 {
			log.InfoLog.Printf("replay of interrupted cron fire for task %s: %v; looking again", m.TaskID, err)
		}
		if parkFor(taskFireRecheckDelay(attempt)) {
			return
		}
	}
}
