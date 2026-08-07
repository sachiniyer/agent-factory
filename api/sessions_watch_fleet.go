package api

import (
	"fmt"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Watching a whole fleet for state TRANSITIONS (#3009).
//
// `af sessions watch <title>` answers "is this session idle?" — a LEVEL. That
// makes "block until something finishes" and "tell me what is finished now" the
// same call: arming watchers over seven already-idle sessions returned all seven
// instantly. A driver cannot tell a session that just completed from one that has
// been idle for an hour, so it re-triggers on the same session forever unless it
// keeps its own prior-state map — which is the bookkeeping the command exists to
// remove.
//
// So this half reports EDGES. The first poll establishes a baseline and reports
// nothing; afterwards a session is reported when it CHANGES into a stopped state.
// --include-current opts into the starting snapshot for a driver that wants one.
//
// Two properties are load-bearing rather than incidental:
//
//   - It says WHY a session stopped. A driver responds differently to "idle",
//     "usage-limited" (auto-resumes; do not interrupt) and "lost" (needs restore,
//     not a prompt). Without it a driver must preview the pane and read terminal
//     text to guess, which is what the issue was filed about.
//   - UNKNOWN is a reason of its own and never collapses into idle. An idle report
//     tells a driver to act; a fabricated one makes it interrupt a working session.

// watchStopReason is why a session is no longer working. It is a string rather
// than an int because it crosses the CLI boundary as JSON that a driver switches
// on — a renumbered iota would silently change every consumer's meaning.
type watchStopReason string

const (
	// watchStopIdle: the agent stopped and is awaiting input.
	//
	// This deliberately covers BOTH "finished its work" and "waiting on input for
	// something it needs", because af cannot tell them apart: both are LiveReady,
	// and no field on InstanceData separates them. Reporting a "finished" would be
	// a guess, and the expensive direction of that guess is a driver moving on from
	// a session that is actually blocked on it (#3009).
	watchStopIdle watchStopReason = "idle"
	// watchStopUsageLimited: blocked on a provider usage limit. The daemon
	// auto-resumes it (#1146), so a driver should NOT send it a prompt — it is
	// reported so the driver can leave it alone knowingly rather than by omission.
	watchStopUsageLimited watchStopReason = "usage-limited"
	// watchStopLost: the backing tmux/worktree vanished under a live record. Needs
	// `af sessions restore`, not a prompt.
	watchStopLost watchStopReason = "lost"
	// watchStopDead: observed death of the backing runtime.
	watchStopDead watchStopReason = "dead"
	// watchStopArchived: deliberately shelved and inert until restored.
	watchStopArchived watchStopReason = "archived"
	// watchStopKilled: a committed kill whose teardown may still be running. The
	// session will not come back.
	watchStopKilled watchStopReason = "killed"
	// watchStopGone: the session was present on an earlier poll and is absent now.
	// Distinct from `killed`, which is a state the record still reports; this is the
	// record itself disappearing.
	watchStopGone watchStopReason = "gone"
	// watchStopUnknown: af cannot determine the state.
	//
	// Never silently mapped to idle, and that is the whole point of having it. An
	// idle report is an instruction to act, so a fabricated one makes an automated
	// driver interrupt a session that is working — this repo's recurring failure
	// shape (#2870, #2874, #2885, #2962), and worse here because the consumer acts
	// immediately and without a human.
	watchStopUnknown watchStopReason = "unknown"
	// watchWorking is the sentinel for "not stopped at all". It never appears in an
	// event; it exists so the phase map has one total function over every session.
	watchWorking watchStopReason = "working"
)

// stopped reports whether this phase is one a driver should be told about.
func (r watchStopReason) stopped() bool { return r != watchWorking }

// watchEvent is one reported transition.
type watchEvent struct {
	Title  string          `json:"title"`
	ID     string          `json:"id,omitempty"`
	Path   string          `json:"path,omitempty"`
	Reason watchStopReason `json:"reason"`
	Detail string          `json:"detail,omitempty"`
	// Session is the full record so a driver can act without a second call. Absent
	// for `gone`, where there is no longer a record to send.
	Session *session.InstanceData `json:"session,omitempty"`
}

// classifyWatchStop maps one snapshot onto a stop reason.
//
// The order is the argument. Every check that means "af does not know" or "this
// is over" is asked BEFORE the liveness axis, because the liveness value on such
// a record is not trustworthy — a failed create can carry a stale OpCreating, and
// a committed kill can carry whatever the row held when the kill landed.
func classifyWatchStop(d session.InstanceData) (watchStopReason, string) {
	// Asked first: a record whose startup could not be confirmed has a liveness
	// value that describes nothing. Reading it would produce exactly the
	// fabricated-idle this command must not emit.
	if d.StartupStateUnknown {
		return watchStopUnknown,
			"af could not confirm which runtime owns this workspace; inspect it and remove it explicitly before retrying"
	}
	if d.UserKilled {
		return watchStopKilled, "killed; its teardown may still be running"
	}
	// Mid-flight operations are not stops. A create/kill/archive/restore/handoff/
	// respawn in progress is a session in motion, and reporting it would wake a
	// driver for a state that is about to change on its own.
	switch d.InFlightOp {
	case session.OpCreating, session.OpKilling, session.OpArchiving,
		session.OpRestoring, session.OpReplacing, session.OpRespawning:
		return watchWorking, ""
	}
	if d.PendingHandoffMission != "" {
		return watchWorking, ""
	}

	switch d.Liveness {
	case session.LiveRunning:
		return watchWorking, ""
	case session.LiveReady:
		return watchStopIdle, "the agent stopped and is awaiting input"
	case session.LiveLimitReached:
		return watchStopUsageLimited, "blocked on a provider usage limit; af resumes it automatically — do not send it a prompt"
	case session.LiveLost:
		return watchStopLost, "its backing tmux/worktree vanished; recover it with 'af sessions restore'"
	case session.LiveDead:
		return watchStopDead, "its backing runtime is gone"
	case session.LiveArchived:
		return watchStopArchived, "archived and inert; restore it before sending work"
	}

	// LivenessUnset, or a value this binary does not know.
	//
	// The single-title path falls back to the legacy Status axis here. This one
	// deliberately does not, and the reason is sharper than "be conservative":
	// `Running` is Status's ZERO VALUE (session/instance.go: `Running Status =
	// iota`), so a record that never had a status set is indistinguishable from one
	// explicitly marked running. Reading it would not be a legacy fallback, it would
	// be inventing an answer out of an unset field — the exact shape this reason
	// exists to refuse. An empty record is something af cannot classify, and that is
	// what it says.
	return watchStopUnknown, "af cannot determine this session's state from its record"
}

// watchKeyOf identifies a session across polls.
//
// The stable id wins over the title, and that is not a detail: titles are reused.
// A session killed and re-created under the same name is a DIFFERENT session, and
// keying by title would carry the dead one's phase onto the new one — so a fresh
// session that started working would look like "no change" and never be reported,
// or worse, inherit an `idle` baseline and be reported the moment it was created.
// Falls back to repo+title only for pre-#1738 records that have no id.
func watchKeyOf(d session.InstanceData) string {
	if d.ID != "" {
		return "id:" + d.ID
	}
	// The worktree path rather than the bare title: it is unique per session and
	// distinguishes same-titled sessions in different repositories.
	return "path:" + d.Path + "\x00" + d.Title
}

// fleetWatcher turns a sequence of whole-fleet snapshots into transition events.
//
// Pure and snapshot-driven: no clock, no daemon, no I/O. The interesting
// behaviour here is entirely about ORDERINGS — first poll, change, change back,
// vanish, reappear — and a state machine that needs a live fleet to reach those
// will not be tested for all of them.
type fleetWatcher struct {
	phase map[string]watchStopReason
	// titles remembers the last title seen for a key, so a session that vanishes
	// can still be named in its event.
	titles map[string]string
	// primed is false until the first snapshot has been absorbed. It is what makes
	// this edge-triggered rather than level-triggered.
	primed bool
	// includeCurrent reports the sessions that are ALREADY stopped at the first
	// snapshot, for a driver that wants a starting picture.
	includeCurrent bool
}

func newFleetWatcher(includeCurrent bool) *fleetWatcher {
	return &fleetWatcher{
		phase:          map[string]watchStopReason{},
		titles:         map[string]string{},
		includeCurrent: includeCurrent,
	}
}

// observe absorbs one snapshot and returns the transitions it represents.
//
// Events are sorted by title so a poll that produces several is deterministic —
// a driver reading the first line of output gets a stable answer rather than map
// order.
func (w *fleetWatcher) observe(snapshot []session.InstanceData) []watchEvent {
	seen := make(map[string]struct{}, len(snapshot))
	var events []watchEvent

	for i := range snapshot {
		data := snapshot[i]
		key := watchKeyOf(data)
		seen[key] = struct{}{}
		reason, detail := classifyWatchStop(data)
		previous, known := w.phase[key]
		w.phase[key] = reason
		w.titles[key] = data.Title

		switch {
		case !known && !w.primed:
			// Baseline. Reported only when the caller asked for the starting
			// snapshot; otherwise this is exactly the level-triggered behaviour that
			// made the single-title command unusable as a trigger.
			//
			// Archived sessions are left out of that snapshot even so, and this is a
			// measured decision rather than a taste one: on the box this was built
			// against, --include-current reported 440 archived sessions and 6 idle
			// ones. A starting picture is "what needs me now", and a deliberately
			// shelved session never does — it is inert by construction and cannot
			// change on its own. Burying six actionable lines under 440 is the
			// "loop with extra steps" outcome this feature exists to avoid.
			//
			// A TRANSITION into archived is still reported below: something archiving
			// a session out from under a driver is news, whereas a shelf that was
			// already full is not.
			if w.includeCurrent && reason.stopped() && reason != watchStopArchived {
				events = append(events, newWatchEvent(data, reason, detail))
			}
		case !known:
			// A session that appeared after priming. Report it only if it arrives
			// already stopped — an appearing session that is working is not news, and
			// its stop will be reported when it happens.
			if reason.stopped() {
				events = append(events, newWatchEvent(data, reason, detail))
			}
		case reason != previous && reason.stopped():
			// The edge this command exists for.
			events = append(events, newWatchEvent(data, reason, detail))
		}
	}

	// A session that was present and is now absent. Only reported after priming:
	// before that there is nothing it could have transitioned from.
	if w.primed {
		for key, previous := range w.phase {
			if _, present := seen[key]; present {
				continue
			}
			if previous != watchStopGone {
				events = append(events, watchEvent{
					Title:  w.titles[key],
					Reason: watchStopGone,
					Detail: "the session's record disappeared while watching (killed or removed)",
				})
			}
			delete(w.phase, key)
			delete(w.titles, key)
		}
	}

	w.primed = true
	sort.SliceStable(events, func(i, j int) bool { return events[i].Title < events[j].Title })
	return events
}

func newWatchEvent(d session.InstanceData, reason watchStopReason, detail string) watchEvent {
	record := d
	return watchEvent{
		Title:   d.Title,
		ID:      d.ID,
		Path:    d.Path,
		Reason:  reason,
		Detail:  detail,
		Session: &record,
	}
}

// describeWatchEvent renders one event as a human line.
func describeWatchEvent(e watchEvent) string {
	if e.Detail == "" {
		return fmt.Sprintf("%s\t%s", e.Title, e.Reason)
	}
	return fmt.Sprintf("%s\t%s\t%s", e.Title, e.Reason, e.Detail)
}

// fleetWatchDeps are the injectable dependencies of the fleet loop, so its
// timeout and polling behaviour are testable without a daemon or a wall clock.
type fleetWatchDeps struct {
	list     func() ([]session.InstanceData, error)
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	sleep    func(time.Duration)
}

// watchFleet polls the whole fleet until at least one session transitions, and
// returns the transitions from that poll.
//
// It returns EVERY event from the poll that produced the first one, not just the
// single earliest. Several sessions finishing inside one interval is the normal
// case for a fleet, and reporting one while discarding the rest would hand a
// driver an incomplete picture that it cannot recover — the next call establishes
// a new baseline, so the dropped events would never be reported at all.
func watchFleet(d fleetWatchDeps, includeCurrent bool) ([]watchEvent, error) {
	watcher := newFleetWatcher(includeCurrent)
	start := d.now()
	for {
		snapshot, err := d.list()
		if err != nil {
			return nil, err
		}
		if events := watcher.observe(snapshot); len(events) > 0 {
			return events, nil
		}
		if d.timeout > 0 && !d.now().Before(start.Add(d.timeout)) {
			return nil, fmt.Errorf("timed out after %s waiting for any session to change state (%d watched)",
				d.timeout, len(snapshot))
		}
		d.sleep(d.interval)
	}
}

var (
	watchAllFlag            bool
	watchIncludeCurrentFlag bool
)

// runFleetWatch is the no-title / --all form: block until any session in scope
// changes state, then report which and why.
func runFleetWatch() error {
	// Same scoping as the single-title form: --repo confines the fleet to one
	// repository, and an empty repoID watches every session the daemon reports.
	repoID, err := resolveRepoIDForLookup()
	if err != nil {
		return jsonError(err)
	}

	events, err := watchFleet(fleetWatchDeps{
		list:     func() ([]session.InstanceData, error) { return listSessionsInScope(repoID) },
		interval: watchIntervalFlag,
		timeout:  watchTimeoutFlag,
		now:      time.Now,
		sleep:    time.Sleep,
	}, watchIncludeCurrentFlag)
	if err != nil {
		return jsonError(err)
	}

	if envelopeOutput {
		return jsonOut(map[string]any{"events": events})
	}
	for _, event := range events {
		fmt.Println(describeWatchEvent(event))
	}
	return nil
}

// listSessionsInScope reads every session the daemon reports, falling back to a
// scoped disk scan when no daemon is reachable — the same read path and the same
// fallback rule the single-title form uses, so the two cannot disagree about what
// "in scope" means.
func listSessionsInScope(repoID string) ([]session.InstanceData, error) {
	data, fallBack, err := snapshotRead(daemon.SnapshotRequest{RepoID: repoID})
	if err != nil {
		// A remote target has no local disk to fall back to; surfacing the daemon's
		// error beats masking it as an empty fleet, which would look like "nothing
		// is happening" and park a driver until its timeout (#1679).
		if !fallBack {
			return nil, err
		}
		return diskListSessions(repoID)
	}
	return data, nil
}
