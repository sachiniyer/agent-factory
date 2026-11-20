package daemon

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	holdKeyA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	holdKeyB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	heldDialogReason = "af is waiting on a folder-trust dialog it can't read (pane 60×12). Widen the pane, or answer it in the session's agent tab."
)

// dialogHoldFixture registers a Ready session and puts nowFunc on a clock the
// test advances, so two attention requests never share a SetAt.
func dialogHoldFixture(t *testing.T) (manager *Manager, repoID, key string, inst *session.Instance, advance func(time.Duration)) {
	t.Helper()
	now := time.Now().UTC()
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	manager, repoID, repoPath := newStatusTestManager(t)
	inst = registerStarted(t, manager, repoID, repoPath, "worker",
		readyFakeBackend{FakeBackend: session.NewFakeBackend()}, true, session.Ready)
	return manager, repoID, stableSessionKey(repoID, inst), inst, func(d time.Duration) { now = now.Add(d) }
}

// feedDialogHold mirrors the poll's order (#7635): the observation is noted
// under the instance's observation-settlement fence, and the write happens
// after the fence releases.
func feedDialogHold(manager *Manager, repoID, key string, inst *session.Instance, hold *session.DialogHold) {
	noteDialogHoldOnly(manager, key, inst, hold)
	manager.flushDialogHold(repoID, key, inst)
}

// noteDialogHoldOnly is the first half of a poll: a snapshot of the session's
// current runtime, and the note of what it observed. A poll whose snapshot
// fails runs only the second half, the flush.
func noteDialogHoldOnly(manager *Manager, key string, inst *session.Instance, hold *session.DialogHold) {
	release := inst.HoldAgentObservationSettlement()
	defer release()
	_, _, generation, _, _, _ := inst.SnapshotAgent()
	manager.noteDialogHold(key, inst, generation, hold)
}

// restartDaemonMemory drops what the tracker holds in memory, which is all a
// daemon restart loses: the request and the flagged key are on the session.
func restartDaemonMemory(manager *Manager) {
	manager.dialogHoldMu.Lock()
	manager.dialogHolds = nil
	manager.dialogHoldMu.Unlock()
}

func rememberedDialogHold(manager *Manager, key string) bool {
	manager.dialogHoldMu.Lock()
	defer manager.dialogHoldMu.Unlock()
	_, ok := manager.dialogHolds[key]
	return ok
}

// heldDialog is the unreadable folder-trust dialog in a 60×12 pane, as the
// held pane named by key.
func heldDialog(key string) *session.DialogHold {
	return &session.DialogHold{
		Kind: tmux.DialogHoldFolderUnreadable, Width: 60, Height: 12,
		Since: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), Key: key,
	}
}

func flaggedKey(inst *session.Instance) string {
	key, _ := inst.DialogHoldFlagged()
	return key
}

// #7635: a settled hold raises the session's attention request with its
// reason, once, marked as the daemon's own. Repeated polls of the same held
// pane change nothing.
func TestIssue7635HeldDialogRaisesAttentionOnceWithItsReason(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "no hold, no flag")

	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	raised := inst.Attention()
	require.NotNil(t, raised, "a settled hold flags the session")
	require.Equal(t, heldDialogReason, raised.Reason)
	require.Equal(t, session.AttentionOriginDialogHold, raised.Origin, "the request records who raised it")
	require.Equal(t, holdKeyA, flaggedKey(inst))

	for i := 0; i < 5; i++ {
		advance(time.Second)
		feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
		again := inst.Attention()
		require.NotNil(t, again)
		require.True(t, again.SetAt.Equal(raised.SetAt), "the same held pane is raised once, not once per poll")
	}
}

// #7635: the flag goes when the hold resolves — an observation with no hold.
func TestIssue7635ResolvedHoldClearsItsOwnAttention(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention())

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "answered, cleared, or changed: the flag this path raised is withdrawn")
	require.Empty(t, flaggedKey(inst), "and the pane is no longer flagged")
	require.True(t, rememberedDialogHold(manager, key), "the raise bound still runs (#7648)")
	advance(10 * time.Second) // the raise interval
	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, rememberedDialogHold(manager, key), "a resolved hold leaves nothing behind once the bound lapses")
}

// #7635, attention ownership: this path clears only the request it raised. A
// request the agent set afterwards survives the hold resolving.
func TestIssue7635ResolvedHoldLeavesAnotherRequestAlone(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention())

	advance(time.Second)
	_, err := inst.SetAttention("the agent's own question", nowFunc())
	require.NoError(t, err)

	feedDialogHold(manager, repoID, key, inst, nil)
	left := inst.Attention()
	require.NotNil(t, left, "the hold resolving must not clear a request it did not raise")
	require.Equal(t, "the agent's own question", left.Reason)
	require.Empty(t, flaggedKey(inst))
}

// #7635: a request already on the row is never overwritten. The notice goes
// up once the row is free and the pane is still held.
func TestIssue7635HeldDialogNeverClobbersAnAttentionRequest(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	_, err := inst.SetAttention("the agent's own question", nowFunc())
	require.NoError(t, err)

	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.Equal(t, "the agent's own question", inst.Attention().Reason, "the notice must not overwrite what the agent asked")
	require.Empty(t, flaggedKey(inst), "a hold that was not flagged is not recorded as flagged")

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	advance(time.Second)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	raised := inst.Attention()
	require.NotNil(t, raised, "the notice lands once the row is free")
	require.Equal(t, heldDialogReason, raised.Reason)
}

// #7635: a user's dismissal sticks. The same held pane is not raised again,
// and the hold a dismissal earns keeps a different key from talking around it
// for its interval (#7648); a pane that changed and settled into a hold again
// past it is a new one.
func TestIssue7635DismissedHeldDialogIsNotRaisedAgain(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention())

	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	for i := 0; i < 5; i++ {
		advance(time.Second)
		feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
		require.False(t, inst.HasAttention(), "a dismissed notice is not re-raised for the same held pane")
	}

	advance(time.Second)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyB))
	require.False(t, inst.HasAttention(), "a new key inside the dismissal's hold does not talk around it")
	advance(30 * time.Second) // the dismissal's hold: three settle times
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyB))
	require.True(t, inst.HasAttention(), "a pane that changed and settled again is a new hold")
}

// #7635 review R4: a dismissal sticks for the life of the held pane, across a
// daemon restart. The restarted daemon remembers nothing, a local gate's
// recorder reports no hold while it settles again, and then reports the same
// pane by the same key: nothing is raised.
func TestIssue7635DismissalSurvivesADaemonRestart(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)

	restartDaemonMemory(manager)
	for i := 0; i < 12; i++ { // the recorder's settle, and then some
		feedDialogHold(manager, repoID, key, inst, nil)
		require.False(t, inst.HasAttention())
		require.Equal(t, holdKeyA, flaggedKey(inst), "the dismissal is not forgotten while the recorder settles")
		advance(time.Second)
	}
	for i := 0; i < 5; i++ {
		feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
		require.False(t, inst.HasAttention(), "the same held pane, dismissed before the restart, is not raised after it")
		advance(time.Second)
	}

	// The hold resolving ends the dismissal with it — but the hold a
	// dismissal earns runs out its interval (#7648), so the same dialog
	// re-held inside it is not raised until the bound has passed.
	feedDialogHold(manager, repoID, key, inst, nil)
	require.Empty(t, flaggedKey(inst))
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.False(t, inst.HasAttention(), "inside the dismissal's hold the same key does not re-raise")
	advance(30 * time.Second) // the dismissal's hold: three settle times
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention(), "the same dialog on a later occasion is a new hold")
}

// #7635 re-review R1: the flagged key never outlives its pane. A flagged
// session is lost and comes back in a new runtime while the daemon also
// restarts — a sandbox re-provisioned across the nightly upgrade. Its new pane
// shows the same dialog, and here even derives the same key, the collision the
// review described. It is a new hold and gets its notice: the key was dropped
// when the session was lost.
func TestIssue7635NewPaneAfterALostRuntimeIsFlaggedAgain(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	first := inst.Attention()
	require.NotNil(t, first)
	require.Equal(t, holdKeyA, flaggedKey(inst))

	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveLost)))
	require.False(t, inst.HasAttention(), "a lost session has no agent to answer")
	require.Empty(t, flaggedKey(inst), "and no pane left to be flagged for")

	restartDaemonMemory(manager)
	inst.SetStatusForTest(session.Ready) // recovered into a new runtime
	advance(time.Minute)
	for i := 0; i < 10; i++ { // the new pane's dialog settling
		feedDialogHold(manager, repoID, key, inst, nil)
		advance(time.Second)
	}
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	raised := inst.Attention()
	require.NotNil(t, raised, "the new pane's hold is surfaced, same key or not")
	require.Equal(t, heldDialogReason, raised.Reason)
	require.False(t, raised.SetAt.Equal(first.SetAt))

	// A dismissal does not cross a replaced runtime either.
	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)
	require.Equal(t, holdKeyA, flaggedKey(inst))
	inst.ClearIdleEvidence() // the runtime is replaced
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention(), "a dismissal belongs to the pane it was made on")
}

// #7635 review R1: a request with af's exact notice wording that the daemon
// did not raise is never adopted and never cleared — not on a quiet session,
// not when a hold is reported under it, not after the hold resolves. It is
// set the way an agent's `af sessions attention --self --reason …` sets one.
// An old row from before the
// marker existed reads the same way: no marker, so not the daemon's.
func TestIssue7635AgentRequestWithTheNoticesWordingIsNeverAdoptedOrCleared(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	// Instance.SetAttention is where Manager.SetAttention lands a control-API
	// request: the reason is the caller's, and there is no origin to pass.
	_, err := inst.SetAttention(heldDialogReason, nowFunc())
	require.NoError(t, err)
	asked := inst.Attention()
	require.NotNil(t, asked)
	require.Equal(t, heldDialogReason, asked.Reason)
	require.Empty(t, asked.Origin, "the control API cannot mark a request as the daemon's")

	stillTheAgents := func(when string) {
		t.Helper()
		now := inst.Attention()
		require.NotNil(t, now, "%s: the agent's request was cleared", when)
		require.True(t, now.SetAt.Equal(asked.SetAt), "%s: the agent's request was replaced", when)
		require.Empty(t, now.Origin, "%s: the agent's request was adopted", when)
	}

	for elapsed := time.Duration(0); elapsed <= 2*dialogHoldRestartGrace; elapsed += time.Second {
		feedDialogHold(manager, repoID, key, inst, nil)
		advance(time.Second)
	}
	stillTheAgents("no hold, well past the grace")
	require.False(t, rememberedDialogHold(manager, key), "a session with no marks is not tracked")

	for i := 0; i < 3; i++ {
		feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
		advance(time.Second)
	}
	stillTheAgents("a hold reported under it")
	require.Empty(t, flaggedKey(inst), "the hold waits for the row; it does not take the agent's request for its own")

	for elapsed := time.Duration(0); elapsed <= 2*dialogHoldRestartGrace; elapsed += time.Second {
		feedDialogHold(manager, repoID, key, inst, nil)
		advance(time.Second)
	}
	stillTheAgents("the hold resolved")
}

// #7635 review R1, the other direction: a daemon-raised notice is recognized
// after a restart by its marker, whatever it says. Here its wording is an
// older release's. With no hold observed for the grace, it is cleared.
func TestIssue7635DaemonNoticeIsRecoveredAfterARestartWhateverItsWording(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	_, err := inst.SetOwnedAttention("af waits on a trust dialog (an older release's words)", session.AttentionOriginDialogHold, nowFunc())
	require.NoError(t, err)
	inst.SetDialogHoldFlagged(holdKeyA)
	restartDaemonMemory(manager)

	// One second short of the grace: the notice stands.
	for elapsed := time.Duration(0); elapsed < dialogHoldRestartGrace; elapsed += time.Second {
		feedDialogHold(manager, repoID, key, inst, nil)
		require.True(t, inst.HasAttention(), "%s after the restart the pane may still be held", elapsed)
		advance(time.Second)
	}
	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "no hold for the whole grace: the dialog is gone, and so is its notice")
	require.Empty(t, flaggedKey(inst))
}

// A notice that came back with the row, on a pane that is still held, is kept
// as it is — the same request, not a second one — and cleared at once when
// that hold resolves.
func TestIssue7635DaemonNoticeIsKeptAcrossARestartWhileThePaneIsHeld(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	before := inst.Attention()
	require.NotNil(t, before)
	restartDaemonMemory(manager)

	feedDialogHold(manager, repoID, key, inst, nil) // the restarted recorder has not settled yet
	advance(10 * time.Second)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	after := inst.Attention()
	require.NotNil(t, after)
	require.True(t, after.SetAt.Equal(before.SetAt), "the same request, not a second one")

	advance(time.Second)
	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "a hold this daemon has seen resolves without the grace")
}

// #7635: a pane that changed and settled into a hold again keeps its one
// request when the words still describe it, and has it replaced when they do
// not. Either way the write is one synchronous step under the attention
// locks: no clear is queued that could land after it.
func TestIssue7635ChangedHeldDialogKeepsOrReplacesItsOwnRequest(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	var queued int
	prevClear := runAttentionClear
	runAttentionClear = func(func()) { queued++ }
	t.Cleanup(func() { runAttentionClear = prevClear })

	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	first := inst.Attention()
	require.NotNil(t, first)

	advance(10 * time.Second) // the raise interval (#7648)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyB))
	kept := inst.Attention()
	require.NotNil(t, kept)
	require.True(t, kept.SetAt.Equal(first.SetAt), "the same words: the request already on the row flags the new hold")
	require.Equal(t, holdKeyB, flaggedKey(inst))

	advance(10 * time.Second)
	narrower := heldDialog(holdKeyA)
	narrower.Width = 41
	feedDialogHold(manager, repoID, key, inst, narrower)
	replaced := inst.Attention()
	require.NotNil(t, replaced)
	require.Equal(t, narrower.Reason(), replaced.Reason)
	require.False(t, replaced.SetAt.Equal(first.SetAt), "different words: this path's request is replaced")
	require.Equal(t, session.AttentionOriginDialogHold, replaced.Origin)

	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention())
	require.Zero(t, queued, "nothing this path does is asynchronous")
}

// #7648 item 3: raises for one session are rate-bounded, whatever keys a
// peer sends. A sandbox that reports a fresh, validly shaped hold key on
// every poll gets at most one raise per settle window — one persist and one
// publish per 10s instead of per poll — and the session stays flagged for the
// pane that was actually raised, not for the peer's latest key.
func TestIssue7648FreshKeyEveryPollRaisesAtMostOncePerInterval(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)

	seen := map[string]bool{}
	raises := 0
	const polls = 35
	for i := 0; i < polls; i++ {
		feedDialogHold(manager, repoID, key, inst, heldDialog(fmt.Sprintf("%032x", i)))
		if flagged := flaggedKey(inst); flagged != "" && !seen[flagged] {
			seen[flagged] = true
			raises++
		}
		advance(time.Second)
	}
	// Raises land at polls 0, 10, 20 and 30: one per 10s raise interval.
	require.LessOrEqual(t, raises, 4, "one raise per settle window whatever keys a peer sends")
	require.Equal(t, fmt.Sprintf("%032x", 30), flaggedKey(inst),
		"the flagged pane is the last one raised, not the peer's latest key")
}

// #7648 item 3: a dismissal holds for at least a minimum interval — three
// settle times — even when a peer sends a new, validly shaped key on every
// poll. The suppression runs from the first poll that saw the notice gone.
func TestIssue7648DismissalSuppressesFreshKeysForTheHold(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention())
	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)

	// One fresh key per poll: none raises while the dismissal's hold runs.
	var lastKey string
	for i := 0; i < 30; i++ {
		advance(time.Second)
		lastKey = fmt.Sprintf("%032x", i+1)
		feedDialogHold(manager, repoID, key, inst, heldDialog(lastKey))
		require.False(t, inst.HasAttention(), "poll %d: a dismissal suppresses fresh keys for the hold", i)
		require.Equal(t, holdKeyA, flaggedKey(inst), "and the session stays flagged for the dismissed pane")
	}

	advance(time.Second)
	feedDialogHold(manager, repoID, key, inst, heldDialog(lastKey))
	require.True(t, inst.HasAttention(), "a genuinely different pane is surfaced once the hold has run")
	require.Equal(t, lastKey, flaggedKey(inst))
}

// #7635 review R3: a peer's `since` decides nothing. A dismissed notice is not
// raised again by a sandbox that sends a different since on every poll, and a
// zero since is no hold at all.
func TestIssue7635MovingOrZeroSinceRaisesNothingNew(t *testing.T) {
	manager, repoID, key, inst, advance := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	_, cleared := inst.ClearAttention(time.Time{})
	require.True(t, cleared)

	for i := 0; i < 10; i++ {
		advance(time.Second)
		moving := heldDialog(holdKeyA)
		moving.Since = moving.Since.Add(time.Duration(i+1) * time.Minute)
		feedDialogHold(manager, repoID, key, inst, moving)
		require.False(t, inst.HasAttention(), "a moving since must not re-raise a dismissed notice")
	}

	zero := heldDialog(holdKeyB)
	zero.Since = time.Time{}
	feedDialogHold(manager, repoID, key, inst, zero)
	require.False(t, inst.HasAttention(), "a zero since is no hold")
	require.Empty(t, flaggedKey(inst), "and reads as the earlier hold resolving")

	malformed := heldDialog("not-a-key\nURGENT")
	feedDialogHold(manager, repoID, key, inst, malformed)
	require.False(t, inst.HasAttention(), "a malformed key is no hold")
}

// #7635: the write never parks the serial poll. With the op lock or the repo
// start lock held — a handoff, a sibling's create — the flag stays owed and
// lands on a later poll.
func TestIssue7635HeldDialogFlagWaitsOutLockContention(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)

	opLock := manager.opLockFor(daemonInstanceKey(repoID, inst.Title))
	opLock.Lock()
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	opLock.Unlock()
	require.False(t, inst.HasAttention(), "contention on the op lock raises nothing and blocks nothing")

	repoStartLock := manager.startLockForRepo(repoID)
	repoStartLock.Lock()
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	repoStartLock.Unlock()
	require.False(t, inst.HasAttention(), "contention on the repo start lock raises nothing and blocks nothing")

	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, inst.HasAttention(), "the flag lands on the next free poll")

	// The same for taking the marks down.
	opLock.Lock()
	feedDialogHold(manager, repoID, key, inst, nil)
	opLock.Unlock()
	require.True(t, inst.HasAttention(), "a contended resolve clears nothing yet")
	feedDialogHold(manager, repoID, key, inst, nil)
	require.False(t, inst.HasAttention(), "and lands on the next free poll")
}

// What one pane's observation leaves owed is never paid on another. A hold's
// flush loses the op lock to a handoff, the handoff replaces the runtime, and
// the replacement's first snapshot fails — so that poll notes nothing and
// still runs the flush. The retired pane's notice must not go up on the
// replacement.
func TestIssue7635DueHoldIsNotRaisedOnAReplacedRuntime(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)

	opLock := manager.opLockFor(daemonInstanceKey(repoID, inst.Title))
	opLock.Lock() // the handoff holds it across the swap
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.False(t, inst.HasAttention())
	require.True(t, rememberedDialogHold(manager, key), "the hold is owed")
	inst.ClearIdleEvidence() // the runtime is replaced
	opLock.Unlock()

	for i := 0; i < 3; i++ { // polls whose snapshot failed: the flush alone
		manager.flushDialogHold(repoID, key, inst)
		require.False(t, inst.HasAttention(), "the retired pane's notice was raised on its replacement")
		require.Empty(t, flaggedKey(inst))
	}
	require.False(t, rememberedDialogHold(manager, key), "what the retired pane owed is dropped")

	// The replacement's own hold is flagged as any other.
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyB))
	require.True(t, inst.HasAttention())
	require.Equal(t, holdKeyB, flaggedKey(inst))

	// The same for marks owed a take-down: a resolve left owed is not run
	// against a runtime that replaced the one observed.
	opLock.Lock()
	feedDialogHold(manager, repoID, key, inst, nil)
	inst.ClearIdleEvidence()
	_, err := inst.SetOwnedAttention("the replacement's own notice", session.AttentionOriginDialogHold, nowFunc())
	require.NoError(t, err)
	opLock.Unlock()
	manager.flushDialogHold(repoID, key, inst)
	require.True(t, inst.HasAttention(), "a resolve owed by the retired runtime cleared the replacement's notice")
}

// The sweep forgets sessions the daemon no longer tracks.
func TestIssue7635DialogHoldSweepDropsDeadSessions(t *testing.T) {
	manager, repoID, key, inst, _ := dialogHoldFixture(t)
	feedDialogHold(manager, repoID, key, inst, heldDialog(holdKeyA))
	require.True(t, rememberedDialogHold(manager, key))

	manager.mu.Lock()
	delete(manager.instances, daemonInstanceKey(repoID, inst.Title))
	manager.mu.Unlock()
	manager.sweepDialogHolds()
	require.False(t, rememberedDialogHold(manager, key), "an entry whose session is gone is swept")
}

// dialogHoldStep alone: every transition of what one poll owes the next.
func TestDialogHoldStep(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	hold := heldDialog(holdKeyA)

	_, keep := dialogHoldStep(dialogHoldEntry{}, nil, "", false, at)
	require.False(t, keep, "no hold and no marks: nothing to remember")

	next, keep := dialogHoldStep(dialogHoldEntry{}, hold, "", false, at)
	require.True(t, keep)
	require.NotNil(t, next.due, "a hold not flagged yet is due")
	require.True(t, next.seenHold)

	next, keep = dialogHoldStep(next, hold, holdKeyA, true, at)
	require.True(t, keep)
	require.Nil(t, next.due, "the pane already flagged is not due again")

	next, _ = dialogHoldStep(next, hold, holdKeyA, false, at)
	require.Nil(t, next.due, "nor after its notice was dismissed")

	// #7648: inside the dismissal's hold a different key raises nothing —
	// while the flagged mark stands, and after it comes down.
	suppressed, _ := dialogHoldStep(next, heldDialog(holdKeyB), holdKeyA, false, at)
	require.Nil(t, suppressed.due, "the dismissal's hold covers a fresh key")
	marksGone, keep := dialogHoldStep(next, nil, "", false, at.Add(5*time.Second))
	require.True(t, keep, "the bound is remembered once the marks are down")
	suppressed, _ = dialogHoldStep(marksGone, heldDialog(holdKeyB), "", false, at.Add(10*time.Second))
	require.Nil(t, suppressed.due, "and still covers the fresh key")
	free, _ := dialogHoldStep(marksGone, heldDialog(holdKeyB), "", false, at.Add(31*time.Second))
	require.NotNil(t, free.due, "until the hold — three settle times — has run")

	changed, _ := dialogHoldStep(next, heldDialog(holdKeyB), holdKeyA, true, at)
	require.NotNil(t, changed.due, "a request live on the row is no dismissal")
	require.False(t, changed.resolveDue, "and nothing is taken down for it here")

	// A hold this daemon saw resolves at once.
	resolved, keep := dialogHoldStep(next, nil, holdKeyA, true, at)
	require.True(t, keep)
	require.True(t, resolved.resolveDue)
	resolved, _ = dialogHoldStep(next, nil, holdKeyA, false, at)
	require.True(t, resolved.resolveDue, "a dismissed notice's key is taken down too")

	// Marks this daemon never saw a hold for wait out the grace.
	waiting, keep := dialogHoldStep(dialogHoldEntry{}, nil, holdKeyA, true, at)
	require.True(t, keep)
	require.False(t, waiting.resolveDue)
	require.True(t, waiting.quietSince.Equal(at))
	waiting, _ = dialogHoldStep(waiting, nil, holdKeyA, true, at.Add(dialogHoldRestartGrace-time.Second))
	require.False(t, waiting.resolveDue)
	require.True(t, waiting.quietSince.Equal(at), "the grace runs from the first quiet observation")
	done, _ := dialogHoldStep(waiting, nil, holdKeyA, true, at.Add(dialogHoldRestartGrace))
	require.True(t, done.resolveDue)

	// A hold seen inside the grace ends the wait.
	seen, _ := dialogHoldStep(waiting, hold, holdKeyA, true, at.Add(5*time.Second))
	require.True(t, seen.seenHold)
	require.True(t, seen.quietSince.IsZero())
	require.Nil(t, seen.due)
}

// #7635, the wire: a sandbox's agent server sends its hold as dialog_hold —
// a kind, two integers, a time and a key, and no text — and omits the field
// when there is none, so an older daemon reading the response sees exactly
// the shape it always did.
func TestIssue7635HeadlessSnapshotCarriesTheDialogHold(t *testing.T) {
	var fields map[string]json.RawMessage

	raw, err := json.Marshal(agentSnapshotResponse{Captured: true})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NotContains(t, fields, "dialog_hold", "no hold, no field")

	raw, err = json.Marshal(agentSnapshotResponse{Captured: true, DialogHold: heldDialog(holdKeyA)})
	require.NoError(t, err)
	fields = nil
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.JSONEq(t,
		`{"kind":"folder-unreadable","width":60,"height":12,"since":"2026-11-19T12:00:00Z","key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		string(fields["dialog_hold"]))
}

// The control API's attention request has no field that could carry an
// origin: a caller cannot claim a request is the daemon's.
func TestIssue7635SetAttentionRequestCannotCarryAnOrigin(t *testing.T) {
	raw, err := json.Marshal(SetAttentionRequest{ID: "x", Title: "t", RepoID: "r", Reason: "why", Clear: true, SetAt: "now"})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NotContains(t, fields, "origin")
	require.Len(t, fields, 6, "a new field on this request needs a decision about ownership; update this test with it")
}

// heldDialogBackend is a local backend whose trust gate reports hold.
type heldDialogBackend struct {
	readyFakeBackend
	hold *session.DialogHold
	// uncaptured makes every snapshot answer without capturing the pane, the
	// way the local backend reports a capture-pane failure.
	uncaptured bool
}

func (b *heldDialogBackend) DialogHold(*session.Instance) *session.DialogHold { return b.hold }

func (b *heldDialogBackend) HasUpdatedObserved(*session.Instance) (updated, hasPrompt bool, content string, baseline, captured, reflow bool) {
	return false, false, "", false, !b.uncaptured, false
}

// A snapshot that did not capture the pane is not evidence its dialog is gone.
// After a restart the daemon's marks wait out a grace and are then taken down
// on "no hold" — but only "no hold" from a pane that was actually looked at.
// Capture failures for longer than the grace leave the notice and the flagged
// key exactly as they were.
func TestIssue7635UncapturedSnapshotsNeverResolveAHold(t *testing.T) {
	now := time.Now().UTC()
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &heldDialogBackend{readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()}}
	inst := registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)

	backend.hold = heldDialog(holdKeyA)
	manager.refreshInstanceStatus(repoID, inst)
	raised := inst.Attention()
	require.NotNil(t, raised)

	// The daemon restarts; the pane cannot be captured, and the restarted
	// recorder has no notice yet.
	restartDaemonMemory(manager)
	backend.hold, backend.uncaptured = nil, true
	for elapsed := time.Duration(0); elapsed <= 2*dialogHoldRestartGrace; elapsed += time.Second {
		manager.refreshInstanceStatus(repoID, inst)
		now = now.Add(time.Second)
	}
	still := inst.Attention()
	require.NotNil(t, still, "uncaptured snapshots took the notice down without observing the pane")
	require.True(t, still.SetAt.Equal(raised.SetAt))
	require.Equal(t, holdKeyA, flaggedKey(inst), "and would have forgotten a dismissal with it")

	// Captures work again and show no hold: now the grace runs, and resolves.
	backend.uncaptured = false
	for elapsed := time.Duration(0); elapsed < dialogHoldRestartGrace; elapsed += time.Second {
		manager.refreshInstanceStatus(repoID, inst)
		require.True(t, inst.HasAttention(), "the grace starts at the first captured no-hold observation")
		now = now.Add(time.Second)
	}
	manager.refreshInstanceStatus(repoID, inst)
	require.False(t, inst.HasAttention(), "a pane observed without its dialog for the whole grace is resolved")
}

// #7635, end to end through the status poll: the hold a backend's gate reports
// rides the observation into the poll, which flags the session, and a later
// poll with no hold clears the flag.
func TestIssue7635StatusPollFlagsAndClearsAHeldDialog(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &heldDialogBackend{readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()}}
	inst := registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)

	manager.refreshInstanceStatus(repoID, inst)
	require.False(t, inst.HasAttention(), "a poll with no hold flags nothing")

	backend.hold = heldDialog(holdKeyA)
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
