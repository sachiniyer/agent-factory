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

// The regression guard: a restarted agent-server whose recorder is NOT
// rehydrated answers anonymous "none", a settled host probes scopeUnknown, and
// the re-reconcile fences the row forever — the exact bug the durable recorder
// exists to close (#7793). If the recorder ever stops persisting or
// rehydrating, the replay-across-a-restart tests above fail and this test still
// documents the failure mode it would regress to.
func TestSettledSwapFencesForeverWhenTheRecorderForgetsOnRestart(t *testing.T) {
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
