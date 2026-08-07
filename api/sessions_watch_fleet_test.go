package api

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

func fleetRunning(title string) session.InstanceData {
	return session.InstanceData{ID: "id-" + title, Title: title, Path: "/w/" + title, Liveness: session.LiveRunning}
}

func withLiveness(title string, l session.Liveness) session.InstanceData {
	d := fleetRunning(title)
	d.Liveness = l
	return d
}

func reasons(events []watchEvent) map[string]watchStopReason {
	out := map[string]watchStopReason{}
	for _, e := range events {
		out[e.Title] = e.Reason
	}
	return out
}

// The defect the whole feature exists for. Arming watchers over already-idle
// sessions returned all of them instantly, so "block until something finishes"
// and "tell me what is finished now" were the same call and a driver re-triggered
// on the same session forever (#3009).
func TestFleetWatcher_FirstSnapshotIsABaselineNotAnAlert(t *testing.T) {
	w := newFleetWatcher(false)

	events := w.observe([]session.InstanceData{
		withLiveness("alpha", session.LiveReady),
		withLiveness("bravo", session.LiveReady),
		withLiveness("charlie", session.LiveReady),
	})
	require.Empty(t, events,
		"three sessions that were ALREADY idle are not three transitions — that is the level-triggered bug")

	// Still nothing while they stay idle: a level would re-fire on every poll.
	require.Empty(t, w.observe([]session.InstanceData{
		withLiveness("alpha", session.LiveReady),
		withLiveness("bravo", session.LiveReady),
		withLiveness("charlie", session.LiveReady),
	}))
}

func TestFleetWatcher_ReportsTheEdgeIntoIdle(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha"), fleetRunning("bravo")}))

	events := w.observe([]session.InstanceData{
		withLiveness("alpha", session.LiveReady),
		fleetRunning("bravo"),
	})
	require.Len(t, events, 1, "only the session that changed is reported")
	require.Equal(t, "alpha", events[0].Title)
	require.Equal(t, watchStopIdle, events[0].Reason)
	require.NotNil(t, events[0].Session, "the record rides along so a driver acts without a second call")

	// The same idle state on the next poll is not a new edge.
	require.Empty(t, w.observe([]session.InstanceData{
		withLiveness("alpha", session.LiveReady),
		fleetRunning("bravo"),
	}))

	// Going back to work and finishing again IS a new edge.
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha"), fleetRunning("bravo")}))
	again := w.observe([]session.InstanceData{withLiveness("alpha", session.LiveReady), fleetRunning("bravo")})
	require.Len(t, again, 1)
	require.Equal(t, watchStopIdle, again[0].Reason)
}

// --include-current is the level query, kept available for a driver that wants a
// starting picture — but it must be asked for.
func TestFleetWatcher_IncludeCurrentReportsTheStartingSnapshot(t *testing.T) {
	w := newFleetWatcher(true)
	events := w.observe([]session.InstanceData{
		withLiveness("alpha", session.LiveReady),
		fleetRunning("bravo"),
		withLiveness("charlie", session.LiveLost),
	})
	require.Equal(t, map[string]watchStopReason{
		"alpha":   watchStopIdle,
		"charlie": watchStopLost,
	}, reasons(events), "the working session is not a stop and is never reported")
}

// A driver responds differently to each of these; without them it must preview the
// pane and read terminal text to guess, which is what #3009 was filed about.
func TestClassifyWatchStop_SaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   session.InstanceData
		reason watchStopReason
	}{
		{"idle", withLiveness("s", session.LiveReady), watchStopIdle},
		{"usage limit auto-resumes", withLiveness("s", session.LiveLimitReached), watchStopUsageLimited},
		{"lost needs restore", withLiveness("s", session.LiveLost), watchStopLost},
		{"dead", withLiveness("s", session.LiveDead), watchStopDead},
		{"archived", withLiveness("s", session.LiveArchived), watchStopArchived},
		{"working is not a stop", fleetRunning("s"), watchWorking},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, detail := classifyWatchStop(tc.data)
			require.Equal(t, tc.reason, reason)
			if tc.reason.stopped() {
				require.NotEmpty(t, detail, "every stop must carry a reason a driver can act on")
			}
		})
	}
}

// The rule that matters most: an idle report tells a driver to ACT, so a
// fabricated one makes it interrupt a working session.
func TestClassifyWatchStop_UnknownIsNeverIdle(t *testing.T) {
	t.Run("unconfirmed startup", func(t *testing.T) {
		d := withLiveness("s", session.LiveReady) // a liveness value that describes nothing
		d.StartupStateUnknown = true
		reason, detail := classifyWatchStop(d)
		require.Equal(t, watchStopUnknown, reason,
			"a record whose startup could not be confirmed must not be read as idle, whatever its liveness says")
		require.NotEmpty(t, detail)
	})

	t.Run("no liveness axis at all", func(t *testing.T) {
		d := session.InstanceData{ID: "x", Title: "s", Path: "/w/s"} // LivenessUnset, zero Status
		reason, _ := classifyWatchStop(d)
		require.Equal(t, watchStopUnknown, reason,
			"a record af cannot classify reports unknown; the single-title path's legacy-status fallback is deliberately not used here")
	})

	t.Run("legacy Status cannot rescue it either", func(t *testing.T) {
		// Running is Status's ZERO VALUE, so "Status == Running" is also true of a
		// record that never had a status set. An earlier version of this code read it
		// as "working"; that was inventing an answer from an unset field rather than
		// falling back to a legacy one, and this case is what caught it.
		explicit := session.InstanceData{ID: "x", Title: "s", Path: "/w/s", Status: session.Running}
		empty := session.InstanceData{ID: "y", Title: "t", Path: "/w/t"}
		require.Equal(t, explicit.Status, empty.Status,
			"premise: an explicit Running is byte-identical to an unset status, so it carries no information")

		reason, _ := classifyWatchStop(explicit)
		require.Equal(t, watchStopUnknown, reason,
			"af cannot tell these apart, so it must not claim to")
	})
}

// A create/kill/archive/restore/handoff in flight is a session in motion. Waking a
// driver for it would report a state that is about to change on its own.
func TestClassifyWatchStop_InFlightOperationsAreNotStops(t *testing.T) {
	for _, op := range []session.InFlightOp{
		session.OpCreating, session.OpKilling, session.OpArchiving,
		session.OpRestoring, session.OpReplacing, session.OpRespawning,
	} {
		d := withLiveness("s", session.LiveReady)
		d.InFlightOp = op
		reason, _ := classifyWatchStop(d)
		require.Equalf(t, watchWorking, reason, "op %v is mid-transition, not a stop", op)
	}
}

// Titles are reused. A kill-and-recreate under the same name is a DIFFERENT
// session, and carrying the dead one's phase onto it would either suppress the new
// session's first real transition or report it the instant it was created.
func TestFleetWatcher_KeysBySessionIdNotTitle(t *testing.T) {
	w := newFleetWatcher(false)
	old := withLiveness("worker", session.LiveReady)
	old.ID = "id-old"
	require.Empty(t, w.observe([]session.InstanceData{old}))

	// Same title, new session, already idle. It is not the session we baselined.
	fresh := withLiveness("worker", session.LiveReady)
	fresh.ID = "id-new"
	events := w.observe([]session.InstanceData{fresh})

	// TWO events, not one, and asserting on a title-keyed map would hide that: the
	// old session is gone and a different session now holds the name. Keying by
	// title anywhere — here or in the watcher — loses exactly this.
	require.Len(t, events, 2, "a recycled title is two facts: one session ended, another arrived")
	byID := map[string]watchStopReason{}
	for _, e := range events {
		byID[e.ID] = e.Reason
	}
	require.Equal(t, watchStopIdle, byID["id-new"],
		"the new session is reported on its own merits rather than inheriting the old one's phase")
	require.Equal(t, watchStopGone, byID["id-old"],
		"and the session that vanished is reported as gone, named by ITS id — a driver seeing only the "+
			"shared title could not tell which of the two records disappeared")
}

func TestFleetWatcher_ReportsASessionThatDisappears(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha"), fleetRunning("bravo")}))

	events := w.observe([]session.InstanceData{fleetRunning("bravo")})
	require.Len(t, events, 1)
	require.Equal(t, "alpha", events[0].Title)
	require.Equal(t, watchStopGone, events[0].Reason)
	require.Nil(t, events[0].Session, "there is no record left to hand a driver")

	// And it is reported once, not on every later poll.
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("bravo")}))
}

// A session created after priming is not news while it works; its stop is reported
// when it happens.
func TestFleetWatcher_SessionAppearingAfterPriming(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha")}))
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha"), fleetRunning("bravo")}),
		"a new session that is working is not a transition")

	events := w.observe([]session.InstanceData{fleetRunning("alpha"), withLiveness("bravo", session.LiveReady)})
	require.Equal(t, map[string]watchStopReason{"bravo": watchStopIdle}, reasons(events))
}

// Several transitions in one poll must come out in a stable order, or a driver
// reading the first line gets whatever the map iteration happened to yield.
func TestFleetWatcher_MultipleTransitionsAreOrdered(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("zulu"), fleetRunning("alpha"), fleetRunning("mike")}))

	events := w.observe([]session.InstanceData{
		withLiveness("zulu", session.LiveReady),
		withLiveness("alpha", session.LiveLost),
		withLiveness("mike", session.LiveReady),
	})
	require.Len(t, events, 3)
	require.Equal(t, []string{"alpha", "mike", "zulu"},
		[]string{events[0].Title, events[1].Title, events[2].Title})
}

// The loop blocks while nothing changes, then returns the transition. This is the
// property the issue asked for in one sentence: "block until any session needs me".
func TestWatchFleet_BlocksUntilSomethingChanges(t *testing.T) {
	polls := 0
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			polls++
			if polls < 4 {
				return []session.InstanceData{fleetRunning("alpha"), withLiveness("bravo", session.LiveReady)}, watchSourceDaemon, nil
			}
			return []session.InstanceData{withLiveness("alpha", session.LiveReady), withLiveness("bravo", session.LiveReady)}, watchSourceDaemon, nil
		},
		interval: time.Second,
		timeout:  time.Hour,
		now:      func() time.Time { return time.Unix(0, 0) },
		sleep:    func(time.Duration) {},
	}

	events, err := watchFleet(deps, false)
	require.NoError(t, err)
	require.Len(t, events, 1, "bravo was already idle at the baseline and is not a transition")
	require.Equal(t, "alpha", events[0].Title)
	require.Equal(t, watchStopIdle, events[0].Reason)
	require.Equal(t, 4, polls, "it kept polling while nothing changed")
}

// Several sessions finishing inside one interval is the normal case for a fleet.
// Reporting one and dropping the rest would lose them permanently — the next call
// re-baselines, so the dropped transitions are never reported at all.
func TestWatchFleet_ReturnsEveryEventFromThePollThatFired(t *testing.T) {
	polls := 0
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			polls++
			if polls == 1 {
				return []session.InstanceData{fleetRunning("alpha"), fleetRunning("bravo"), fleetRunning("charlie")}, watchSourceDaemon, nil
			}
			return []session.InstanceData{
				withLiveness("alpha", session.LiveReady),
				withLiveness("bravo", session.LiveLimitReached),
				fleetRunning("charlie"),
			}, watchSourceDaemon, nil
		},
		interval: time.Second, timeout: time.Hour,
		now:   func() time.Time { return time.Unix(0, 0) },
		sleep: func(time.Duration) {},
	}

	events, err := watchFleet(deps, false)
	require.NoError(t, err)
	require.Equal(t, map[string]watchStopReason{
		"alpha": watchStopIdle,
		"bravo": watchStopUsageLimited,
	}, reasons(events), "both transitions come back; charlie is still working and is not one")
}

func TestWatchFleet_TimesOutRatherThanBlockingForever(t *testing.T) {
	clock := time.Unix(0, 0)
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			return []session.InstanceData{fleetRunning("alpha")}, watchSourceDaemon, nil
		},
		interval: time.Second,
		timeout:  10 * time.Second,
		now:      func() time.Time { return clock },
		sleep:    func(d time.Duration) { clock = clock.Add(d) },
	}
	_, err := watchFleet(deps, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
	require.Contains(t, err.Error(), "1 watched", "the message says how many sessions were being watched")
}

// A read failure must surface, not read as an empty fleet: an empty fleet looks
// like "nothing is happening" and parks a driver until its timeout.
func TestWatchFleet_SurfacesAReadFailure(t *testing.T) {
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			return nil, watchSourceNone, errors.New("daemon unreachable")
		},
		interval: time.Second, timeout: time.Hour,
		now:   func() time.Time { return time.Unix(0, 0) },
		sleep: func(time.Duration) {},
	}
	_, err := watchFleet(deps, false)
	require.ErrorContains(t, err, "daemon unreachable")
}

// Measured on the box this was built against: --include-current reported 440
// archived sessions and 6 idle ones. A starting picture is "what needs me now",
// and an archived session is inert by construction — it cannot change on its own
// and a driver cannot act on it. Burying the actionable lines is the "loop with
// extra steps" outcome the feature exists to avoid.
func TestFleetWatcher_IncludeCurrentOmitsTheArchivedShelf(t *testing.T) {
	w := newFleetWatcher(true)
	snapshot := []session.InstanceData{withLiveness("live-idle", session.LiveReady)}
	for _, name := range []string{"shelf-a", "shelf-b", "shelf-c"} {
		snapshot = append(snapshot, withLiveness(name, session.LiveArchived))
	}

	events := w.observe(snapshot)
	require.Equal(t, map[string]watchStopReason{"live-idle": watchStopIdle}, reasons(events),
		"the shelf is not a starting picture of what needs attention")
}

// But being archived WHILE a driver is watching is news — something shelved a
// session out from under it.
func TestFleetWatcher_ReportsATransitionIntoArchived(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha")}))

	events := w.observe([]session.InstanceData{withLiveness("alpha", session.LiveArchived)})
	require.Equal(t, map[string]watchStopReason{"alpha": watchStopArchived}, reasons(events))
}

// A `gone` event must still identify WHICH session went. In the kill-and-recreate
// case the same poll emits a gone and an arrival under one title, so a driver that
// only has the title cannot tell them apart.
func TestFleetWatcher_GoneEventKeepsSessionIdentity(t *testing.T) {
	w := newFleetWatcher(false)
	require.Empty(t, w.observe([]session.InstanceData{fleetRunning("alpha")}))

	events := w.observe(nil)
	require.Len(t, events, 1)
	require.Equal(t, watchStopGone, events[0].Reason)
	require.Equal(t, "id-alpha", events[0].ID, "the vanished session is named by its stable id")
	require.Equal(t, "/w/alpha", events[0].Path)
}

// Version skew: a newer daemon can send an InFlightOp this build has no name for.
// Matching only the known ops would fall through to a liveness field that is stale
// BECAUSE an operation is running, and report a mid-transition session as idle.
func TestClassifyWatchStop_UnknownInFlightOpIsNotIdle(t *testing.T) {
	d := withLiveness("s", session.LiveReady)
	d.InFlightOp = session.InFlightOp(9999) // from a future daemon
	reason, _ := classifyWatchStop(d)
	require.Equal(t, watchWorking, reason,
		"an operation this build cannot name is still an operation in flight")
}

// MarkUserKilled does not clear StartupStateUnknown, so both are legitimately true
// during teardown. Telling a driver to "inspect it and remove it explicitly" there
// is advice to do what is already happening.
func TestClassifyWatchStop_KillIntentOutranksStartupUncertainty(t *testing.T) {
	d := withLiveness("s", session.LiveReady)
	d.StartupStateUnknown = true
	d.UserKilled = true
	reason, detail := classifyWatchStop(d)
	require.Equal(t, watchStopKilled, reason)
	require.Contains(t, detail, "teardown")
}

// A pre-#1195 record read off disk has no liveness axis, but its non-zero legacy
// Status values are unambiguous and should be used rather than discarded.
func TestClassifyWatchStop_LegacyStatusIsUsedWhereItIsUnambiguous(t *testing.T) {
	legacy := func(st session.Status) session.InstanceData {
		return session.InstanceData{ID: "x", Title: "s", Path: "/w/s", Status: st}
	}
	idle, _ := classifyWatchStop(legacy(session.Ready))
	require.Equal(t, watchStopIdle, idle, "legacy Ready is unambiguous")

	loading, _ := classifyWatchStop(legacy(session.Loading))
	require.Equal(t, watchWorking, loading)

	// Running is the ZERO value, so it cannot be told from a record that never had
	// a status set — the one legacy value that must stay unknown.
	ambiguous, _ := classifyWatchStop(legacy(session.Running))
	require.Equal(t, watchStopUnknown, ambiguous,
		"Running is Status's zero value and carries no information")
}

// --interval and --timeout are validated independently, so an interval longer than
// the timeout is accepted. An unconditional sleep would make the advertised bound a
// fiction: poll once, block for the interval, then report the short timeout.
func TestWatchFleet_NeverSleepsPastTheDeadline(t *testing.T) {
	clock := time.Unix(0, 0)
	var slept []time.Duration
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			return []session.InstanceData{fleetRunning("alpha")}, watchSourceDaemon, nil
		},
		interval: time.Hour,
		timeout:  5 * time.Second,
		now:      func() time.Time { return clock },
		sleep:    func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
	}
	_, err := watchFleet(deps, false)
	require.ErrorContains(t, err, "timed out")
	require.Equal(t, []time.Duration{5 * time.Second}, slept,
		"the wait is clamped to the remaining timeout, not the full hour")
}

func TestDescribeWatchEvent_QualifiesOnlyWhenTitlesCanCollide(t *testing.T) {
	e := watchEvent{Title: "worker", Path: "/w/proj-a/worker", Reason: watchStopIdle, Detail: "d"}
	require.Equal(t, "worker\tidle\td", describeWatchEvent(e, false),
		"a single-project fleet has unique titles and stays short")
	require.Equal(t, "worker\t/w/proj-a/worker\tidle\td", describeWatchEvent(e, true),
		"an all-project fleet can hold two 'worker's, so the path says which")
}

// A newer daemon's Liveness value must fail closed, not fall through to the legacy
// Status axis. That fallback is only valid for a record with NO liveness axis; a
// record that HAS one this build cannot read is not a legacy record, and reading
// its compatibility status could emit `idle` for a state the client does not
// understand.
func TestClassifyWatchStop_UnknownLivenessDoesNotUseTheLegacyFallback(t *testing.T) {
	d := session.InstanceData{ID: "x", Title: "s", Path: "/w/s",
		Liveness: session.Liveness(9999), // from a future daemon
		Status:   session.Ready,          // compatibility value that would say "idle"
	}
	reason, detail := classifyWatchStop(d)
	require.Equal(t, watchStopUnknown, reason,
		"an unreadable liveness must not be rescued by a Status field that says idle")
	require.Contains(t, detail, "upgrade af")
}

// A pre-#1195 record read off disk can carry its terminal state only on the legacy
// axis. Reporting those as `unknown` loses an answer af actually has.
func TestClassifyWatchStop_LegacyTerminalStatuses(t *testing.T) {
	legacy := func(st session.Status) session.InstanceData {
		return session.InstanceData{ID: "x", Title: "s", Path: "/w/s", Status: st}
	}
	lost, _ := classifyWatchStop(legacy(session.Lost))
	require.Equal(t, watchStopLost, lost)

	// Dead rolls forward to lost: deaths have recorded as Lost since #1108, and
	// `lost` is the reason carrying the recovery instruction.
	dead, detail := classifyWatchStop(legacy(session.Dead))
	require.Equal(t, watchStopLost, dead)
	require.Contains(t, detail, "restore")

	archived, _ := classifyWatchStop(legacy(session.Archived))
	require.Equal(t, watchStopArchived, archived)

	// A delete in progress is motion, matching classifyActivityByStatus; its
	// outcome arrives as a `gone` event.
	deleting, _ := classifyWatchStop(legacy(session.Deleting))
	require.Equal(t, watchWorking, deleting)
}

// Switching projections mid-watch would manufacture transitions: a pending create
// lives in the daemon's memory and not on disk, so a daemon->disk fallback reads as
// every such session vanishing, and the return trip as them arriving.
func TestWatchFleet_RefusesToCompareTwoSnapshotSources(t *testing.T) {
	polls := 0
	deps := fleetWatchDeps{
		list: func() ([]session.InstanceData, watchSource, error) {
			polls++
			if polls == 1 {
				return []session.InstanceData{fleetRunning("alpha"), fleetRunning("mid-create")}, watchSourceDaemon, nil
			}
			// The daemon read failed and the disk projection does not know about the
			// in-memory create.
			return []session.InstanceData{fleetRunning("alpha")}, watchSourceDisk, nil
		},
		interval: time.Second, timeout: time.Hour,
		now:   func() time.Time { return time.Unix(0, 0) },
		sleep: func(time.Duration) {},
	}

	events, err := watchFleet(deps, false)
	require.Error(t, err, "it must stop rather than report a create-in-flight as gone")
	require.Empty(t, events)
	require.Contains(t, err.Error(), "switched from")
	require.Contains(t, err.Error(), "re-run")
}

// Same-titled sessions in different projects vanishing in one poll are appended
// while ranging a MAP, so title alone leaves their order randomised.
func TestFleetWatcher_OrderingBreaksTiesOnIdentity(t *testing.T) {
	first := session.InstanceData{ID: "id-a", Title: "worker", Path: "/w/proj-a", Liveness: session.LiveRunning}
	second := session.InstanceData{ID: "id-b", Title: "worker", Path: "/w/proj-b", Liveness: session.LiveRunning}

	// Run it repeatedly: a title-only comparator passes this by luck roughly half
	// the time, so one iteration would not catch the regression.
	for attempt := 0; attempt < 50; attempt++ {
		w := newFleetWatcher(false)
		require.Empty(t, w.observe([]session.InstanceData{first, second}))
		events := w.observe(nil)
		require.Len(t, events, 2)
		require.Equalf(t, []string{"id-a", "id-b"}, []string{events[0].ID, events[1].ID},
			"attempt %d: same-titled sessions must order deterministically", attempt)
	}
}
