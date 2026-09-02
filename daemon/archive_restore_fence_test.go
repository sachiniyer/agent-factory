package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// #3596, the archived-restore half of #3586. `claimRestoreOperation` makes Kill
// fail the instant an archived restore is admitted, but on the LOCAL route the
// OpRestoring fence goes up much later — inside RestoreFromArchive, after the
// relocation claim, the repo-gone guard, the destination derivation and the
// worktree relocate itself. `canKillFor` never consults liveness, so an archived
// row advertises Kill, and one pressed in that window is refused at the admission
// gate with the misleading "operation already in progress".
//
// The fence here is MarkRestoring, not BeginRestore (#3596 triage, option 1): op
// axis only, liveness stays LiveArchived. That is load-bearing rather than
// convenient — ShownArchived's own doc spells out why an eager LIVENESS flip
// would be the #1203 regression, since the snapshot reconcile keys its
// Archived->live rebuild on seeing that exact transition.
//
// Every refusal branch below therefore has two things to prove, and they are
// different things: the fence comes DOWN (or the row is permanently busy, worse
// than the bug) and the row is still ARCHIVED with its archive intact (or a failed
// restore has silently unshelved the user's session).

// archivedFenceProbe is what a client would see if it asked at a given moment.
type archivedFenceProbe struct {
	canKill        bool
	canKillProject bool
	liveness       session.Liveness
	shownArchived  bool
	isArchived     bool
}

func probeArchivedFence(inst *session.Instance) archivedFenceProbe {
	data := inst.ToInstanceData()
	return archivedFenceProbe{
		canKill:        inst.CanKill(),
		canKillProject: data.CanKill,
		liveness:       inst.GetLiveness(),
		shownArchived:  inst.ShownArchived(),
		isArchived:     inst.IsArchived(),
	}
}

// seedArchivedForFence registers a local session and archives it. Kill is verified to be on offer first: canKillFor
// refuses an id-less row outright, so without this the assertions below could
// pass for the wrong reason.
func seedArchivedForFence(t *testing.T, title string) (*Manager, string, string, *session.Instance) {
	t.Helper()
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, title)
	inst.SetBackend(&recoverFakeBackend{FakeBackend: session.NewFakeBackend()})

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: title, RepoID: repoID})
	require.NoError(t, err)
	require.True(t, inst.CanKill(), "setup: a settled archived row advertises Kill before any restore claims it")
	require.True(t, inst.ShownArchived(), "setup: it sits in the Archived section")
	return manager, repoID, repoPath, inst
}

// assertArchivedFenceReleased is the shared post-condition for every refusal
// branch: the fence is down, the row is back where it was, and the archive that
// was never moved is still on disk.
func assertArchivedFenceReleased(t *testing.T, inst *session.Instance, archivePath, after string) {
	t.Helper()
	require.Equalf(t, session.OpNone, inst.GetInFlightOp(),
		"the restore fence was left raised after %s: the row is permanently busy — the poll skips any "+
			"session with an op in flight and every runtime action and lifecycle control refuses it", after)
	require.Truef(t, inst.CanKill(), "Instance.CanKill() is still false after %s: the row cannot be removed", after)
	require.Truef(t, inst.ToInstanceData().CanKill,
		"the projected can_kill is still false after %s: no web client can remove the row", after)
	require.Equalf(t, session.LiveArchived, inst.GetLiveness(),
		"liveness = %v after %s, want LiveArchived: a FAILED restore must leave the session shelved, "+
			"not silently unshelved", inst.GetLiveness(), after)
	require.Truef(t, inst.ShownArchived(),
		"the row did not drop back into the Archived section after %s", after)
	require.Truef(t, exists(archivePath), "the archive was not left intact after %s", after)
	require.NoErrorf(t, inst.ValidateRuntimeAction(session.RuntimeActionRestoreArchived),
		"the refused row is no longer restorable after %s", after)
}

// TestRestoreArchived_KillIsHiddenAcrossTheLocalRelocate is #3596 asserted where
// it reproduces: the restore is parked at the relocate boundary and the assertion
// is made from outside, as a user pressing Kill at that instant would see it.
func TestRestoreArchived_KillIsHiddenAcrossTheLocalRelocate(t *testing.T) {
	manager, repoID, _, inst := seedArchivedForFence(t, "archived-fenced")

	entered := make(chan struct{})
	release := make(chan struct{})
	var atPath, atUse archivedFenceProbe
	prevPath, prevUse := beforeRestoreWorktreePath, beforeRestoreWorktreeUse
	beforeRestoreWorktreePath = func() { atPath = probeArchivedFence(inst) }
	beforeRestoreWorktreeUse = func() {
		atUse = probeArchivedFence(inst)
		close(entered)
		<-release
	}
	t.Cleanup(func() { beforeRestoreWorktreePath, beforeRestoreWorktreeUse = prevPath, prevUse })

	done := make(chan error, 1)
	go func() {
		_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-fenced", RepoID: repoID})
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the archived restore never reached the relocate boundary")
	}

	// The assertion a client makes: right now, is Kill on offer?
	live := probeArchivedFence(inst)
	assert.False(t, live.canKill,
		"Instance.CanKill() is true while the archived restore is parked at its worktree relocate: the "+
			"restore claim already refuses Kill at the admission gate, so the TUI menu offers a Kill whose "+
			"only outcome is the misleading \"operation already in progress\" refusal (#3533/#3596)")
	assert.False(t, live.canKillProject,
		"the projected can_kill is true mid-relocate: the web client renders a Kill control for a session "+
			"whose restore has already claimed it (#3533/#3596)")
	assert.False(t, atPath.canKill, "Kill was still on offer at destination derivation, before the relocate")
	assert.False(t, atUse.canKill, "Kill was still on offer at the relocate boundary")

	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("RestoreArchived never returned after the relocate was released")
	}

	assert.Equal(t, session.OpNone, inst.GetInFlightOp(), "a completed restore leaves no fence behind")
	assert.True(t, inst.CanKill(), "and the restored row is removable again")
}

// TestRestoreArchived_FenceKeepsLivenessArchivedAndReHomesTheRow is the "check,
// don't assume" half of the #3596 triage, and it settles which predicate moves.
//
// ShownArchived is `liveness == LiveArchived && inFlightOp != OpRestoring` — the
// RENDER predicate — so the early overlay re-homes the row into the live
// Instances section for the whole relocate. That is the INTENDED reading, not a
// side effect: #1210 made an OpRestoring overlay re-home the row eagerly as "the
// visible feedback the archive epic owes restore", and raising it at the claim
// simply starts that feedback when the restore actually starts.
//
// IsArchived is `liveness == LiveArchived` ALONE — it does not read the op axis,
// contrary to the premise this issue was filed on — so it stays true across the
// relocate and the inert-state gates it guards (tab spawn, tab close/arrange, web
// tab serve, conversation capture, PR-info refresh) are unaffected. That is
// strictly better than the status quo it preserves: those gates stay CLOSED
// through the mid-move window rather than opening early. No narrowing is owed.
func TestRestoreArchived_FenceKeepsLivenessArchivedAndReHomesTheRow(t *testing.T) {
	manager, repoID, _, inst := seedArchivedForFence(t, "archived-rehomed")

	var atUse archivedFenceProbe
	prev := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() { atUse = probeArchivedFence(inst) }
	t.Cleanup(func() { beforeRestoreWorktreeUse = prev })

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-rehomed", RepoID: repoID})
	require.NoError(t, err)

	assert.Equal(t, session.LiveArchived, atUse.liveness,
		"the early fence flipped LIVENESS during the relocate: the snapshot reconcile keys its "+
			"Archived->live rebuild on seeing that exact transition, so an eager flip makes it see "+
			"live->live and SKIP the rebuild, stranding the row live-but-not-started (#1203)")
	assert.False(t, atUse.shownArchived,
		"the row stayed in the Archived section for the whole relocate: an OpRestoring overlay is "+
			"supposed to re-home it into the live section eagerly, which is the visible feedback a "+
			"restore owes the user (#1210)")
	assert.True(t, atUse.isArchived,
		"IsArchived went false mid-relocate: it reads the liveness axis alone and gates the inert-state "+
			"checks (tab spawn, web tab serve), which must stay closed while the worktree is in motion")
}

// TestRestoreArchived_ClaimFailureLowersTheFence covers the first early return
// past the raise: the relocation claim cannot resolve the archived worktree.
func TestRestoreArchived_ClaimFailureLowersTheFence(t *testing.T) {
	manager, repoID, _, inst := seedArchivedForFence(t, "archived-claim-unknown")
	archivePath := inst.GetWorktreePath()
	t.Cleanup(sessiongit.SetRelocationIdentityErrorForTest(archivePath, context.DeadlineExceeded))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-claim-unknown", RepoID: repoID})
	require.Error(t, err)
	assertArchivedFenceReleased(t, inst, archivePath, "the relocation-claim failure")
}

// TestRestoreArchived_RepoGoneLowersTheFence covers the early repo-gone guard,
// whose whole contract is that the archive is left intact and recoverable by hand.
func TestRestoreArchived_RepoGoneLowersTheFence(t *testing.T) {
	manager, repoID, repoPath, inst := seedArchivedForFence(t, "archived-repo-gone")
	archivePath := inst.GetWorktreePath()
	require.NoError(t, os.RemoveAll(repoPath))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-repo-gone", RepoID: repoID})
	require.Error(t, err)
	assertArchivedFenceReleased(t, inst, archivePath, "the repo-gone refusal")
}

// TestRestoreArchived_PathDerivationFailureLowersTheFence covers the
// RestoreWorktreePath failure: the origin survives the guard and disappears
// before the destination can be derived.
func TestRestoreArchived_PathDerivationFailureLowersTheFence(t *testing.T) {
	manager, repoID, repoPath, inst := seedArchivedForFence(t, "archived-path-fails")
	archivePath := inst.GetWorktreePath()

	prev := beforeRestoreWorktreePath
	beforeRestoreWorktreePath = func() { require.NoError(t, os.RemoveAll(repoPath)) }
	t.Cleanup(func() { beforeRestoreWorktreePath = prev })

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-path-fails", RepoID: repoID})
	require.Error(t, err)
	assertArchivedFenceReleased(t, inst, archivePath, "the destination-derivation failure")
}

// TestRestoreArchived_RelocateFailureLowersTheFence covers the relocate itself
// failing with the destination occupied — the generic unresolved-claim exit.
func TestRestoreArchived_RelocateFailureLowersTheFence(t *testing.T) {
	manager, repoID, repoPath, inst := seedArchivedForFence(t, "archived-relocate-fails")
	archivePath := inst.GetWorktreePath()
	destination, err := sessiongit.RestoreWorktreePath(repoPath, "archived-relocate-fails", inst.GetBranch())
	require.NoError(t, err)

	prev := beforeRestoreWorktreeUse
	beforeRestoreWorktreeUse = func() {
		require.NoError(t, os.Mkdir(destination, 0o755), "occupy the selected destination")
	}
	t.Cleanup(func() { beforeRestoreWorktreeUse = prev })

	_, _, err = manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-relocate-fails", RepoID: repoID})
	require.Error(t, err)
	assertArchivedFenceReleased(t, inst, archivePath, "the relocate failure")
}

// TestRestoreArchived_RelocateCutOffLowersTheFence covers the
// ErrRelocateStateUnknown branch: the bytes may have reached either pathname, so
// the row keeps both handles and stays archived. The fence must still come down.
//
// This one deliberately does NOT assert the archive is at its original path — the
// whole point of the branch is that the location is UNKNOWN — so it checks the
// fence and the liveness directly rather than through the shared helper.
func TestRestoreArchived_RelocateCutOffLowersTheFence(t *testing.T) {
	manager, repoID, _, inst := seedArchivedForFence(t, "archived-cutoff")

	_ = partialRelocateGitOnPath(t)
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(300 * time.Millisecond))

	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-cutoff", RepoID: repoID})
	require.Error(t, err)
	require.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)

	assert.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"a cut-off relocate left the restore fence raised: the row is permanently busy, and it is exactly "+
			"the row an operator now has to inspect and retry")
	assert.True(t, inst.CanKill(), "and it cannot be removed")
	assert.Equal(t, session.LiveArchived, inst.GetLiveness(),
		"a cut-off relocate must leave the session shelved, not silently unshelved")
	assert.True(t, inst.ShownArchived(), "and back in the Archived section")
}

// TestRestoreArchived_AnnouncesTheFenceAndItsRelease is the events-plane contract
// #3597 established, on this route. The order against the admission claim is the
// same invariant one level up: killsInFlight is what actually refuses Kill, so no
// event may say can_kill=true while it is still held.
func TestRestoreArchived_AnnouncesTheFenceAndItsRelease(t *testing.T) {
	manager, repoID, repoPath, inst := seedArchivedForFence(t, "archived-events")
	archivePath := inst.GetWorktreePath()

	// A refusal, so the release is the ONLY thing that can announce the settled row:
	// none of the failure helpers publish.
	prev := beforeRestoreWorktreePath
	beforeRestoreWorktreePath = func() { require.NoError(t, os.RemoveAll(repoPath)) }
	t.Cleanup(func() { beforeRestoreWorktreePath = prev })

	_, events := manager.events.subscribe()
	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "archived-events", RepoID: repoID})
	require.Error(t, err)

	published := drainSessionEvents(t, events)
	require.GreaterOrEqual(t, len(published), 2,
		"clients that did not initiate this restore learn of it only from the events plane, and the status "+
			"poll cannot repair the gap because it skips any session with an op in flight")
	assert.Equal(t, session.OpRestoring, published[0].InFlightOp,
		"the fence was never announced, so an already-open TUI or browser kept offering Kill for the "+
			"whole relocate")
	for i, ev := range published[:len(published)-1] {
		assert.Falsef(t, ev.CanKill,
			"session.updated #%d of %d carries can_kill=true while the restore claim is still held: the "+
				"admission gate would refuse that Kill", i+1, len(published))
	}
	released := published[len(published)-1]
	assert.Equal(t, session.OpNone, released.InFlightOp, "the release was never announced")
	assert.True(t, released.CanKill, "the release announcement leaves the row unremovable on every client")
	assert.Equal(t, session.LiveArchived, released.Liveness, "a refused restore stays archived")

	manager.mu.Lock()
	claims := len(manager.killsInFlight)
	manager.mu.Unlock()
	assert.Zero(t, claims, "the release event promised a killable row while the claim was still held")

	assertArchivedFenceReleased(t, inst, archivePath, "the announced refusal")
}

// TestRestoreArchived_RemoteRouteRaisesNoEarlyFence pins the scope decision. The
// remote route reaches RestoreFromArchive as its first statement after the claim,
// op-lock and re-validate, so its unfenced prefix is microseconds and #3596 leaves
// it alone. The observable difference is the announcement: the local route
// publishes a {LiveArchived, OpRestoring} snapshot before its relocate, and this
// route must publish no such thing.
func TestRestoreArchived_RemoteRouteRaisesNoEarlyFence(t *testing.T) {
	withRemoteLossThresholds(t, 3, time.Minute, time.Second)
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, backend := registerStartedRemote(t, manager, repoID, repoPath, "remote-archived", "http://127.0.0.1:1", session.Running)
	inst.SetStatusForTest(session.Archived)

	_, events := manager.events.subscribe()
	_, _, err := manager.RestoreArchived(RestoreArchivedRequest{Title: "remote-archived", RepoID: repoID})
	require.NoError(t, err)
	require.Equal(t, 1, backend.recoverCalls(), "the remote route re-provisions through RestoreFromArchive")

	for _, ev := range drainSessionEvents(t, events) {
		assert.Falsef(t, ev.Liveness == session.LiveArchived && ev.InFlightOp == session.OpRestoring,
			"the remote archived restore announced an early {LiveArchived, OpRestoring} fence: #3596 is "+
				"scoped to the local route, whose relocate is the only prefix long enough to matter")
	}
}
