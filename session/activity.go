package session

// Activity is the derived answer to "is this session still busy?" — the single
// question both `af sessions watch` and the watch-task concurrency limit (#1892)
// ask of a session record. It is a projection of the two-axis state (#1195), not
// a fourth stored axis: nothing persists it, ClassifyActivity computes it.
//
// It lives here, in the leaf session package, because both consumers need it and
// daemon/ cannot import api/ (api/ imports daemon/). Two copies of this state
// machine would drift — and the two callers disagreeing about whether a session
// is busy is exactly the class of bug #1892 reports from userland, where a
// monitor inferred busyness from titles and liveness and overshot its own cap.
type Activity int

const (
	// ActivityPending: the session is still settling or working — an operation is
	// in flight (create/kill/archive/restore), the agent is running, or it is
	// parked on a usage limit the daemon auto-resumes (#1146). It holds a
	// concurrency slot and `sessions watch` keeps polling.
	ActivityPending Activity = iota
	// ActivityIdle: the agent went idle and awaits input — done working, ready for
	// review. Releases a concurrency slot; `sessions watch` exits 0.
	ActivityIdle
	// ActivityTerminal: the session reached a state it cannot leave on its own
	// (lost/dead/archived). Releases a concurrency slot; `sessions watch` exits
	// non-zero with the reason.
	ActivityTerminal
)

// ClassifyActivity maps a session record onto the activity projection, returning
// the outcome and a human clause explaining a terminal (or idle) result.
//
// It reads the canonical two-axis state (#1195): an in-flight client/executor
// operation means the session is still settling, so it wins over the liveness
// axis — this is what makes a brand-new session count as busy from the moment its
// create begins, before any liveness exists and while its asynchronous
// post-worktree hooks still run (#1892).
//
// The LivenessUnset branch falls back to the composed legacy Status for records
// that predate the liveness field. That fallback is load-bearing, not vestigial:
// LivenessForStatus maps the transient Loading/Deleting to LiveReady, so
// resolving a legacy record through the liveness axis alone would report a
// mid-create session as idle — releasing a concurrency slot it should hold, and
// telling `sessions watch` a session is ready before it ever started.
func ClassifyActivity(data InstanceData) (Activity, string) {
	// Any operation in flight (create, kill, archive, restore) means the session
	// is mid-transition; wait for it to settle rather than reporting the
	// transient composed status.
	switch data.InFlightOp {
	case OpCreating, OpKilling, OpArchiving, OpRestoring:
		return ActivityPending, ""
	}

	switch data.Liveness {
	case LiveReady:
		return ActivityIdle, "idle (ready for review)"
	case LiveRunning:
		return ActivityPending, ""
	case LiveLimitReached:
		// Blocked on a provider usage limit; the daemon auto-resumes it (#1146),
		// so treat it as still-working rather than done.
		return ActivityPending, ""
	case LiveLost:
		return ActivityTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case LiveDead:
		return ActivityTerminal, "session is dead (its backing tmux/worktree vanished)"
	case LiveArchived:
		return ActivityTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	case LivenessUnset:
		// Pre-#1195 record with no liveness axis: derive from the legacy Status.
		return classifyActivityByStatus(data.Status)
	}
	return ActivityPending, ""
}

// ClassifyInstanceActivity is ClassifyActivity for a LIVE instance, reading the
// two axes instead of serializing the whole session.
//
// The distinction is not cosmetic: ToInstanceData walks an instance's tabs,
// worktree, and PR state, and the daemon's concurrency check runs this over every
// session in a repo while holding the manager lock — the same reason Snapshot
// keeps its serialization outside that lock. Both entry points share the one
// state machine below, so a live instance and its persisted record can never
// disagree about whether the session is busy.
//
// The two axes are read under a single instance lock (activityAxesLocked), not
// through separate GetLiveness/GetInFlightOp calls: the status poll can be
// mutating this instance concurrently, and a torn read that paired a stale
// LiveReady with a just-set OpNone would misclassify a session that is really
// mid-transition as idle — freeing a concurrency slot it should still hold. The
// legacy Status axis is not read at all: a live in-memory instance always has a
// resolved liveness (NewInstance sets it, FromInstanceData rolls a legacy record
// forward at load), so ClassifyActivity's LivenessUnset fallback never applies.
func ClassifyInstanceActivity(i *Instance) Activity {
	if i == nil {
		return ActivityTerminal
	}
	liveness, op := i.activityAxes()
	activity, _ := ClassifyActivity(InstanceData{Liveness: liveness, InFlightOp: op})
	return activity
}

// classifyActivityByStatus is the legacy-Status fallback for ClassifyActivity,
// used only for records written before the liveness axis existed (#1195).
func classifyActivityByStatus(s Status) (Activity, string) {
	switch s {
	case Ready:
		return ActivityIdle, "idle (ready for review)"
	case Running, Loading, Deleting:
		return ActivityPending, ""
	case Lost:
		return ActivityTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case Dead:
		return ActivityTerminal, "session is dead (its backing tmux/worktree vanished)"
	case Archived:
		return ActivityTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	}
	return ActivityPending, ""
}
