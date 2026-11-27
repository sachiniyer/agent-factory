package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #7793: the agent-server's swap recorder survives its own restart — the
// durable terminal record + discard tombstone it persists in the session dir
// rehydrate into a fresh process, so the durable in-flight record's restart
// reconcile gets THIS swap's own verdict back rather than the anonymous "none"
// a fresh recorder answers (which a settled host cannot tell from "still
// queued" and fences the row forever).

// A save+load round-trips the terminal record + tombstone, and the file is
// written atomically (a concurrent reader never sees a torn write).
func TestAccountSwapStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	require.NoError(t, store.save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1", Cancelled: "doomed",
	}))
	d, err := store.load()
	require.NoError(t, err)
	assert.Equal(t, accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1", Cancelled: "doomed",
	}, d)
}

// A corrupted record file fails the load rather than silently rehydrating
// garbage — the agent-server refuses to start on a record it cannot trust,
// rather than guessing a verdict.
func TestAccountSwapStoreLoadCorruptFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, accountSwapRecordMarkerName("key"))
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	store := newAccountSwapStore(path)
	_, err := store.load()
	require.Error(t, err)
}

// The discard tombstone persists, and a rehydrated recorder still refuses the
// cancelled id into rolled_back — the discard's verdict survives the restart
// the way the codebase's tombstone-replay contract says it must.
func TestAccountSwapRecorderPersistsAndRehydratesTheDiscardTombstone(t *testing.T) {
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	var r accountSwapRecorder
	r.store = store

	r.discardStaged("doomed")
	r.noteStagedDiscarded("doomed")
	d, err := store.load()
	require.NoError(t, err)
	assert.Equal(t, "doomed", d.Cancelled, "the tombstone persisted")
	assert.Equal(t, "doomed", d.ID, "the discard's rolled_back verdict persisted under its own id")
	assert.Equal(t, swapOutcomeRolledBack, d.Outcome)

	// A fresh recorder rehydrates the tombstone and still refuses the id.
	var fresh accountSwapRecorder
	fresh.store = store
	fresh.restore(d)
	_, err = fresh.run("doomed", func() (string, error) { return "claude", nil })
	require.Error(t, err, "the rehydrated tombstone refuses the cancelled id")
	o, _, id := fresh.status()
	assert.Equal(t, swapOutcomeRolledBack, o)
	assert.Equal(t, "doomed", id)
}

// A recorder with no store (the daemon's in-process local sessions) persists
// nothing and behaves exactly as before — the fix is opt-in per process.
func TestAccountSwapRecorderWithoutStorePersistsNothing(t *testing.T) {
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	var r accountSwapRecorder // no store
	_, err := r.run("swap-1", func() (string, error) { return "claude", nil })
	require.NoError(t, err)
	d, err := store.load()
	require.NoError(t, err)
	assert.Equal(t, accountSwapRecordData{}, d, "an unbacked recorder writes nothing")
}

// InstallDurableAccountSwapRecord rehydrates the localAgentServer's recorder
// from the prior process's terminal record when AgentServer() builds it, the
// way the agent-server process rehydrates at startup.
func TestInstallDurableAccountSwapRecordRehydratesTheRecorder(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	// A previous process left a committed verdict on disk.
	prev := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName(key)))
	require.NoError(t, prev.save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1", Cancelled: "doomed",
	}))

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))

	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok, "a local instance builds a localAgentServer")
	o, p, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o, "the recorder rehydrated the committed verdict")
	assert.Equal(t, "claude", p)
	assert.Equal(t, "swap-1", id)

	// The rehydrated recorder is backed by the store, so a new swap persists.
	_, err := las.acctSwap.run("swap-2", func() (string, error) { return "claude", nil })
	require.NoError(t, err)
	d, err := prev.load()
	require.NoError(t, err)
	assert.Equal(t, "swap-2", d.ID, "the new swap overwrote the rehydrated record durably")
	assert.Equal(t, swapOutcomeCommitted, d.Outcome)
	// The tombstone the prior process left survives the new swap (the recorder
	// keeps the last tombstone across swaps).
	assert.Equal(t, "doomed", d.Cancelled)
}

// A daemon's in-process local session (no InstallDurableAccountSwapRecord
// call) keeps the recorder process-memory: no store, no persistence, and
// AgentServer() builds a localAgentServer whose recorder answers none.
func TestLocalSessionKeepsTheRecorderProcessMemory(t *testing.T) {
	inst := &Instance{Title: "local"}
	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	assert.Nil(t, las.acctSwap.store, "a local session's recorder has no durable backing")
	o, _, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeNone, o)
	assert.Empty(t, id)
}

// --- end-to-end: the bug's two scenarios, across an agent-server restart ---

// restartAgentServerRecorder simulates an agent-server restart on the fixture's
// recorder: the in-memory recorder is replaced with a fresh one rehydrated from
// the durable store, the way a restarted agent-server rehydrates its recorder
// from the session dir before serving any request (#7793).
func (f *swapFixture) restartAgentServerRecorder(t *testing.T) {
	t.Helper()
	store := f.srv.rec.store
	require.NotNil(t, store, "the fixture's recorder must be backed by a store")
	data, err := store.load()
	require.NoError(t, err)
	f.srv.rec = accountSwapRecorder{}
	f.srv.rec.store = store
	f.srv.rec.restore(data)
}

// The discard's tombstone survives an agent-server restart: a daemon whose
// record-clear checkpoint was lost re-runs the reconcile on a host whose staged
// scope is gone, and the restarted server's recorder — rehydrated from the
// session dir — answers THIS swap's own rolled_back rather than the anonymous
// "none" a stage-less, prev-less host can never tell from "still queued"
// (#7793). Without the rehydrate the row would fence through every restart
// forever.
func TestDiscardVerdictReplaysAcrossAnAgentServerRestart(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.rec.store = newAccountSwapStore(filepath.Join(t.TempDir(), accountSwapRecordMarkerName("key")))
	f.srv.holdNext = 1 // the request crossed but is still queued server-side

	_, err := f.inst.SwapOffBoxAccount("alt", false, "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate,
		"the first reconcile settled: discard -> none -> rollback")
	require.Nil(t, f.inst.InFlightOffBoxSwap())

	// The clear's checkpoint never landed: a restart reloads a row that still
	// carries the record and re-runs the same reconcile cold.
	f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "test-swap-1", From: "work", To: "alt"}
	f.inst.startupStateUnknown = true

	// The agent-server process restarted: its in-memory recorder is gone, and
	// its rehydrated replacement loads the prior process's terminal record.
	f.restartAgentServerRecorder(t)

	settled, err := f.reconcileInFlight()
	require.NoError(t, err)
	assert.True(t, settled, "the rehydrated tombstone replays as this swap's own rolled_back")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
	assert.False(t, f.inst.StartupStateUnknown())

	// And the queued request still lands to the tombstone: rolled back only.
	_, err = f.srv.runHeldSwap(0)
	require.Error(t, err)
	assert.Equal(t, secretWork, f.hostCred(t, "account"))
}

// A committed verdict survives an agent-server restart: the host rotated while
// the response was lost, the daemon fenced the row indeterminate, and the
// agent-server then restarted. The restarted server's recorder — rehydrated
// from the session dir — answers THIS swap's own committed, so the re-reconcile
// records the account the host provably runs instead of fencing a healthy row
// forever (#7793). This is the commit-settled variant the bug report's direct
// reproduction isolates.
func TestCommittedVerdictReplaysAcrossAnAgentServerRestart(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.rec.store = newAccountSwapStore(filepath.Join(t.TempDir(), accountSwapRecordMarkerName("key")))
	f.srv.loseResponse = errors.New("remote agent-server: POST /v1/agent/swap-account: context deadline exceeded")
	f.srv.statusErr = errors.New("remote agent-server: connection reset") // the terminal record cannot be read either

	_, err := f.inst.SwapOffBoxAccount("alt", false, "")
	require.ErrorIs(t, err, ErrAccountSwapIndeterminate)
	assert.Equal(t, secretAlt, f.hostCred(t, "account"), "the host committed while the response was lost")
	require.NotNil(t, f.inst.InFlightOffBoxSwap())

	// The agent-server process restarted: its fresh recorder rehydrates the
	// committed verdict the prior process reached, and its transport answers.
	f.restartAgentServerRecorder(t)
	f.srv.statusErr = nil

	settled, err := f.reconcileInFlight()
	require.NoError(t, err)
	assert.True(t, settled)
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "alt", acct, "the rehydrated commit records the account the host provably runs")
	assert.Nil(t, f.inst.InFlightOffBoxSwap())
	assert.False(t, f.inst.StartupStateUnknown())
}

// The fail-closed pin: a restarted agent-server whose recorder is NOT
// rehydrated answers anonymous "none", a settled host probes scopeUnknown, and
// the re-reconcile fences the row forever — the exact bug the durable recorder
// exists to close (#7793). This is the fail-closed contract a forgetting
// recorder must keep, not an accepted gap: if the recorder ever stops
// persisting or rehydrating, the replay-across-a-restart tests above fail and
// this test still documents the failure mode it would regress to.
func TestAForgottenSwapRecorderFailsClosedFencingTheRowForever(t *testing.T) {
	f := newSwapFixture(t)
	f.srv.rec.store = newAccountSwapStore(filepath.Join(t.TempDir(), accountSwapRecordMarkerName("key")))
	f.srv.holdNext = 1

	_, err := f.inst.SwapOffBoxAccount("alt", false, "")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAccountSwapIndeterminate)
	require.Nil(t, f.inst.InFlightOffBoxSwap())

	f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "test-swap-1", From: "work", To: "alt"}
	f.inst.startupStateUnknown = true

	// The agent-server restarted and its recorder forgot every verdict — the
	// shape the bug takes when the recorder is not rehydrated.
	f.srv.rec = accountSwapRecorder{}

	settled, err := f.reconcileInFlight()
	require.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate)
	assert.False(t, settled, "an empty recorder + a settled host fences forever")
	assert.NotNil(t, f.inst.InFlightOffBoxSwap())
	assert.True(t, f.inst.StartupStateUnknown())
}

// clear removes a previous process's durable record so a restart rehydrates
// nothing — the way a fresh pre-fix recorder answered swapOutcomeNone. A
// missing file is not an error: there is nothing to clear (#7793 review R1).
func TestAccountSwapStoreClearRemovesTheRecordAndToleratesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	require.NoError(t, store.save(accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))
	require.NoError(t, store.clear())
	d, err := store.load()
	require.NoError(t, err)
	assert.Equal(t, accountSwapRecordData{}, d, "the record was removed")

	// A second clear on the now-missing file is not an error.
	require.NoError(t, store.clear())
}

// R1's crash window: swap N-1 settled leaving {committed, N-1} on disk, swap N
// pre-wrote its own id as unknown, rotated, and the agent-server died before
// the terminal save. A restarted agent-server rehydrates THIS swap's own
// unknown from the pre-write, so the reconcile fences safely — it never
// rehydrates N-1's stale terminal record and mistakes it for a queued request
// that rolls back against a host already running N's account (#7793 review R1).
// On a head with no pre-write (the prior commit) the disk still holds
// {committed, N-1}, the reconcile discards into a definite rollback, and the
// row unfences on the wrong account — this test fails there.
func TestCrashAfterThePreWriteBeforeTheTerminalSaveStaysFenced(t *testing.T) {
	f := newSwapFixture(t)
	store := newAccountSwapStore(filepath.Join(t.TempDir(), accountSwapRecordMarkerName("key")))
	f.srv.rec.store = store

	// Swap N-1 settled: its terminal record is on the durable store and in
	// the running recorder's memory.
	require.NoError(t, store.save(accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "test-swap-1"}))
	f.srv.rec.restore(accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "test-swap-1"})

	// The daemon's durable in-flight record names swap N (test-swap-2), and
	// the loader fenced the row on startup.
	f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "test-swap-2", From: "work", To: "alt"}
	f.inst.startupStateUnknown = true

	// Swap N crosses: run() persists this swap's id as unknown (the pre-write
	// the fix adds), then swap() runs — the rotation — and the agent-server
	// dies before the terminal save. The closure simulates the crash by
	// detaching the store, so the deferred terminal save run() would do is a
	// no-op and the disk holds exactly what the pre-write left.
	_, err := f.srv.rec.run("test-swap-2", func() (string, error) {
		f.srv.rec.store = nil // the process died mid-rotation; the terminal save never lands
		return "claude", nil
	})
	require.NoError(t, err)

	// The agent-server restarted: its fresh recorder rehydrates from the disk
	// the crash left — {test-swap-2, unknown} on this head (the pre-write
	// landed), not the prior {committed, test-swap-1} a head with no pre-write
	// would leave.
	f.srv.rec.store = store // reattach: the crash nil'd the in-memory store; the file persists
	f.restartAgentServerRecorder(t)

	settled, err := f.reconcileInFlight()
	require.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate, "a crash mid-rotation rehydrates this swap's own unknown and fences")
	assert.False(t, settled, "the row is not settled on the wrong account")
	assert.NotNil(t, f.inst.InFlightOffBoxSwap(), "the fence stays up")
	assert.True(t, f.inst.StartupStateUnknown())
}

// R1's pre-write is required, not best-effort: when the checkpoint cannot land
// and the stale prior record cannot be removed either, the swap is refused
// before anything rotates — the host never moves while the durable record
// could name another swap (#7793 review R1).
func TestPreRotationCheckpointFailureRefusesTheSwapWhenTheRecordCannotBeCleared(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the save/clear failure this test forces needs a non-root uid")
	}
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	// A prior swap's terminal record is on disk and in the running recorder.
	require.NoError(t, store.save(accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))
	// The directory becomes read-only-as-writes: CreateTemp (save) and Remove
	// (clear) both fail with EACCES — the shape of a disk that can neither
	// create nor unlink, so neither the pre-write nor the fallback clear can
	// land.
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var r accountSwapRecorder
	r.store = store
	r.restore(accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"})

	ran := false
	_, err := r.run("swap-2", func() (string, error) { ran = true; return "claude", nil })
	require.Error(t, err, "the swap is refused when the pre-write cannot land and the stale record cannot be cleared")
	assert.False(t, ran, "swap() never ran — the rotation did not move the host")

	// The in-memory slot is restored to the prior swap's record.
	o, p, id := r.status()
	assert.Equal(t, swapOutcomeCommitted, o)
	assert.Equal(t, "claude", p)
	assert.Equal(t, "swap-1", id)

	// The stale record is still on disk, unchanged — the refused swap did not
	// touch it.
	d, lerr := store.load()
	require.NoError(t, lerr)
	assert.Equal(t, "swap-1", d.ID)
	assert.Equal(t, swapOutcomeCommitted, d.Outcome)
}

// R2's tombstone window: discardStaged tombstones the id AND claims its
// rolled_back verdict in the SAME write, so a crash between the tombstone save
// and noteStagedDiscarded's later save rehydrates as this swap's OWN
// rolled_back — never the anonymous none a bare tombstone would leave reading
// forever (#7793 review R2).
func TestCrashBetweenTheTombstoneAndNoteStagedDiscardedRehydratesAsRolledBack(t *testing.T) {
	dir := t.TempDir()
	store := newAccountSwapStore(filepath.Join(dir, accountSwapRecordMarkerName("key")))
	var r accountSwapRecorder
	r.store = store

	// discardStaged tombstones the id and claims rolled_back under it in one
	// write. The agent-server then crashed — noteStagedDiscarded never ran.
	r.discardStaged("doomed")

	var fresh accountSwapRecorder
	fresh.store = store
	d, err := store.load()
	require.NoError(t, err)
	fresh.restore(d)

	o, p, id := fresh.status()
	assert.Equal(t, swapOutcomeRolledBack, o, "the tombstone's write already carried the rolled_back verdict")
	assert.Equal(t, "doomed", id, "the verdict is this swap's own")
	assert.Empty(t, p)
}
