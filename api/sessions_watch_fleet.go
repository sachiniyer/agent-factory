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
	// Kill intent outranks startup uncertainty. MarkUserKilled writes the durable
	// tombstone WITHOUT clearing StartupStateUnknown, so both are legitimately true
	// while teardown runs — and the useful thing to tell a driver there is "this is
	// killed", not "inspect it and remove it explicitly", which is advice to do what
	// is already happening.
	if d.UserKilled {
		return watchStopKilled, "killed; its teardown may still be running"
	}
	// A pending account swap is a durable replacement transaction whose mission
	// delivery may not have settled, and it reaches the watch path populated
	// even on a LiveReady record with no operation in flight: the manual
	// delivery path drops the OpRespawning fence before ClearPendingAccountSwap
	// when the prompt cannot be confirmed. Reading such a row as idle would
	// fabricate the one verdict this surface is documented never to emit, and
	// makes the fleet form disagree with the single-title path. Match
	// session.ClassifyActivity, which gates on PendingAccountSwap before the
	// liveness axis for the same reason (#4027).
	//
	// The swap gate holds the slot as `working` only for the in-motion liveness
	// values a mid-swap record carries while its mission settles — LiveRunning,
	// and LiveReady (the latter because the manual delivery path can leave a
	// mid-swap row at LiveReady with no operation in flight). Every other value
	// is a verdict a driver must hear instead of polling until --timeout, so the
	// gate positively names the in-motion set and lets the rest fall through:
	//
	//   - A terminal backing runtime (lost/dead/archived). The status loop
	//     resumes normal probing once a swap leaves LiveLimitReached
	//     (TestRefreshStatuses_PendingAccountSwapDoesNotSuppressNonLimitRows),
	//     so fleet watch reports the terminal reason and the restore
	//     instruction rather than holding the row as working.
	//   - A swap parked at a usage limit on the incoming identity.
	//     ParkManualAccountSwapAtLimit holds PendingAccountSwap with
	//     LiveLimitReached and keeps the OpRespawning fence, so reporting
	//     `working` would suppress the documented `usage-limited` edge and a
	//     driver would never learn the session is parked and must not be
	//     prompted. It is reported below, ahead of the InFlightOp axis, because
	//     that retained fence would otherwise read as in motion and mask it.
	//   - A swap whose replacement vanished before readiness. The delivery path
	//     marks the row StartupStateUnknown while keeping PendingAccountSwap
	//     (TestHandoffAccountReadinessFailureBecomesInert); that row is inert
	//     and operator-action-required, and holding it as `working` would mask
	//     the `unknown` a driver needs to inspect and remove.
	//   - An unrecognized liveness value a newer daemon persists alongside the
	//     swap (e.g. session.Liveness(9999)). It is not an in-motion value this
	//     build knows, so matching it here would return `working` and fleet
	//     watch would time out without telling the driver to upgrade — the
	//     fail-closed guarantee TestClassifyWatchStop_UnknownLivenessDoesNotUseTheLegacyFallback
	//     makes for a non-swap row, inverted on the one path an automated
	//     driver acts on. Leaving it out lets it fall through to the
	//     unknown-liveness branch below.
	if d.PendingAccountSwap != nil &&
		!d.StartupStateUnknown &&
		(d.Liveness == session.LiveRunning || d.Liveness == session.LiveReady) {
		return watchWorking, ""
	}
	if d.StartupStateUnknown {
		return watchStopUnknown,
			"af could not confirm which runtime owns this workspace; inspect it and remove it explicitly before retrying"
	}
	// ParkManualAccountSwapAtLimit leaves PendingAccountSwap populated with
	// LiveLimitReached and does NOT release the OpRespawning fence (the settle
	// path only lowers it on success), so a swap parked at a usage limit reaches
	// the watch path as a row the InFlightOp axis below would otherwise hold as
	// in motion. That suppresses the `usage-limited` edge a driver is owed: the
	// incoming identity is at a quota wall, the scheduler waits for its reset
	// window, and fleet watch would time out without saying the session is
	// parked and must not be prompted. Report it here, ahead of the InFlightOp
	// axis and with the liveness axis's own reason, mirroring how a terminal
	// runtime outranks the swap above. Scoped to PendingAccountSwap because a
	// non-swap row parked at a limit carries OpNone (the park clears the fence),
	// so the InFlightOp axis already lets it fall through to the liveness switch.
	//
	// Restricted to a MANUAL pending swap: ParkManualAccountSwapAtLimit is the
	// only settle path that attributes the wall to the INCOMING identity while
	// keeping the respawn fence, and it requires PendingAccountSwap.Manual. An
	// AUTOMATIC swap resume also reaches the watch path with all three fields
	// set — resumeFromLimitLockedOutcome raises OpRespawning, then
	// SelectAccountAutomatically installs PendingAccountSwap while the ORIGINAL
	// account's LiveLimitReached is still present, before
	// RespawnForAccountSwapWithLiveBoundary clears it — but that row is
	// mid-replacement, not parked: the limit it carries is the outgoing
	// identity's stale value, the replacement has not settled, and reporting
	// `usage-limited` here would make fleet watch return early instead of
	// waiting for the delivery. An AUTOMATIC swap with OpRespawning (the
	// active-resume view) is left to the InFlightOp axis below, which holds it
	// as `working` — the verdict a driver needs while the replacement
	// delivers. An AUTOMATIC swap with OpNone at LiveLimitReached is the
	// crash-recovered pre-respawn view of the same mid-replacement row, and
	// the gate below holds that as `working` too.
	//
	// The override is gated on InFlightOp for the same version-skew reason the
	// InFlightOp axis below is fail-closed: a newer daemon can persist a
	// PendingAccountSwap.Manual row at LiveLimitReached carrying an InFlightOp
	// value this build has no name for, and matching it here would emit
	// `usage-limited` ahead of that axis, returning early on a stale limit
	// while an operation this client cannot read is still running. Restrict the
	// override to the two values a parked swap actually carries — OpRespawning
	// (the fence ParkManualAccountSwapAtLimit keeps) and OpNone (the
	// disk-scrubbed view, or the fence released once the limit lifts) — so any
	// other operation falls through to the InFlightOp axis and is held as
	// working, the version-skew-safe verdict.
	//
	// Gated on ReplacementPanesStarted so the override fires only for a limit
	// observed on the INCOMING identity. Both park paths —
	// ParkManualAccountSwapAtLimit and the manual deliverManualAccountMission
	// settle — run after RespawnForAccountSwapWithLiveBoundary marked the
	// replacement panes started (ValidateAccountSwapReplacementPanes requires
	// it before any settle/deliver), so a parked manual swap always carries it.
	// A manual swap that has NOT started its replacement yet reaches this point
	// with OpRespawning and ReplacementPanesStarted=false: handoffAccount on a
	// session already at LiveLimitReached commits the incoming identity before
	// RespawnForAccountSwapWithLiveBoundary clears the OUTGOING account's stale
	// liveness, so the row carries the outgoing identity's stale limit while
	// the requested replacement is actively executing. Reporting usage-limited
	// there would end fleet watch early instead of waiting for the delivery, so
	// the active-resume view falls through to the InFlightOp axis, which holds
	// OpRespawning as working — the same verdict the automatic active-resume
	// row below gets.
	//
	// ReplacementPanesStarted is still not enough on its own. A manual swap that
	// HAS started its replacement can reach the watch path with the OUTGOING
	// account's stale LiveLimitReached still set: RespawnForAccountSwapWithLiveBoundary
	// marks the replacement panes started, then resumeFromLimitLockedOutcome's
	// restorePendingLiveness re-parks the limit under the resume fence before
	// settleReplacementRuntime has checked or delivered to the incoming runtime
	// (daemon/limit.go; session/liveness.go), and that re-park restores
	// LiveLimitReached without touching limitAccount/limitAgent. The post-respawn
	// snapshot therefore carries PendingAccountSwap.Manual, the outgoing
	// identity's stale LiveLimitReached, OpRespawning (actively delivering), and
	// ReplacementPanesStarted=true — every field this gate names — while the
	// replacement is still executing, not parked. Reporting usage-limited there
	// would let fleet watch return early on the outgoing account's stale wall.
	// Gate the override on the limit's IDENTITY as well: ParkManualAccountSwapAtLimit
	// is the only settle path that attributes the wall to the incoming identity,
	// and it sets limitAccount/limitAgent to the current account/agent, so
	// requiring LimitAccount == Account and LimitAgent == CurrentAgent proves the
	// wall is the committed incoming identity's. The post-respawn active-resume
	// row still carries the outgoing identity's limitAccount/limitAgent, so it no
	// longer matches and falls through to the InFlightOp axis, which holds
	// OpRespawning as working until the replacement delivers — the verdict a
	// driver needs instead of an early return on the outgoing account's stale
	// limit.
	if d.PendingAccountSwap != nil && d.PendingAccountSwap.Manual && d.Liveness == session.LiveLimitReached &&
		d.PendingAccountSwap.ReplacementPanesStarted &&
		d.LimitAccount == d.Account && d.LimitAgent == d.CurrentAgent &&
		(d.InFlightOp == session.OpRespawning || d.InFlightOp == session.OpNone) {
		return watchStopUsageLimited, "blocked on a provider usage limit; af resumes it automatically — do not send it a prompt"
	}
	// A crash-recovered AUTOMATIC swap at the original account's limit keeps
	// PendingAccountSwap populated while its operation fence is gone: a
	// daemon restart immediately after the swap's identity checkpoint strips
	// OpRespawning on the way to disk, and FromInstanceData does not rebuild
	// the fence for an unresolved PendingAccountSwap, so the restored row
	// carries PendingAccountSwap (automatic, so the manual-only override
	// above did not match) with the ORIGINAL account's stale LiveLimitReached
	// and OpNone until the first ResumeLimitedSessions pass re-raises the
	// fence and continues the replacement. Reading that row as
	// `usage-limited` would make fleet watch return early and tell the driver
	// the session is parked at a quota wall when it is not — the replacement
	// is about to resume — so hold it as `working` the way the active-resume
	// path above does.
	//
	// Not every automatic OpNone row at LiveLimitReached with a pending swap
	// is that crash-recovered pre-respawn shape. settleReplacementRuntime
	// re-parks an automatic replacement that itself reached the INCOMING
	// identity's wall at LiveLimitReached while retaining PendingAccountSwap,
	// and the error path's EndLimitResume then lowers the fence to OpNone — a
	// row genuinely parked at the incoming identity's limit that a driver is
	// owed a `usage-limited` edge for while the scheduler waits out its reset
	// window. The two share every field the snapshot carries except
	// ReplacementPanesStarted: the crash-recovered row's replacement never
	// started (the restart hit between the identity checkpoint and
	// RespawnForAccountSwapWithLiveBoundary), while the incoming-limit park's
	// replacement did start (RespawnForAccountSwapWithLiveBoundary completed
	// before settle). Gate the `working` verdict on !ReplacementPanesStarted so
	// the crash-recovered pre-respawn row is still held as working, and the
	// genuine incoming-limit park (ReplacementPanesStarted) falls through to
	// the liveness switch below and reports `usage-limited` the way a settled
	// non-swap row does.
	// Restricted to OpNone because an automatic swap with OpRespawning is the
	// active-resume shape the InFlightOp axis below already holds as
	// `working`, with the same verdict.
	if d.PendingAccountSwap != nil && !d.PendingAccountSwap.Manual &&
		d.Liveness == session.LiveLimitReached && d.InFlightOp == session.OpNone &&
		!d.PendingAccountSwap.ReplacementPanesStarted {
		return watchWorking, ""
	}
	// ANY operation in flight means the session is in motion, including one this
	// binary does not recognise. That last part is the point: a newer daemon can
	// send an InFlightOp value this build has no name for, JSON decoding accepts it
	// happily, and matching only the known ops would fall through to a liveness
	// field that is stale precisely because an operation is running — reporting
	// `idle` for a session mid-transition, which is the fail-closed guarantee
	// inverted. Anything non-zero is therefore in motion, known or not.
	if d.InFlightOp != session.OpNone {
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

	// Anything else on the liveness axis is a value this build has no name for —
	// a newer daemon's. It fails closed HERE rather than falling through, because
	// the legacy fallback below is only valid for a record that has no liveness
	// axis at all. A record that HAS one this client cannot read is not a legacy
	// record, and consulting its compatibility Status could emit `idle` for a state
	// the client does not understand — the fail-closed guarantee inverted, on the
	// one path where an automated driver acts immediately.
	if d.Liveness != session.LivenessUnset {
		return watchStopUnknown,
			"this session reports a state a newer daemon understands and this af build does not; upgrade af to act on it"
	}

	// LivenessUnset: a pre-#1195 record read off disk when no daemon is reachable,
	// or one from an older daemon. Its real state is on the legacy Status axis, and
	// the NON-ZERO values there are unambiguous — Ready, Loading and Deleting each
	// mean one thing and cannot have been left unset.
	//
	// `Running` is the exception, and it is why this does not simply defer to the
	// legacy axis wholesale: it is Status's ZERO VALUE (session/instance.go:
	// `Running Status = iota`), so a record that never had a status set is
	// byte-identical to one explicitly marked running. Reading that as "working"
	// would not be a legacy fallback, it would be inventing an answer out of an
	// unset field — the exact shape this reason exists to refuse.
	switch d.Status {
	case session.Ready:
		return watchStopIdle, "the agent stopped and is awaiting input (from this record's legacy status)"
	case session.Loading, session.Deleting:
		// In motion, matching classifyActivityByStatus. A delete in progress resolves
		// into a `gone` event when the record leaves the snapshot, which is the
		// outcome a driver acts on — reporting it as a stop first would wake one for
		// a session that is on its way out anyway.
		return watchWorking, ""
	case session.Lost:
		return watchStopLost, "its backing tmux/worktree vanished; recover it with 'af sessions restore' (from this record's legacy status)"
	case session.Dead:
		// Rolled forward to `lost`, not reported as its own reason. Status: Dead is
		// legacy — deaths have recorded as Lost since #1108 — and `lost` is the
		// reason that carries the recovery instruction, so a driver reading an old
		// record gets the same actionable answer as one reading a current one.
		return watchStopLost, "its backing runtime is gone; recover it with 'af sessions restore' (from this record's legacy status)"
	case session.Archived:
		return watchStopArchived, "archived and inert; restore it before sending work (from this record's legacy status)"
	}
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
	// identity remembers the last identifying fields seen for a key, so a session
	// that vanishes can still be NAMED in its event. Title alone is not enough: in
	// the kill-and-recreate case the poll emits a `gone` and an arrival under the
	// same title, and without the id a driver cannot tell which of the two records
	// disappeared.
	identity map[string]watchEvent
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
		identity:       map[string]watchEvent{},
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
		w.identity[key] = watchEvent{Title: data.Title, ID: data.ID, Path: data.Path}

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
				gone := w.identity[key]
				gone.Reason = watchStopGone
				gone.Detail = "the session's record disappeared while watching (killed or removed)"
				events = append(events, gone)
			}
			delete(w.phase, key)
			delete(w.identity, key)
		}
	}

	w.primed = true
	// Title, then identity. The `gone` events above are appended while ranging a
	// MAP, so their relative order is randomised — and same-titled sessions from
	// different projects compare equal on title alone, leaving SliceStable to
	// preserve that randomness. A driver reading the first line would then get a
	// different answer run to run for an output documented as deterministic.
	sort.Slice(events, func(i, j int) bool {
		if events[i].Title != events[j].Title {
			return events[i].Title < events[j].Title
		}
		if events[i].ID != events[j].ID {
			return events[i].ID < events[j].ID
		}
		return events[i].Path < events[j].Path
	})
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

// describeWatchEvent renders one event as a tab-separated human line.
//
// qualify appends the worktree path. Session titles are unique within a PROJECT,
// not globally, so an unscoped fleet — run outside a repository, or against a
// remote without --repo — can hold two sessions called `worker`, and both would
// render as the same line with no way to tell which one changed. The path is
// added exactly when the scope permits that collision rather than always, so the
// common single-project output stays short. --json always carries id and path.
func describeWatchEvent(e watchEvent, qualify bool) string {
	line := e.Title
	if qualify && e.Path != "" {
		line += "\t" + e.Path
	}
	line += "\t" + string(e.Reason)
	if e.Detail != "" {
		line += "\t" + e.Detail
	}
	return line
}

// fleetWatchDeps are the injectable dependencies of the fleet loop, so its
// timeout and polling behaviour are testable without a daemon or a wall clock.
type fleetWatchDeps struct {
	// list returns a snapshot AND the source it came from. The source is part of
	// the contract because comparing two sources is worse than not polling at all
	// — see watchFleet.
	list     func() ([]session.InstanceData, watchSource, error)
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
	baseline := watchSourceNone
	for {
		snapshot, source, err := d.list()
		if err != nil {
			return nil, err
		}
		// The baseline pins the SOURCE, not just the state. listSessionsInScope falls
		// back to a disk scan when the daemon read fails, and the two projections are
		// legitimately different views of the same fleet — a pending create exists in
		// the daemon's memory and not yet on disk. Feeding both into one watcher
		// compares them as consecutive states of one fleet, so a transient daemon
		// failure would manufacture `gone` events for every in-memory-only session
		// and `idle`/arrival events when it came back. Those are fabricated
		// transitions, and a driver acts on them immediately.
		//
		// So a switch ends the watch with an error instead. The caller re-runs and
		// gets a fresh, coherent baseline; reporting nothing is recoverable, while
		// reporting a session as gone when it is mid-create is not.
		if baseline == watchSourceNone {
			baseline = source
		} else if source != baseline {
			return nil, fmt.Errorf(
				"stopped watching: the session snapshot switched from the %s to the %s mid-watch, "+
					"and comparing the two would report transitions that did not happen — re-run to start a fresh baseline",
				baseline, source)
		}
		if events := watcher.observe(snapshot); len(events) > 0 {
			return events, nil
		}
		if d.timeout > 0 && !d.now().Before(start.Add(d.timeout)) {
			return nil, fmt.Errorf("timed out after %s waiting for a session stop-state change or disappearance (%d watched)",
				d.timeout, len(snapshot))
		}
		// Never sleep past the deadline. --interval and --timeout are validated
		// independently, so `--timeout 5s --interval 1h` is accepted — and an
		// unconditional sleep would poll once, block for an hour, and then report a
		// five-second timeout, making the advertised bound a fiction.
		wait := d.interval
		if d.timeout > 0 {
			if remaining := start.Add(d.timeout).Sub(d.now()); remaining < wait {
				wait = remaining
			}
		}
		d.sleep(wait)
	}
}

// watchSource names which projection a snapshot came from. Two sources cannot be
// compared as consecutive states of one fleet — see watchFleet.
type watchSource string

const (
	watchSourceNone   watchSource = ""
	watchSourceDaemon watchSource = "daemon snapshot"
	watchSourceDisk   watchSource = "on-disk session records"
)

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
		list:     func() ([]session.InstanceData, watchSource, error) { return listSessionsInScope(repoID) },
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
	// An empty repoID means every project is in scope, which is exactly when titles
	// can collide.
	qualify := repoID == ""
	for _, event := range events {
		fmt.Println(describeWatchEvent(event, qualify))
	}
	return nil
}

// listSessionsInScope reads every session the daemon reports, falling back to a
// scoped disk scan when no daemon is reachable — the same read path and the same
// fallback rule the single-title form uses, so the two cannot disagree about what
// "in scope" means. When the daemon dropped a repo at startup due to a corrupted
// instances.json (#603), it refuses naming the dropped repos rather than silently
// serving a partial fleet, mirroring listSessionsRequest (#730's loud corrupt-file
// contract extended to the wire surface #1029 PR 2 introduced); the watch cleanly
// exits on the error so a driver learns the fleet view is incomplete.
func listSessionsInScope(repoID string) ([]session.InstanceData, watchSource, error) {
	data, skipped, fallBack, err := snapshotRead(daemon.SnapshotRequest{RepoID: repoID})
	if err != nil {
		// A remote target has no local disk to fall back to; surfacing the daemon's
		// error beats masking it as an empty fleet, which would look like "nothing
		// is happening" and park a driver until its timeout (#1679).
		if !fallBack {
			return nil, watchSourceNone, err
		}
		data, err = diskListSessions(repoID)
		if err != nil {
			return nil, watchSourceNone, err
		}
		return data, watchSourceDisk, nil
	}
	if len(skipped) > 0 {
		return nil, watchSourceNone, skippedReposError(skipped)
	}
	return data, watchSourceDaemon, nil
}
