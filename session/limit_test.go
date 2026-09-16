package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestSetLimitReached sets the LimitReached liveness + reset time and reports
// them through the accessors (#1146). The composed Status stays Ready (there is
// no legacy limit value), so render sites must key off LimitReached, not Status.
func TestSetLimitReached(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 0, 0, 0, time.UTC)
	i := &Instance{}
	i.SetLimitReached(reset)

	require.True(t, i.LimitReached())
	require.Equal(t, LiveLimitReached, i.GetLiveness())
	require.Equal(t, Ready, i.GetStatus(), "LimitReached composes to Ready for legacy readers")

	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset))
}

func TestSetLimitReachedAttributesWallToAccountThatProducedIt(t *testing.T) {
	i := &Instance{Program: tmux.ProgramClaude, Account: "work", accountAutoSelected: true}
	i.SetLimitReached(time.Time{})

	account, ok := i.LimitAccount()
	require.True(t, ok)
	require.Equal(t, "work", account)
	agent, identityAccount, ok := i.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", identityAccount)

	data := i.ToInstanceData()
	require.Equal(t, tmux.ProgramClaude, data.LimitAgent)
	require.Equal(t, "work", data.LimitAccount)
	i.Account = "personal"
	account, _ = i.LimitAccount()
	require.Equal(t, "work", account)
}

func TestLimitAgentFromDataRejectsArbitraryText(t *testing.T) {
	data := InstanceData{
		ID: "limited", Program: tmux.ProgramClaude, Account: "work",
		Liveness: LiveLimitReached, LimitAgent: "client-chosen-text", LimitAccount: "work",
	}
	account, observations := AccountLimitEvidenceFromData(data)

	agent := limitAgentFromData(data, account, observations)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", account)
	require.NotEqual(t, "client-chosen-text", agent)
}

func TestAccountLimitObservationSurvivesClearAndStorage(t *testing.T) {
	reset := time.Date(2026, 8, 10, 17, 0, 0, 0, time.UTC)
	observed := time.Date(2026, 8, 10, 9, 30, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })
	i := &Instance{
		Program: tmux.ProgramClaude, Account: "work", accountAutoSelected: true,
	}
	i.SetLimitReached(reset)
	i.ClearLimitReached()

	want := []AccountLimitObservationData{{
		Agent: tmux.ProgramClaude, Account: "work", ResetAt: reset, ObservedAt: observed,
	}}
	require.Equal(t, want, i.AccountLimitObservations(),
		"clearing current liveness must not make an exhausted identity eligible")

	raw, err := json.Marshal(i.ToInstanceData().ForStorage())
	require.NoError(t, err)
	var stored InstanceData
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, want, stored.AccountLimitObservations,
		"account limit evidence must survive the daemon restart boundary")
}

func TestAccountLimitObservationPreservesSafestReset(t *testing.T) {
	earlier := time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)
	later := earlier.Add(7 * 24 * time.Hour)

	t.Run("later reset is not shortened", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		i.SetLimitReached(later)
		i.ClearLimitReached()
		i.SetLimitReached(earlier)

		require.Equal(t, later, i.AccountLimitObservations()[0].ResetAt,
			"a short-window wall must not erase a still-active longer quota wall")
	})

	t.Run("unknown reset dominates", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		i.SetLimitReached(time.Time{})
		i.ClearLimitReached()
		i.SetLimitReached(later)

		require.True(t, i.AccountLimitObservations()[0].ResetAt.IsZero(),
			"a later timestamp cannot make an indefinite limit observation safe to reuse")
	})
}

// TestSetLimitReached_NoResetTime: a banner with no parseable reset time still
// blocks the session, but LimitResetAt reports no known time.
func TestSetLimitReached_NoResetTime(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Time{})
	require.True(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a zero reset time must report as unknown")
}

// TestSetLimitReached_SkipsTransient: a row mid create/kill teardown must not be
// clobbered into LimitReached (mirrors SetStatusIfNotDeleting).
func TestSetLimitReached_SkipsTransient(t *testing.T) {
	for _, s := range []Status{Loading, Deleting} {
		i := &Instance{}
		i.SetStatusForTest(s)
		i.SetLimitReached(time.Now())
		require.False(t, i.LimitReached(), "%v must not be overwritten by SetLimitReached", s)
		require.Equal(t, s, i.GetStatus())
	}
}

// TestSetLimitReached_SkipsRestore: a usage-limit observation must not destroy
// the OpRestoring fence or its LiveLost liveness while a restore is in flight.
func TestSetLimitReached_SkipsRestore(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "restoring", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.NoError(t, i.Transition(BeginArchive()))
	require.NoError(t, i.Transition(CommitArchive()))
	require.NoError(t, i.Transition(BeginRestore()))
	require.Equal(t, LiveLost, i.GetLiveness())
	require.Equal(t, OpRestoring, i.GetInFlightOp())

	i.SetLimitReached(time.Now().Add(time.Hour))

	require.Equal(t, OpRestoring, i.GetInFlightOp(), "SetLimitReached must preserve an active restore fence")
	require.Equal(t, LiveLost, i.GetLiveness(), "SetLimitReached must preserve restore liveness")
	require.False(t, i.LimitReached(), "an in-flight restore must not be marked limit-reached")
}

// TestClearLimitReached moves a limit-blocked instance back to Running and drops
// the reset time; it is a no-op on a non-limit instance.
func TestClearLimitReached(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.ClearLimitReached()
	require.False(t, i.LimitReached())
	require.Equal(t, LiveRunning, i.GetLiveness())
	_, ok := i.LimitResetAt()
	require.False(t, ok)
	_, ok = i.LimitAccount()
	require.False(t, ok)

	// No-op on a Ready instance.
	r := &Instance{}
	r.SetStatusForTest(Ready)
	r.ClearLimitReached()
	require.Equal(t, Ready, r.GetStatus())
}

// TestLimitResetAt_ClearedWhenNotLimit: once the liveness moves off LimitReached
// the reset time never surfaces, even if the field lingers in memory.
func TestLimitResetAt_ClearedWhenNotLimit(t *testing.T) {
	i := &Instance{}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.SetStatusForTest(Ready) // shim moves liveness to LiveReady, limitResetAt lingers
	require.False(t, i.LimitReached())
	_, ok := i.LimitResetAt()
	require.False(t, ok, "a lingering reset time must not surface once off LimitReached")
}

// TestLimitReached_PersistRoundTrip: a limit-blocked instance serializes its
// liveness + reset time, and the InstanceData survives the JSON round-trip the
// daemon writes to disk / carries in the snapshot — so the badge survives a
// restart (#1146).
func TestLimitReached_PersistRoundTrip(t *testing.T) {
	reset := time.Date(2026, 7, 5, 14, 30, 0, 0, time.UTC)
	i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetLimitReached(reset)
	i.Prompt = "run the nightly report"

	data := i.ToInstanceData()
	require.Equal(t, LiveLimitReached, data.Liveness)
	require.True(t, data.LimitResetAt.Equal(reset))
	require.Equal(t, "run the nightly report", data.Prompt)

	// JSON round-trip (the on-disk + snapshot format).
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, LiveLimitReached, back.Liveness)
	require.True(t, back.LimitResetAt.Equal(reset))
	require.Equal(t, "run the nightly report", back.Prompt)

	// FromInstanceData maps the reset time onto the rebuilt instance's field.
	require.Equal(t, reset, back.LimitResetAt)
}

// TestToInstanceData_NoLimitResetForNormalSession: a Ready session never persists
// a reset time, so omitempty drops the field for every normal row.
func TestToInstanceData_NoLimitResetForNormalSession(t *testing.T) {
	i, err := NewInstance(InstanceOptions{Title: "normal", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetStatusForTest(Ready)
	data := i.ToInstanceData()
	require.True(t, data.LimitResetAt.IsZero(), "a non-limit session must not carry a reset time")
}

// The wall's recorded time is what lets a reader distrust a stale claim
// (#4361): the incident behind it was a reset time carried over from an ambient
// identity while the account it named was answering prompts fine. SetLimitReached
// therefore stamps WHEN af made the observation — on the current-wall field that
// serializes, and on the durable account evidence that survives the clear.
func TestSetLimitReachedStampsWhenAfObservedTheWall(t *testing.T) {
	observed := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(observed.Add(5 * 24 * time.Hour))

	require.True(t, i.limitObservedAt.Equal(observed),
		"the current wall must record when af saw it, got %v", i.limitObservedAt)
	data := i.ToInstanceData()
	require.True(t, data.LimitObservedAt.Equal(observed),
		"the serialized record must carry the sighting time, got %v", data.LimitObservedAt)
	require.Len(t, i.AccountLimitObservations(), 1)
	require.True(t, i.AccountLimitObservations()[0].ObservedAt.Equal(observed),
		"the durable account evidence must carry the same sighting time")
}

// Re-observing the same wall inside one episode must not re-date the sighting:
// a poll ticks over a parked session for days, and if every tick re-stamped the
// field the record would always read "observed just now" — the staleness the
// field exists to show. The zero edge is also what the daemon's persist gate
// keys on to write the stamp exactly once.
func TestSetLimitReachedKeepsFirstSightingWithinAnEpisode(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	second := first.Add(3 * 24 * time.Hour)
	oldClock := instanceNow
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	instanceNow = func() time.Time { return first }
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))

	instanceNow = func() time.Time { return second }
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.True(t, i.limitObservedAt.Equal(first),
		"a re-observation inside the episode must keep the first sighting, got %v", i.limitObservedAt)

	// A new episode — the wall cleared, then observed again — stamps afresh:
	// ClearLimitReached zeroes the field, so the next sighting is a new edge.
	i.ClearLimitReached()
	i.SetLimitReached(first.Add(6 * 24 * time.Hour))
	require.True(t, i.limitObservedAt.Equal(second),
		"a wall observed after the clear is a new sighting, got %v", i.limitObservedAt)
}

// The sighting time rides the same JSON round-trip as the reset time: a daemon
// restart must not make an old observation read as a fresh one.
func TestLimitObservedAtPersistRoundTrip(t *testing.T) {
	observed := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })

	i, err := NewInstance(InstanceOptions{Title: "limited", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	i.SetLimitReached(observed.Add(6 * 24 * time.Hour))

	raw, err := json.Marshal(i.ToInstanceData().ForStorage())
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.True(t, back.LimitObservedAt.Equal(observed),
		"limit_observed_at must survive the disk round-trip, got %v", back.LimitObservedAt)

	back.Path = t.TempDir()
	back.Worktree = GitWorktreeData{RepoPath: back.Path, WorktreePath: back.Path, SessionName: back.Title}
	// An uncertain startup loads INERT — without this, FromInstanceData drives
	// Start(false) and re-spawns the recorded program under real tmux, which a
	// CI runner cannot do for a program it does not install.
	back.StartupStateUnknown = true
	rebuilt, err := FromInstanceData(back)
	require.NoError(t, err)
	require.True(t, rebuilt.limitObservedAt.Equal(observed),
		"the rebuilt instance must keep the original sighting, got %v", rebuilt.limitObservedAt)
}

// A wall observed before the field existed — a pre-#4361 record — loads with an
// unknown sighting time rather than a fabricated one.
func TestLimitObservedAtAbsentInOldRecordLoadsAsUnknown(t *testing.T) {
	reset := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	back := InstanceData{
		Title: "old-record", Path: t.TempDir(), Program: "claude",
		Liveness: LiveLimitReached, LimitResetAt: reset,
		// Load inert: a live local record would drive Start(false) and try to
		// re-spawn the recorded program under real tmux.
		StartupStateUnknown: true,
	}
	back.Worktree = GitWorktreeData{RepoPath: back.Path, WorktreePath: back.Path, SessionName: back.Title}
	rebuilt, err := FromInstanceData(back)
	require.NoError(t, err)
	require.True(t, rebuilt.limitObservedAt.IsZero(),
		"a record without limit_observed_at must load as unknown, not as now")
	require.True(t, rebuilt.ToInstanceData().LimitObservedAt.IsZero())
}

// Clearing the wall drops its sighting time with the rest of the current-wall
// attribution; the durable account evidence keeps its own.
func TestClearLimitReachedDropsTheObservationTime(t *testing.T) {
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.ClearLimitReached()
	require.True(t, i.limitObservedAt.IsZero(),
		"a cleared wall must not carry a sighting time into the next episode")
	require.True(t, i.ToInstanceData().LimitObservedAt.IsZero())
	require.Len(t, i.AccountLimitObservations(), 1,
		"the durable evidence survives — only the current wall's copy clears")
}

// Re-parking under the resume fence carries the SAME wall across a respawn —
// it is a state restore, not a new sighting. Stamping it would freshen a claim
// nothing re-verified, which is the exact lie the field exists to catch.
func TestReparkLimitUnderResumeFenceKeepsTheOriginalSighting(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	second := first.Add(30 * time.Minute)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.NoError(t, i.Transition(BeginRespawn()))

	instanceNow = func() time.Time { return second }
	require.NoError(t, i.ReparkLimitUnderResumeFence(first.Add(5*24*time.Hour)))

	require.True(t, i.limitObservedAt.Equal(first),
		"a re-park restores state; it must not freshen the observation to %v", second)
}

// A handoff that parks the incoming runtime at its wall is a real sighting and
// stamps like one — this is the path that carried the ambient identity's reset
// in the incident.
func TestParkHandoffStampsTheObservationTime(t *testing.T) {
	observed := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{
		Program: tmux.ProgramCodex, Account: "codex4",
		liveness: LiveRunning, inFlightOp: OpReplacing,
	}
	require.NoError(t, i.Transition(ParkHandoff(observed.Add(6*24*time.Hour))))

	require.True(t, i.limitObservedAt.Equal(observed))
	require.True(t, i.ToInstanceData().LimitObservedAt.Equal(observed))
	require.True(t, i.AccountLimitObservations()[0].ObservedAt.Equal(observed))
}

// A manual account swap that finds the replacement's wall is likewise a real
// sighting.
func TestParkManualAccountSwapAtLimitStampsTheObservationTime(t *testing.T) {
	observed := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{
		Program: tmux.ProgramCodex, Account: "codex4",
		liveness: LiveRunning, inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{Manual: true, To: "codex4"},
	}
	require.NoError(t, i.ParkManualAccountSwapAtLimit(observed.Add(6*24*time.Hour)))

	require.True(t, i.limitObservedAt.Equal(observed))
	require.True(t, i.AccountLimitObservations()[0].ObservedAt.Equal(observed))
}

// A poll observation that moves the session OFF the wall — the agent answered
// again, or the runtime was declared lost — ends the sighting episode: the
// stamp belongs to the wall that produced it, and the next wall must stamp its
// own first sighting rather than inherit this one's date (#4361 review).
// ObserveLiveness is the edge every such departure routes through: the daemon
// poll's settles, the remote-loss path's LiveLost, and the client reconcile
// mirroring either.
func TestObserveLivenessLeavingTheWallDropsItsObservationTime(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Hour)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.True(t, i.limitObservedAt.Equal(first))

	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	require.True(t, i.limitObservedAt.IsZero(),
		"leaving the wall must drop its sighting time — it belongs to that wall")
	require.True(t, i.limitResetAt.IsZero())

	// The next wall is a new episode: it stamps its own first sighting.
	instanceNow = func() time.Time { return second }
	i.SetLimitReached(second.Add(24 * time.Hour))
	require.True(t, i.limitObservedAt.Equal(second),
		"a wall after the cleared episode must stamp its own sighting, got %v", i.limitObservedAt)
}

// Re-applying the SAME liveness through the daemon-truth edge is not a
// departure: the client reconcile mirrors LiveLimitReached onto a row already
// parked there, and dropping the stamp on that no-op edge would clear the
// sighting the snapshot just carried (#4361 review).
func TestObserveLivenessReappliedOnTheWallKeepsItsObservationTime(t *testing.T) {
	observed := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return observed }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(observed.Add(5 * 24 * time.Hour))

	require.NoError(t, i.Transition(ObserveLiveness(LiveLimitReached)))
	require.True(t, i.limitObservedAt.Equal(observed),
		"re-observing the same wall is not a departure — the stamp stays")
}

// The account evidence's last-seen refresh is quantized (#4361 review): a
// sighting inside the quantum keeps the recorded stamp, so a poll ticking
// every few seconds cannot force a durable write per tick; a sighting past the
// quantum re-dates the wall — and the persist gate's evidence compare makes
// that re-dating reach disk.
func TestRepeatObservationRefreshIsQuantized(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	inside := first.Add(accountObservationRefreshQuantum - time.Second)
	past := first.Add(accountObservationRefreshQuantum + time.Second)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	i.ClearLimitReached()

	instanceNow = func() time.Time { return inside }
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.True(t, i.AccountLimitObservations()[0].ObservedAt.Equal(first),
		"a repeat sighting inside the quantum must not re-date the evidence")

	i.ClearLimitReached()
	instanceNow = func() time.Time { return past }
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.True(t, i.AccountLimitObservations()[0].ObservedAt.Equal(past),
		"a sighting past the quantum refreshes when af last saw the wall")
}

// UpdatedAt is the mutation stamp storage/archive reconciliation reads as proof
// of real state change, so a repeat sighting may only advance it when the
// RETAINED evidence actually changed (#4409 review): the poll re-observes a
// parked session every few seconds, and stamping each sighting would let an
// unrelated later checkpoint persist a synthetic mutation time — the row's
// evidence slice compares equal, so the persist gate never wrote it, yet
// reconcilers still saw the row as freshly mutated.
func TestRepeatObservationTouchesUpdatedAtOnlyOnEvidenceChange(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	inside := first.Add(accountObservationRefreshQuantum - time.Second)
	past := first.Add(accountObservationRefreshQuantum + time.Second)
	later := past.Add(time.Second)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })
	reset := first.Add(5 * 24 * time.Hour)

	i := &Instance{}
	i.mu.Lock()
	i.recordAccountLimitObservationLocked(tmux.ProgramCodex, "work", reset)
	i.mu.Unlock()
	require.True(t, i.UpdatedAt.Equal(first), "the first sighting is a real mutation")

	// Same wall inside the quantum: nothing the record keeps changed.
	instanceNow = func() time.Time { return inside }
	i.mu.Lock()
	i.recordAccountLimitObservationLocked(tmux.ProgramCodex, "work", reset)
	i.mu.Unlock()
	require.True(t, i.UpdatedAt.Equal(first),
		"an unchanged repeat sighting is not a mutation — UpdatedAt must not advance")

	// Past the quantum the ObservedAt refresh IS retained evidence.
	instanceNow = func() time.Time { return past }
	i.mu.Lock()
	i.recordAccountLimitObservationLocked(tmux.ProgramCodex, "work", reset)
	i.mu.Unlock()
	require.True(t, i.UpdatedAt.Equal(past),
		"a retained ObservedAt refresh must stamp the mutation it persists")

	// A reset the merge retains is evidence change even inside the quantum.
	instanceNow = func() time.Time { return later }
	i.mu.Lock()
	i.recordAccountLimitObservationLocked(tmux.ProgramCodex, "work", reset.Add(time.Hour))
	i.mu.Unlock()
	require.True(t, i.UpdatedAt.Equal(later),
		"a retained reset change must stamp the mutation it persists")
}

// A repeat sighting of the same account wall refreshes when af last saw it,
// while the conservative reset merge still keeps the safer boundary.
func TestRepeatObservationRefreshesTheSightingNotTheReset(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Hour)
	laterReset := first.Add(5 * 24 * time.Hour)
	earlierReset := first.Add(2 * 24 * time.Hour)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(laterReset)
	i.ClearLimitReached()
	instanceNow = func() time.Time { return second }
	i.SetLimitReached(earlierReset)

	observations := i.AccountLimitObservations()
	require.Len(t, observations, 1)
	require.True(t, observations[0].ObservedAt.Equal(second),
		"seeing the wall again must refresh when af last saw it")
	require.True(t, observations[0].ResetAt.Equal(laterReset),
		"a shorter second window must not shorten the durable one")
}

// ObserveLiveness is not the only way off the wall: the archive outcomes and a
// spawn completing also move a parked session out of LiveLimitReached without
// any poll observation, and the episode ends there too — the same in-memory
// record's next wall must stamp its own sighting rather than inherit this
// one's date (#4361 review).
func TestNonPollEdgesLeavingTheWallDropItsObservationTime(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	// A committed archive ends the wall's episode.
	committed := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	committed.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.NoError(t, committed.Transition(BeginArchive()))
	require.NoError(t, committed.Transition(CommitArchive()))
	require.True(t, committed.limitObservedAt.IsZero(),
		"committing a parked session to the archive must drop the wall's sighting")
	require.True(t, committed.limitResetAt.IsZero())

	// So does the failed archive's landing at Lost.
	aborted := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	aborted.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.NoError(t, aborted.Transition(BeginArchive()))
	require.NoError(t, aborted.Transition(AbortArchiveToLost()))
	require.True(t, aborted.limitObservedAt.IsZero(),
		"a parked session whose archive fails to Lost must drop the wall's sighting")
	require.True(t, aborted.limitResetAt.IsZero())

	// And a non-resume spawn completing off the wall.
	completed := &Instance{
		Program: tmux.ProgramClaude, Account: "work",
		liveness: LiveLimitReached, inFlightOp: OpCreating,
		limitObservedAt: first, limitResetAt: first.Add(5 * 24 * time.Hour),
	}
	require.NoError(t, completed.Transition(ConfirmLive()))
	require.True(t, completed.limitObservedAt.IsZero(),
		"a spawn completing off the wall without a resume fence must drop the sighting")
	require.True(t, completed.limitResetAt.IsZero())
}

// The one deliberate retain on the completion edge: a spawn coming up while
// OpRespawning is held IS the limit resume's runtime arriving —
// ReparkLimitUnderResumeFence still needs this episode's metadata, so
// ConfirmLive keeps it exactly there (#4361 review).
func TestConfirmLiveUnderResumeFenceKeepsTheWallMetadata(t *testing.T) {
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return first }
	t.Cleanup(func() { instanceNow = oldClock })

	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	i.SetLimitReached(first.Add(5 * 24 * time.Hour))
	require.NoError(t, i.Transition(BeginRespawn()))
	require.NoError(t, i.Transition(ConfirmLive()))

	require.True(t, i.limitObservedAt.Equal(first),
		"the resume's own completion must keep the episode's sighting for the re-park")
	require.True(t, i.limitResetAt.Equal(first.Add(5*24*time.Hour)),
		"the resume's own completion must keep the episode's reset for the re-park")
	require.Equal(t, OpRespawning, i.GetInFlightOp(),
		"ConfirmLive under the fence keeps the op until the re-park")
}
