package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// dialogHoldFixture registers a Ready session, runs attention clears inline so
// a test reads their result at once, and puts nowFunc on a clock the test
// advances, so two attention requests never share a SetAt.
func dialogHoldFixture(t *testing.T) (manager *Manager, repoID, key string, inst *session.Instance, tick func()) {
	t.Helper()
	now := time.Now().UTC()
	prevNow, prevClear := nowFunc, runAttentionClear
	nowFunc = func() time.Time { return now }
	runAttentionClear = func(clear func()) { clear() }
	t.Cleanup(func() { nowFunc, runAttentionClear = prevNow, prevClear })

	manager, repoID, repoPath := newStatusTestManager(t)
	inst = registerStarted(t, manager, repoID, repoPath, "worker",
		readyFakeBackend{FakeBackend: session.NewFakeBackend()}, true, session.Ready)
	return manager, repoID, stableSessionKey(repoID, inst), inst, func() { now = now.Add(time.Second) }
}

// feedDialogHold mirrors the poll's order (#7635): the observation is noted
// under the instance's observation-settlement fence, and the attention write
// happens after the fence releases.
func feedDialogHold(manager *Manager, repoID, key string, inst *session.Instance, hold *session.DialogHold) {
	release := inst.HoldAgentObservationSettlement()
	manager.noteDialogHold(key, inst, hold)
	release()
	manager.raiseDueDialogHold(repoID, key, inst)
}

func heldDialog(since time.Time) *session.DialogHold {
	return &session.DialogHold{Subject: "a folder-trust dialog it can't read (pane 60×12)", Since: since}
}

const heldDialogReason = "af is waiting on a folder-trust dialog it can't read (pane 60×12). Widen the pane, or answer it in the session's agent tab."

// #7635: a settled hold raises the session's attention request with its
// reason, once. Repeated polls of the same held pane change nothing.
func TestIssue7635HeldDialogRaisesAttentionOnceWithItsReason(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	hold := heldDialog(time.Now().Add(-time.Minute))

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "no hold, no flag")

	feedDialogHold(manager, repoID, key, inst, hold)
	raised := inst.Attention()
	require.NotNil(t, raised, "a settled hold flags the session")
	require.Equal(t, heldDialogReason, raised.Reason)

	for i := 0; i < 5; i++ {
		tick()
		feedDialogHold(manager, repoID, key, inst, hold)
		again := inst.Attention()
		require.NotNil(t, again)
		require.True(t, again.SetAt.Equal(raised.SetAt), "the same held pane is raised once, not once per poll")
	}
}

// #7635: the flag goes when the hold resolves — an observation with no hold.
func TestIssue7635ResolvedHoldClearsItsOwnAttention(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(time.Now().Add(-time.Minute)))
	require.True(t, inst.HasAttention())

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "answered, cleared, or changed: the flag this path raised is withdrawn")
	manager.dialogHoldMu.Lock()
	_, remembered := manager.dialogHolds[key]
	manager.dialogHoldMu.Unlock()
	require.False(t, remembered, "a resolved hold leaves nothing behind")
}

// #7635, attention ownership: this path clears only the request it raised. A
// request the agent set afterwards survives the hold resolving.
func TestIssue7635ResolvedHoldLeavesAnotherRequestAlone(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(time.Now().Add(-time.Minute)))
	require.True(t, inst.HasAttention())

	tick()
	_, err := inst.SetAttention("the agent's own question", nowFunc())
	require.NoError(t, err)

	feedDialogHold(manager, repoID, key, inst, nil)
	left := inst.Attention()
	require.NotNil(t, left, "the hold resolving must not clear a request it did not raise")
	require.Equal(t, "the agent's own question", left.Reason)
}

// #7635: a request already on the row is never overwritten. The notice goes
// up once the row is free and the pane is still held.
func TestIssue7635HeldDialogNeverClobbersAnAttentionRequest(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	_, err := inst.SetAttention("the agent's own question", nowFunc())
	require.NoError(t, err)
	hold := heldDialog(time.Now().Add(-time.Minute))

	feedDialogHold(manager, repoID, key, inst, hold)
	require.Equal(t, "the agent's own question", inst.Attention().Reason, "the notice must not overwrite what the agent asked")

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	tick()
	feedDialogHold(manager, repoID, key, inst, hold)
	raised := inst.Attention()
	require.NotNil(t, raised, "the notice lands once the row is free")
	require.Equal(t, heldDialogReason, raised.Reason)
}

// #7635: a user's dismissal sticks. The same held pane is not raised again;
// a pane that changed and settled into a hold again is a new one.
func TestIssue7635DismissedHeldDialogIsNotRaisedAgain(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	since := time.Now().Add(-time.Minute)
	feedDialogHold(manager, repoID, key, inst, heldDialog(since))
	require.True(t, inst.HasAttention())

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	for i := 0; i < 5; i++ {
		tick()
		feedDialogHold(manager, repoID, key, inst, heldDialog(since))
		require.False(t, inst.HasAttention(), "a dismissed notice is not re-raised for the same held pane")
	}

	tick()
	feedDialogHold(manager, repoID, key, inst, heldDialog(since.Add(30*time.Second)))
	require.True(t, inst.HasAttention(), "a pane that changed and settled again is a new hold")
}

// #7635: a pane that changed and settled into a hold again, with no clear
// observation in between, replaces the old flag with the new hold's.
func TestIssue7635ChangedHeldDialogReplacesItsOwnFlag(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	since := time.Now().Add(-time.Minute)
	feedDialogHold(manager, repoID, key, inst, heldDialog(since))
	first := inst.Attention()
	require.NotNil(t, first)

	tick()
	narrower := &session.DialogHold{Subject: "a folder-trust dialog it can't read (pane 41×12)", Since: since.Add(20 * time.Second)}
	feedDialogHold(manager, repoID, key, inst, narrower)
	second := inst.Attention()
	require.NotNil(t, second)
	require.Equal(t, narrower.Reason(), second.Reason)
	require.False(t, second.SetAt.Equal(first.SetAt), "the earlier hold's flag was cleared and this one raised")

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "and it is the new flag this path clears")
}

// #7635: after a daemon restart the pane is still held and the row still
// carries the notice. The tracker adopts it rather than raising a second one,
// and clears it when the hold resolves.
func TestIssue7635HeldDialogAdoptsItsOwnNoticeAfterARestart(t *testing.T) {
	manager, repoID, key, inst, tick := dialogHoldFixture(t)
	_, err := inst.SetAttention(heldDialogReason, nowFunc())
	require.NoError(t, err)
	before := inst.Attention()

	tick()
	feedDialogHold(manager, repoID, key, inst, heldDialog(time.Now()))
	after := inst.Attention()
	require.NotNil(t, after)
	require.True(t, after.SetAt.Equal(before.SetAt), "the notice already on the row is kept, not raised again")

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "an adopted notice is cleared when the hold resolves")
}

// #7635: the write never parks the serial poll. With the op lock or the repo
// start lock held — a handoff, a sibling's create — the flag stays owed and
// lands on a later poll.
func TestIssue7635HeldDialogFlagWaitsOutLockContention(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)
	hold := heldDialog(time.Now().Add(-time.Minute))

	opLock := manager.opLockFor(daemonInstanceKey(repoID, inst.Title))
	opLock.Lock()
	feedDialogHold(manager, repoID, key, inst, hold)
	opLock.Unlock()
	require.False(t, inst.HasAttention(), "contention on the op lock raises nothing and blocks nothing")

	repoStartLock := manager.startLockForRepo(repoID)
	repoStartLock.Lock()
	feedDialogHold(manager, repoID, key, inst, hold)
	repoStartLock.Unlock()
	require.False(t, inst.HasAttention(), "contention on the repo start lock raises nothing and blocks nothing")

	feedDialogHold(manager, repoID, key, inst, hold)
	require.True(t, inst.HasAttention(), "the flag lands on the next free poll")
}

// The sweep forgets sessions the daemon no longer tracks.
func TestIssue7635DialogHoldSweepDropsDeadSessions(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(time.Now().Add(-time.Minute)))

	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, inst.Title))
	manager.mu.Unlock()
	manager.sweepDialogHolds()
	manager.dialogHoldMu.Lock()
	_, remembered := manager.dialogHolds[key]
	manager.dialogHoldMu.Unlock()
	require.False(t, remembered, "an entry whose session is gone is swept")
}

// dialogHoldStep alone: every transition of what the poll remembers.
func TestDialogHoldStep(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	setAt := at.Add(time.Minute)
	hold := heldDialog(at)

	next, _, clear := dialogHoldStep(nil, nil)
	require.Nil(t, next)
	require.False(t, clear)

	next, _, clear = dialogHoldStep(nil, hold)
	require.False(t, clear)
	require.NotNil(t, next.due, "a hold not flagged yet is due")
	require.True(t, next.since.IsZero())

	// Still due (the row was owned, or a lock was busy): due again, nothing cleared.
	again, _, clear := dialogHoldStep(next, hold)
	require.False(t, clear)
	require.NotNil(t, again.due)

	flagged := &dialogHoldEntry{since: at, setAt: setAt}
	kept, _, clear := dialogHoldStep(flagged, hold)
	require.False(t, clear)
	require.Nil(t, kept.due, "the hold already flagged is not due again")
	require.True(t, kept.setAt.Equal(setAt))

	next, cleared, clear := dialogHoldStep(flagged, nil)
	require.Nil(t, next)
	require.True(t, clear)
	require.True(t, cleared.Equal(setAt), "a resolved hold clears the request raised for it")

	next, cleared, clear = dialogHoldStep(flagged, heldDialog(at.Add(time.Hour)))
	require.True(t, clear)
	require.True(t, cleared.Equal(setAt))
	require.NotNil(t, next.due, "and the new hold is due")

	// A flag the user dismissed raised no request of this path's to clear.
	next, _, clear = dialogHoldStep(&dialogHoldEntry{since: at}, nil)
	require.Nil(t, next)
	require.False(t, clear)
}

// #7635, the wire: a sandbox's agent server sends its hold as dialog_hold and
// omits the field when there is none, so an older daemon reading the response
// sees exactly the shape it always did.
func TestIssue7635HeadlessSnapshotCarriesTheDialogHold(t *testing.T) {
	since := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var fields map[string]json.RawMessage

	raw, err := json.Marshal(agentSnapshotResponse{Captured: true})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NotContains(t, fields, "dialog_hold", "no hold, no field")

	raw, err = json.Marshal(agentSnapshotResponse{Captured: true, DialogHold: heldDialog(since)})
	require.NoError(t, err)
	fields = nil
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.JSONEq(t, `{"subject":"a folder-trust dialog it can't read (pane 60×12)","since":"2026-11-19T12:00:00Z"}`,
		string(fields["dialog_hold"]))
}

// heldDialogBackend is a local backend whose trust gate reports hold.
type heldDialogBackend struct {
	readyFakeBackend
	hold *session.DialogHold
}

func (b *heldDialogBackend) DialogHold(*session.Instance) *session.DialogHold { return b.hold }

// #7635, end to end through the status poll: the hold a backend's gate reports
// rides the observation into the poll, which flags the session, and a later
// poll with no hold clears the flag.
func TestIssue7635StatusPollFlagsAndClearsAHeldDialog(t *testing.T) {
	prevClear := runAttentionClear
	runAttentionClear = func(clear func()) { clear() }
	t.Cleanup(func() { runAttentionClear = prevClear })

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &heldDialogBackend{readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()}}
	inst := registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)

	manager.refreshInstanceStatus(repoID, inst)
	require.False(t, inst.HasAttention(), "a poll with no hold flags nothing")

	backend.hold = heldDialog(time.Now().Add(-time.Minute))
	manager.refreshInstanceStatus(repoID, inst)
	raised := inst.Attention()
	require.NotNil(t, raised, "the poll flags a session whose gate reports a settled hold")
	require.Equal(t, heldDialogReason, raised.Reason)

	manager.refreshInstanceStatus(repoID, inst)
	require.True(t, inst.Attention().SetAt.Equal(raised.SetAt), "raised once")

	backend.hold = nil
	manager.refreshInstanceStatus(repoID, inst)
	require.False(t, inst.HasAttention(), "the flag clears when the hold resolves")
}
