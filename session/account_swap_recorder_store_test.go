package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
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
// pre-wrote its own id as a pending checkpoint, rotated, and the agent-server
// died before the terminal save. A restarted agent-server rehydrates THIS
// swap's own pending from the pre-write (reported as anonymous "none" — see
// accountSwapRecorder.status), so the reconcile probes the host the way a
// fresh pre-fix recorder did and fences safely — it never rehydrates N-1's
// stale terminal record and mistakes it for a queued request that rolls back
// against a host already running N's account (#7793 review R1). On a head
// with no pre-write (the prior commit) the disk still holds
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

	// Swap N crosses: run() persists this swap's id as a pending checkpoint
	// (the pre-write the fix adds), then swap() runs — the rotation — and the
	// agent-server dies before the terminal save. The closure simulates the
	// crash by detaching the store, so the deferred terminal save run() would
	// do is a no-op and the disk holds exactly what the pre-write left.
	_, err := f.srv.rec.run("test-swap-2", func() (string, error) {
		f.srv.rec.store = nil // the process died mid-rotation; the terminal save never lands
		return "claude", nil
	})
	require.NoError(t, err)

	// The agent-server restarted: its fresh recorder rehydrates from the disk
	// the crash left — {test-swap-2, pending} on this head (the pre-write
	// landed, reported as anonymous "none" — see accountSwapRecorder.status),
	// not the prior {committed, test-swap-1} a head with no pre-write would
	// leave.
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

// The other arm of run()'s failed-pre-write handling, driven end-to-end: the
// pre-rotation checkpoint cannot land, but the prior record CAN be unlinked,
// so the swap proceeds (the rotation runs and commits) and the agent-server
// crashes before the terminal save. The disk is empty, so the restart
// reconcile re-asks the host exactly like master's empty recorder and fences
// where master fences (#7862 review F3 — the crash table's empty-disk rows
// state this disk directly; this test drives run()'s branch that produces it).
func TestPreRotationCheckpointFailureThatClearsThePriorRecordLetsTheRotationRunAndFencesLikeMaster(t *testing.T) {
	f := newSwapFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, accountSwapRecordMarkerName("key"))
	// Make the record path an empty directory: save (CreateTemp + Rename)
	// fails at the Rename ("file exists"), while clear (os.Remove) succeeds —
	// the shape of a disk that can unlink a stale record but cannot commit a
	// new one. clear() removes the directory, leaving the disk empty so the
	// swap proceeds.
	require.NoError(t, os.Mkdir(path, 0o700))
	store := newAccountSwapStore(path)
	f.srv.rec.store = store

	// Stage this swap's incoming scope the way the daemon does, so the
	// rotation actually runs and leaves account.prev behind.
	f.srv.lastScoped = true
	stage := f.stagedDirFor("test-swap-1")
	require.NoError(t, os.MkdirAll(stage, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stage, accountSwapMarkerRel), []byte("test-swap-1"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(stage, credFile), []byte(secretAlt), 0o600))

	f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: "test-swap-1", From: "work", To: "alt"}
	f.inst.startupStateUnknown = true

	var logs bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &logs)

	ran := false
	_, err := f.srv.rec.run("test-swap-1", func() (string, error) {
		prog, serr := f.srv.swap("test-swap-1") // the rotation runs and commits
		ran = serr == nil
		f.srv.rec.store = nil // the process died before the terminal save
		return prog, serr
	})
	require.NoError(t, err, "the swap proceeds: the pre-write failed but the prior record cleared")
	assert.True(t, ran, "swap() ran — the rotation moved the host")
	assert.Contains(t, logs.String(), "degraded",
		"the checkpoint's clear-and-proceed degrade is logged at the moment it happens (#7907 R3)")

	// The terminal save is best-effort and also cannot land (the store was
	// detached mid-rotation), so the disk is the empty state clear left —
	// the crash state the restarted recorder rehydrates.
	f.srv.rec.store = store
	f.restartAgentServerRecorder(t)

	settled, err := f.reconcileInFlight()
	require.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate, "an empty disk over the rotation's scopeIncoming fences, exactly like master's empty recorder")
	assert.False(t, settled, "the row is not settled on a guess")
	assert.NotNil(t, f.inst.InFlightOffBoxSwap(), "the fence stays up")
	assert.True(t, f.inst.StartupStateUnknown())
	acct, _ := f.inst.AccountSelection()
	assert.Equal(t, "work", acct, "the row keeps its recorded account while it fences")
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

// TestSwapRestartReconcileSettlesEveryCrashPointToTheHostsProvenVerdictOrFencesNoWorseThanMaster
// is the crash-point matrix the #7858 review's stop rule asks for: one table over
// every point an agent-server can die mid-swap, each row asserting the restart
// reconcile's verdict and whether the row stays fenced, started both with a prior
// swap's terminal record on disk and with none.
//
// Property under test: at every crash point the reconcile reaches EITHER the
// verdict the host's layout proves, OR stays fenced — but only where master (a
// recorder that forgets every verdict on restart) would also stay fenced. It
// never reaches a verdict the host contradicts, and never fences a layout
// master settles.
//
// The crash points (the matrix's rows):
//  1. before the pre-write;
//  2. after the pre-write, before the first rename (the P1 this head fixes:
//     the disk holds this swap's `pending` checkpoint, reported as anonymous
//     "none", so the reconcile takes master's `none` arm and settles as
//     rolled_back instead of fencing forever — 53e3e788a fences here);
//  3. between the two renames (next + prev both present: the host cannot prove
//     the swap never rotated, so the row fences, as master does);
//  4. after the rotation, before the terminal save (a prev present: the
//     rotation may have reached the host, so the row fences, as master does);
//  5. after the terminal save (a matching committed over a prev: settles as
//     committed — master fences here, the #7858 fix);
//  6. an empty disk over a proven scopeOutgoing (a cleared prior record, or no
//     prior): settles as rolled_back exactly like master's empty recorder;
//  7. a failed pre-write that couldn't clear it (refused: the prior record is
//     unchanged and the host never moved);
//  8. the tombstone window in discardStaged (a matching rolled_back over a
//     cleared stage: settles as rolled_back — master fences here);
//  9. a terminal `unknown` over a proven scopeOutgoing (the rotation ran and
//     its scope could not be put back): this head fences and leaves the stage
//     in place to keep the P2 straggler protection master's empty recorder
//     lacked — master settles this row, this head is the deliberate exception
//     to "no row is worse than master";
//  10. a non-own `pending` (a prior swap's failed terminal write left its
//     checkpoint on disk) over the next swap's proven scopeOutgoing: the
//     reconcile reports the stale `pending` as anonymous "none" and settles
//     as rolled_back exactly like master, instead of fencing the next swap
//     forever (#7862 review F1) — 2d938df71/53e3e788a fence here.
//
// Each newly-correct row (2, 5, 8, 10) fails on 53e3e788a/2d938df71 or master,
// which fenced where this head settles. No row is worse than master except
// row 9, whose fence is the deliberate P2 straggler protection above.
func TestSwapRestartReconcileSettlesEveryCrashPointToTheHostsProvenVerdictOrFencesNoWorseThanMaster(t *testing.T) {
	// disk is the recorder's durable record the crash leaves — what the
	// restarted agent-server rehydrates from. zero + clearDisk means the
	// crash left no record (a cleared store, or no prior swap).
	type crashPoint struct {
		name      string
		hasPrior  bool                  // a prior swap's {committed, N-1} is on disk before the crash
		disk      accountSwapRecordData // the recorder's durable record after the crash
		clearDisk bool                  // the crash left no record on disk
		stage     bool                  // account.next.<N> present — the scopeOutgoing component
		prev      bool                  // account.prev present — the scopeIncoming component
		settled   bool                  // the reconcile settles the row
		fenced    bool                  // the in-flight record stays up (the fence holds)
		account   string                // the account the row settles on
		// masterVerdict is what master (a forgetting recorder) reaches for
		// the same crash point, stated for each row so the table shows no row
		// is worse than master.
		masterVerdict string
		// newlyCorrect marks rows this head settles where master/53e3e788a
		// fences — the rows that must fail on an earlier head.
		newlyCorrect bool
		// stragglerRefused runs a late run(swapN) after the reconcile settles
		// and asserts it is refused and the host never rotates — the
		// property that a matching `unknown` over a proven scopeOutgoing
		// settles only after the discard tombstones swapN (#7862 review).
		// Such a row must fail on 8a68aee82, which settled before the
		// discard and left the still-staged scope open to a late commit.
		stragglerRefused bool
		// stageUntouched asserts the staged scope (account.next.<N>) is left
		// in place after the reconcile — the property that a terminal
		// `unknown` is fenced, not discarded, so a host the rotation may have
		// reached keeps its staged scope for recovery (#7862 review P2).
		stageUntouched bool
	}

	const (
		swapN = "test-swap-2" // the daemon's in-flight record names swap N
		swap1 = "test-swap-1" // a prior swap whose terminal record may be on disk
	)
	priorRecord := accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: swap1}
	rows := []crashPoint{
		// 1. before the pre-write: the recorder has not yet checkpointed swap
		//    N, so the disk holds the prior swap's record (or nothing). The
		//    host still has the staged scope and nothing rotated — scopeOutgoing.
		{name: "1-before-prewrite/prior", hasPrior: true, disk: priorRecord, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (non-matching committed -> discard -> rolled_back)"},
		{name: "1-before-prewrite/none", hasPrior: false, clearDisk: true, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)"},

		// 2. after the pre-write, before the first rename: the disk holds
		//    {pending, N} and the host proves scopeOutgoing. This head
		//    settles as rolled_back; 53e3e788a fences (the P1).
		{name: "2-after-prewrite-before-rename/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard)", newlyCorrect: true},
		{name: "2-after-prewrite-before-rename/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard)", newlyCorrect: true},

		// 2b. the straggler property on the same crash point: a matching
		//     `pending` over a proven scopeOutgoing settles only AFTER the
		//     serialized discard tombstones swapN, so a late run(swapN) that
		//     names the still-staged scope is refused instead of committing
		//     against a daemon that already recorded a rollback (#7862
		//     review). On 8a68aee82 the reconcile settled on the first read
		//     without discarding, so the tombstone was never set and the
		//     still-staged scope was still present — the straggler rotates
		//     and commits, and these rows fail there.
		{name: "2b-after-prewrite-before-rename-straggler/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, settled: true, account: "work", stragglerRefused: true,
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> tombstone)"},
		{name: "2b-after-prewrite-before-rename-straggler/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, settled: true, account: "work", stragglerRefused: true,
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> tombstone)"},

		// 3. between the two renames: next + prev both present, so the host
		//    cannot prove the swap never rotated. The row fences, as master
		//    does (none -> probe scopeUnknown -> fence).
		{name: "3-between-renames/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, prev: true, fenced: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeUnknown -> fence)"},
		{name: "3-between-renames/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, stage: true, prev: true, fenced: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeUnknown -> fence)"},

		// 4. after the rotation, before the terminal save: a prev is present
		//    (scopeIncoming), so the rotation may have reached the host. The
		//    row fences, as master does (none -> probe scopeIncoming -> fence).
		{name: "4-after-rotation-before-terminal-save/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, prev: true, fenced: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeIncoming -> fence)"},
		{name: "4-after-rotation-before-terminal-save/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swapN}, prev: true, fenced: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeIncoming -> fence)"},

		// 5. after the terminal save: a matching committed over a prev —
		//    settles as committed. Master fences (the #7858 bug).
		{name: "5-after-terminal-save/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: swapN}, prev: true, settled: true, account: "alt",
			masterVerdict: "fenced (none -> probe scopeIncoming -> fence)", newlyCorrect: true},
		{name: "5-after-terminal-save/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: swapN}, prev: true, settled: true, account: "alt",
			masterVerdict: "fenced (none -> probe scopeIncoming -> fence)", newlyCorrect: true},

		// 6. an empty disk over a proven scopeOutgoing (a cleared prior
		//    record, or no prior at all): the reconcile behaves exactly like
		//    master's empty recorder. The end-to-end failed-pre-write path
		//    that produces this empty disk — save fails, clear succeeds, the
		//    rotation runs, the agent-server crashes before the terminal
		//    save — is driven in
		//    TestPreRotationCheckpointFailureThatClearsThePriorRecordLetsTheRotationRunAndFencesLikeMaster.
		{name: "6-empty-disk-over-scopeoutgoing/prior", hasPrior: true, clearDisk: true, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)"},
		{name: "6-empty-disk-over-scopeoutgoing/none", hasPrior: false, clearDisk: true, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)"},

		// 7. a failed pre-write that couldn't clear it: the swap is refused,
		//    the host never moved, and the prior record is unchanged. The
		//    end-to-end refused path is driven in
		//    TestPreRotationCheckpointFailureRefusesTheSwapWhenTheRecordCannotBeCleared.
		{name: "7-failed-prewrite-refused/prior", hasPrior: true, disk: priorRecord, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)"},
		{name: "7-failed-prewrite-refused/none", hasPrior: false, clearDisk: true, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)"},

		// 8. the tombstone window in discardStaged: the discard already
		//    claimed rolled_back under N and removed the stage, so a matching
		//    rolled_back over a cleared stage settles. Master fences (none ->
		//    probe scopeNone -> fence).
		{name: "8-tombstone-window/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: swapN, Cancelled: swapN}, settled: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeNone -> fence)", newlyCorrect: true},
		{name: "8-tombstone-window/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: swapN, Cancelled: swapN}, settled: true, account: "work",
			masterVerdict: "fenced (none -> probe scopeNone -> fence)", newlyCorrect: true},

		// 9. a terminal unknown over a proven scopeOutgoing: the rotation
		//    ran and its scope could not be put back, so the record alone
		//    cannot settle it and the discard must NOT upgrade it to
		//    rolled_back — a straggler that took the lock between the read
		//    and the discard can leave the host with neither account in
		//    place (#7862 review P2). This head fences and leaves the staged
		//    scope untouched. Master's empty recorder has no straggler to
		//    protect against, so master settles this row (none -> probe
		//    scopeOutgoing -> discard -> noteStagedDiscarded claims rolled_back
		//    -> read 2); this head is the deliberate exception to "no row is
		//    worse than master". On ce99d8d45 a matching `unknown` over
		//    scopeOutgoing fell through to the discard, the discard's
		//    refuseLocked upgraded it to rolled_back and removed the stage,
		//    and the row settled — so this row fails there.
		{name: "9-terminal-unknown-over-scopeoutgoing/prior", hasPrior: true, disk: accountSwapRecordData{Outcome: swapOutcomeUnknown, ID: swapN}, stage: true, fenced: true, account: "work", stageUntouched: true,
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> noteStagedDiscarded claims rolled_back)"},
		{name: "9-terminal-unknown-over-scopeoutgoing/none", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomeUnknown, ID: swapN}, stage: true, fenced: true, account: "work", stageUntouched: true,
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> noteStagedDiscarded claims rolled_back)"},

		// 10. a non-own `pending` over the next swap's proven scopeOutgoing
		//     (#7862 review F1): a prior swap's terminal save failed after
		//     its pre-write landed and its rotation committed, leaving its
		//     `pending` checkpoint on disk. The next swap (the one this
		//     row reconciles) has its own staged scope (scopeOutgoing) and
		//     its request was never delivered. The reconcile reports the
		//     stale `pending` as anonymous "none" (see
		//     accountSwapRecorder.status) and settles as rolled_back exactly
		//     like master, instead of fencing the next swap forever. On
		//     2d938df71/53e3e788a the non-own `pending` hit the
		//     `swapOutcomePending` arm and fenced with no discard, so this
		//     row fails there.
		{name: "10-non-own-pending-failed-terminal-save", hasPrior: false, disk: accountSwapRecordData{Outcome: swapOutcomePending, ID: swap1}, stage: true, settled: true, account: "work",
			masterVerdict: "settled rolled_back (none -> probe scopeOutgoing -> discard -> rolled_back)", newlyCorrect: true},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newSwapFixture(t)
			store := newAccountSwapStore(filepath.Join(t.TempDir(), accountSwapRecordMarkerName("key")))
			f.srv.rec.store = store

			// A prior swap's terminal record may be on disk before the crash.
			if row.hasPrior {
				require.NoError(t, store.save(priorRecord))
			}
			// The crash leaves the recorder's durable record in the row's
			// state (or clears it).
			if row.clearDisk {
				require.NoError(t, store.clear())
			} else {
				require.NoError(t, store.save(row.disk))
			}

			// The host's account scope as the crash left it: the staged
			// scope (account.next.<N>) and the outgoing prev (account.prev).
			stage := f.stagedDirFor(swapN)
			prev := f.w.PrevAccountDir()
			require.NoError(t, os.RemoveAll(stage))
			require.NoError(t, os.RemoveAll(prev))
			if row.stage {
				require.NoError(t, os.MkdirAll(stage, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(stage, accountSwapMarkerRel), []byte(swapN), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(stage, credFile), []byte(secretAlt), 0o600))
			}
			if row.prev {
				require.NoError(t, os.MkdirAll(prev, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(prev, credFile), []byte(secretWork), 0o600))
			}

			// The daemon's durable in-flight record names swap N and fences
			// the row on load; the agent-server restarted and rehydrated its
			// recorder from the disk the crash left.
			f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: swapN, From: "work", To: "alt", ScopedStage: true}
			f.inst.startupStateUnknown = true
			f.restartAgentServerRecorder(t)

			if row.stragglerRefused {
				// The straggler property: a matching `unknown` over a proven
				// scopeOutgoing must settle only AFTER the serialized discard
				// tombstones swapN, so a late run(swapN) that names the
				// still-staged scope is refused instead of committing
				// against a daemon that already recorded a rollback (#7862
				// review). Call reconcileSwap DIRECTLY so the straggler lands
				// in the window between its verdict and the caller's own
				// staged-scope discard — the window a late request reaches
				// the restarted server. On 8a68aee82 reconcileSwap returned
				// rolled_back on the first read WITHOUT discarding, so the
				// tombstone was never set inside reconcileSwap and the
				// still-staged scope was still present; the straggler
				// rotates and commits, and these assertions fail there.
				committed, _, known, _ := f.w.reconcileSwap(f.srv, swapN, true)
				assert.False(t, committed, "the reconcile settles as rolled_back")
				assert.True(t, known, "the reconcile reaches a definite verdict (not fenced)")

				f.srv.lastScoped = true // the scoped staging path the stage was set at
				ran := false
				_, serr := f.srv.rec.run(swapN, func() (string, error) {
					ran = true
					return f.srv.swap(swapN)
				})
				require.Error(t, serr, "the straggler run(swapN) is refused — reconcileSwap tombstoned it before returning")
				assert.False(t, ran, "swap() never ran — the host never rotated")
				assert.Equal(t, secretWork, f.hostCred(t, "account"), "the host account is unchanged")
				return
			}

			settled, err := f.reconcileInFlight()

			if row.fenced {
				require.ErrorIs(t, err, ErrOffBoxSwapVerdictIndeterminate, "the row stays fenced where the host cannot prove the verdict")
				assert.False(t, settled, "the reconcile does not settle a fenced row")
				assert.NotNil(t, f.inst.InFlightOffBoxSwap(), "the fence stays up")
				assert.True(t, f.inst.StartupStateUnknown())
			} else {
				require.NoError(t, err, "the row settles cleanly")
				assert.True(t, settled, "the reconcile settles the row")
				assert.Nil(t, f.inst.InFlightOffBoxSwap(), "the fence lifts")
				assert.False(t, f.inst.StartupStateUnknown())
			}
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, row.account, acct, "the row settles on the account the verdict proves (master: %s)", row.masterVerdict)
			if row.stageUntouched {
				assert.DirExists(t, stage, "the staged scope is left in place while the row fences (master: %s)", row.masterVerdict)
			}
		})
	}
}

// A record that cannot be read, decoded, or interpreted — including one that
// decodes into a state the recorder never persists (#7897) — can vouch for
// nothing, so it must never stop the agent-server from starting (#7883): the
// install moves the file aside under a uniquely-named sibling and the recorder
// starts empty — anonymous "none", the same answer a missing record gives —
// which sends every later reconcile through the host's own markers instead of
// vouching for a swap it cannot prove. On master the undecodable cases make
// InstallDurableAccountSwapRecord return the load error and `af agent-server`
// refuses to start, wedging the sandbox session until the file is deleted by
// hand; the semantic cases fail there differently — master's load accepts
// them, and the rehydrated state has no settle arm, so the row fences forever
// (each row's masterFail names its stated reason).
func TestACorruptSwapRecordStartsTheRecorderEmptyAndReconcilesAsMasterWould(t *testing.T) {
	rows := []struct {
		name     string
		contents string
		// why the row fails on master — each names the stated reason.
		masterFail string
	}{
		{name: "truncated JSON", contents: `{"outcome":"committed","id":"test-swap-1"`,
			masterFail: "the load's parse error fails the install"},
		{name: "wrong top-level type", contents: `[1,2,3]`,
			masterFail: "the load's parse error fails the install"},
		{name: "wrong field type", contents: `{"outcome":42,"id":"test-swap-1"}`,
			masterFail: "the load's parse error fails the install"},
		{name: "zero bytes", contents: ``,
			masterFail: "the load's parse error fails the install"},
		{name: "uninterpretable verdict", contents: `{"outcome":"bogus","id":"test-swap-1"}`,
			masterFail: "the load accepts the file, the rehydrated verdict has no reconcile arm, and the row fences forever"},
		{name: "none carrying an id", contents: `{"outcome":"none","id":"test-swap-1"}`,
			masterFail: "the load accepts the file, the rehydrated none-with-id matches the swap's id, and the outcome has no settle arm — the row fences forever"},
		{name: "bare none", contents: `{"outcome":"none"}`,
			masterFail: "the load accepts the file — none is a status answer, never a persisted state, so the file stays in place forever re-readable"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newSwapFixture(t)
			dir := t.TempDir()
			key := RebuildTeardownKey("/repo", "title")
			path := filepath.Join(dir, accountSwapRecordMarkerName(key))
			require.NoError(t, os.WriteFile(path, []byte(row.contents), 0o600))

			// The agent-server installs its durable record the way
			// RunAgentServer does at startup (#7862).
			agentInst := &Instance{Title: "title"}
			require.NoError(t, agentInst.InstallDurableAccountSwapRecord(dir, key),
				"a corrupt record must not stop the agent-server from starting (master: %s)", row.masterFail)

			// The recorder starts with nothing it can vouch for: anonymous
			// "none", exactly what a missing record answers. On master the
			// semantic rows fail HERE — the load accepted the file and the
			// recorder vouches for a verdict or an id no write ever produced —
			// before the quarantine block below shows the file was never moved
			// aside.
			las, ok := agentInst.AgentServer().(*localAgentServer)
			require.True(t, ok)
			o, p, id := las.acctSwap.status()
			assert.Equal(t, swapOutcomeNone, o)
			assert.Empty(t, p)
			assert.Empty(t, id)

			// The evidence is moved aside under a uniquely-named sibling that
			// says so, never deleted — and under its new name no load reads it
			// again.
			assert.NoFileExists(t, path)
			asides, err := filepath.Glob(path + ".corrupt-*")
			require.NoError(t, err)
			require.Len(t, asides, 1, "one uniquely-named quarantine sibling")
			aside, err := os.ReadFile(asides[0])
			require.NoError(t, err)
			assert.Equal(t, row.contents, string(aside), "the quarantined file keeps the corrupt bytes")

			// And it reconciles as master would with no record: the host's own
			// markers decide. A proven scopeOutgoing (the staged scope present,
			// no account.prev) takes the serialized discard and settles
			// rolled_back — master's `none` arm verbatim. The reconcile runs
			// through a recorder restored from whatever the install loaded —
			// which is what the semantic rows' masterFail describes fencing.
			const swapN = "test-swap-1"
			f.srv.rec = accountSwapRecorder{store: las.acctSwap.store}
			f.srv.rec.restore(agentInst.durableAccountSwapRecord)
			stage := f.stagedDirFor(swapN)
			require.NoError(t, os.RemoveAll(f.w.PrevAccountDir()))
			require.NoError(t, os.MkdirAll(stage, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(stage, accountSwapMarkerRel), []byte(swapN), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(stage, credFile), []byte(secretAlt), 0o600))
			f.inst.offBoxSwapInFlight = &InFlightOffBoxSwapData{SwapID: swapN, From: "work", To: "alt", ScopedStage: true}
			f.inst.startupStateUnknown = true

			settled, err := f.reconcileInFlight()
			require.NoError(t, err, "the row settles — master's empty recorder reaches rolled_back over scopeOutgoing")
			assert.True(t, settled)
			assert.Nil(t, f.inst.InFlightOffBoxSwap())
			assert.False(t, f.inst.StartupStateUnknown())
			acct, _ := f.inst.AccountSelection()
			assert.Equal(t, "work", acct, "the verdict the host's layout proves, never one the corrupt record could not vouch for")

			// The recorder is durably backed again already: the discard's
			// tombstone write recreated the record file — the corrupt bytes are
			// gone for good under .corrupt, and this swap's own verdict is on
			// disk where the next restart will find it.
			d, err := las.acctSwap.store.load()
			require.NoError(t, err)
			assert.Equal(t, accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: swapN, Cancelled: swapN}, d)
		})
	}
}

// The record is retired at the explicit Kill RPC and nowhere else (#7897): a
// restart within the generation rehydrates it, and a signal-driven Kill — the
// path a host reboot's SIGTERM and a restart's pre-launch
// stopUnadoptedServer take through shutdownWorkspace — keeps it, because the
// reattach restarts the server in this same session dir and must find it.
// A record that still outlives its row sits in a per-generation mktemp dir no
// later generation reuses. On master the signal-driven Kill retires the
// record — the host-reboot case the issue names.
func TestSwapRecordRetiresWithTheSessionRowNotTheServerProcess(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))

	// Generation 1: the agent-server installs the store and a swap commits.
	first := &Instance{Title: "title", backend: &FakeBackend{}}
	require.NoError(t, first.InstallDurableAccountSwapRecord(dir, key))
	las1, ok := first.AgentServer().(*localAgentServer)
	require.True(t, ok)
	_, err := las1.acctSwap.run("swap-1", func() (string, error) { return "claude", nil })
	require.NoError(t, err)
	assert.FileExists(t, path)

	// A restart WITHIN the generation — the crashed server's replacement,
	// which for a normally provisioned sandbox is a fresh launch, not an
	// adoption — installs the same store, reaches no Kill, and rehydrates the
	// verdict the prior process reached (#7862).
	second := &Instance{Title: "title", backend: &FakeBackend{}, gitWorktree: &git.GitWorktree{}}
	require.NoError(t, second.InstallDurableAccountSwapRecord(dir, key))
	las2, ok := second.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, p, id := las2.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o)
	assert.Equal(t, "claude", p)
	assert.Equal(t, "swap-1", id)
	assert.FileExists(t, path, "a same-generation restart keeps the record")

	// A signal-driven Kill — the graceful host reboot's SIGTERM reaching
	// shutdownWorkspace — destroys the workspace but leaves the daemon's row
	// alive, so the record survives with it: the reattach restarts the server
	// in this same session dir and rehydrates.
	require.NoError(t, las2.Kill(false))
	assert.FileExists(t, path, "a signal-driven Kill keeps the record — the reattach rehydrates it (master: it retires here)")

	// The explicit Kill RPC's own retire ends the record's life — the af
	// sessions kill case, which removes the row (the daemon test pins the
	// handler's call; here the call itself).
	second.RetireDurableAccountSwapRecord()
	assert.NoFileExists(t, path, "the Kill RPC's retire removes the record")

	third := &Instance{Title: "title", backend: &FakeBackend{}}
	require.NoError(t, third.InstallDurableAccountSwapRecord(dir, key))
	las3, ok := third.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, _, id = las3.acctSwap.status()
	assert.Equal(t, swapOutcomeNone, o, "a new generation inherits nothing from the last")
	assert.Empty(t, id)
}

// A kill on a never-launched server — the restart's pre-launch
// stopUnadoptedServer SIGTERM case (#7897): the replacement agent-server is
// stopped before it was ever given the workspace, so its Instance holds no
// worktree and no tabs. The kill no-ops, and the record survives either way —
// the record's life is the row's, and a process-level kill is never the
// row's removal. Fails on bd0cce25a, whose Kill retired unconditionally.
func TestAKillOnANeverLaunchedServerKeepsTheRecord(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))

	inst := &Instance{Title: "title", backend: &LocalBackend{}}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	_, err := las.acctSwap.run("swap-1", func() (string, error) { return "claude", nil })
	require.NoError(t, err)

	require.NoError(t, las.Kill(false))
	assert.FileExists(t, path, "a never-launched server's Kill is no generation boundary — the record stays")
}

// The /v1/agent/stop path reaches localAgentServer.Kill too — StopPreserving
// runs StopPreservingWorkspace, then its SIGTERM drives shutdownWorkspace into
// Kill on a backend that no-ops — but the workspace PRESERVED, so the session
// generation survives for a later adopted restart and the record must survive
// with it: Kill never retires, so the whole preserve sequence leaves the file
// where the adopted restart's install finds it (Codex on #7894, #7897).
func TestSwapRecordSurvivesAWorkspacePreservingShutdown(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))

	inst := &Instance{Title: "title", backend: &FakeBackend{}, gitWorktree: &git.GitWorktree{}}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	_, err := las.acctSwap.run("swap-1", func() (string, error) { return "claude", nil })
	require.NoError(t, err)

	// The preserve stop releases the workspace: teardownTabs(keepWorkspace)
	// leaves the tree on disk and clears the pointer — exactly the sequence
	// the /v1/agent/stop handler runs before its SIGTERM.
	require.NoError(t, las.StopPreservingWorkspace())
	assert.Nil(t, inst.gitWorktree, "the preserve released the instance's workspace")

	// The SIGTERM's shutdownWorkspace then calls Kill on a backend that
	// no-ops — the workspace it would destroy was already released. The kill
	// succeeds, but the record is the PRESERVED generation's own: it must
	// survive for a later adopted restart to rehydrate.
	require.NoError(t, las.Kill(false))
	assert.FileExists(t, path, "the record outlives a workspace-preserving shutdown")

	reborn := &Instance{Title: "title", backend: &FakeBackend{}, gitWorktree: &git.GitWorktree{}}
	require.NoError(t, reborn.InstallDurableAccountSwapRecord(dir, key))
	las2, ok := reborn.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, _, id := las2.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o, "the adopted restart rehydrates the preserved generation's verdict")
	assert.Equal(t, "swap-1", id)
}

// Only a non-regular entry or bytes that were read and then failed to decode
// or validate are moved aside (#7897): a READ error (EIO, EACCES, EMFILE)
// never examined the file, so a transient fault must not permanently set
// aside a record that may be fine — the recorder starts empty and the file
// stays put (surviving only until the recorder's next write, whose atomic
// rename replaces it). On master the install moved the unexamined file aside:
// the FileExists below fails there. Root reads through a chmod-000, so the
// test skips there (the repo convention).
func TestAnUnreadableSwapRecordStartsEmptyButIsLeftInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through a chmod-000 file")
	}
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))
	require.NoError(t, os.WriteFile(path, []byte(`{"outcome":"committed","program":"claude","id":"swap-1"}`), 0o600))
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key),
		"a record that cannot be READ still must not stop the agent-server from starting")
	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, _, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeNone, o)
	assert.Empty(t, id)

	assert.FileExists(t, path, "an unread record is left in place — its bytes were never examined")
	asides, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	assert.Empty(t, asides, "no quarantine ran on a read error")
}

// Moving a record aside never overwrites earlier evidence (#7897): each
// corrupt file is claimed under a unique *.corrupt-* name before the rename,
// so a second corruption preserves the first one's bytes instead of renaming
// over them. On master the second corruption clobbered the first.
func TestACorruptSwapRecordQuarantineNeverClobbersEarlierEvidence(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))

	for i, contents := range []string{`{"outcome":"bogus-1"`, `{"outcome":"bogus-2"`} {
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		inst := &Instance{Title: "title"}
		require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
		asides, err := filepath.Glob(path + ".corrupt-*")
		require.NoError(t, err)
		require.Len(t, asides, i+1, "each corruption leaves its own evidence — never renamed over")
	}

	asides, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, aside := range asides {
		b, err := os.ReadFile(aside)
		require.NoError(t, err)
		seen[string(b)] = true
	}
	assert.True(t, seen[`{"outcome":"bogus-1"`], "the first corruption's bytes survive")
	assert.True(t, seen[`{"outcome":"bogus-2"`], "the second corruption's bytes survive")
}

// The validator and the writer agree (#7897 review): every state a real run,
// discardStaged or noteStagedDiscarded can persist — including the id-less
// states an unmarked shared-scope sweep (discardStaged with "") and a
// pre-identity daemon's swap (run with "") produce on routine paths — loads
// cleanly on the next install: no corrupt warning, nothing moved aside, and
// the rehydrated recorder holds exactly what the writer persisted. The
// id-less rows fail on efea00bc6, whose validator rejected every id-less
// state and quarantined this build's own writes — a regression from master.
func TestEveryStateTheRecorderPersistsLoadsCleanly(t *testing.T) {
	key := RebuildTeardownKey("/repo", "title")
	ok := func() (string, error) { return "claude", nil }
	boom := func() (string, error) { return "", errors.New("rotation failed") }
	indeterminate := func() (string, error) { return "", errors.New(accountScopeIndeterminate) }

	rows := []struct {
		name  string
		drive func(t *testing.T, r *accountSwapRecorder, s *accountSwapStore)
		want  accountSwapRecordData
	}{
		{"run commits under a named swap", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			_, err := r.run("swap-1", ok)
			require.NoError(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}},

		{"run's pending checkpoint, read mid-swap", func(t *testing.T, r *accountSwapRecorder, s *accountSwapStore) {
			_, err := r.run("swap-1", func() (string, error) {
				// The pre-rotation checkpoint is on disk right now — it must
				// load cleanly too, or a crash here leaves a "corrupt" record.
				d, lerr := s.load()
				require.NoError(t, lerr)
				assert.Equal(t, accountSwapRecordData{Outcome: swapOutcomePending, ID: "swap-1"}, d)
				return "claude", nil
			})
			require.NoError(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}},

		{"run commits for a pre-identity daemon", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			_, err := r.run("", ok)
			require.NoError(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeCommitted, Program: "claude"}},

		{"run rolls back under a named swap", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			_, err := r.run("swap-1", boom)
			require.Error(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: "swap-1"}},

		{"run rolls back for a pre-identity daemon", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			_, err := r.run("", boom)
			require.Error(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeRolledBack}},

		{"run turns indeterminate under a named swap", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			_, err := r.run("swap-1", indeterminate)
			require.Error(t, err)
		}, accountSwapRecordData{Outcome: swapOutcomeUnknown, ID: "swap-1"}},

		{"discardStaged claims a named slot", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			r.discardStaged("swap-1")
		}, accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: "swap-1", Cancelled: "swap-1"}},

		{"discardStaged anonymous on a fresh recorder", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			r.discardStaged("")
		}, accountSwapRecordData{}},

		{"noteStagedDiscarded claims a named slot", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			r.noteStagedDiscarded("swap-1")
		}, accountSwapRecordData{Outcome: swapOutcomeRolledBack, ID: "swap-1"}},

		{"noteStagedDiscarded anonymous", func(t *testing.T, r *accountSwapRecorder, _ *accountSwapStore) {
			r.noteStagedDiscarded("")
		}, accountSwapRecordData{Outcome: swapOutcomeRolledBack}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, accountSwapRecordMarkerName(key))
			store := newAccountSwapStore(path)
			rec := &accountSwapRecorder{store: store}
			row.drive(t, rec, store)

			d, err := store.load()
			require.NoError(t, err, "a state this build's writer persisted must load, not read corrupt")
			assert.Equal(t, row.want, d)

			// End-to-end through the install the way RunAgentServer loads it:
			// no corrupt warning, nothing quarantined, and the rehydrated
			// record is exactly what the writer persisted.
			inst := &Instance{Title: "title"}
			require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
			assert.Equal(t, row.want, inst.durableAccountSwapRecord)
			assert.FileExists(t, path, "a persisted state is never moved aside")
			asides, err := filepath.Glob(path + ".corrupt-*")
			require.NoError(t, err)
			assert.Empty(t, asides, "no quarantine ran on a state the writer persists")
		})
	}
}

// A non-regular entry at the record path is corrupt and moved aside, never
// opened (#7897 review): a directory's ReadFile is EISDIR (pre-fix: left in
// place, so every later run refused "could not checkpoint the swap record"),
// a FIFO's ReadFile blocks forever (pre-fix: hangs startup — pre-existing
// since #7862), and a symlink is the link, not the bytes. Lstat decides
// before anything opens the entry. On efea00bc6 every case leaves the entry
// in place — and the FIFO hangs the ReadFile — so only the directory and
// symlink rows can demonstrate the failure there.
func TestANonRegularSwapRecordEntryIsMovedAsideWithoutBeingOpened(t *testing.T) {
	key := RebuildTeardownKey("/repo", "title")
	rows := []struct {
		name  string
		plant func(t *testing.T, path string)
	}{
		{"directory", func(t *testing.T, path string) {
			require.NoError(t, os.Mkdir(path, 0o700))
		}},
		{"fifo", func(t *testing.T, path string) {
			require.NoError(t, syscall.Mkfifo(path, 0o600))
		}},
		{"symlink", func(t *testing.T, path string) {
			require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "the-real-record"), path))
		}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, accountSwapRecordMarkerName(key))
			row.plant(t, path)

			inst := &Instance{Title: "title"}
			require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key),
				"a non-regular entry is corrupt, not an open — the install returns")
			las, ok := inst.AgentServer().(*localAgentServer)
			require.True(t, ok)
			o, _, id := las.acctSwap.status()
			assert.Equal(t, swapOutcomeNone, o)
			assert.Empty(t, id)

			_, err := os.Lstat(path)
			assert.True(t, os.IsNotExist(err), "the non-regular entry was moved aside")
			asides, err := filepath.Glob(path + ".corrupt-*")
			require.NoError(t, err)
			require.Len(t, asides, 1, "one uniquely-named quarantine sibling")
			info, err := os.Lstat(asides[0])
			require.NoError(t, err)
			assert.False(t, info.Mode().IsRegular(), "the entry itself moved aside, unopened")
		})
	}
}

// #7907: a session dir the install's writability probe cannot write is
// surfaced LOUDLY at install — the dir and the error — instead of being
// discovered at the first swap. The probe changes nothing about how the
// recorder runs: the store is wired whatever the dir's writability, and with
// an earlier record on disk the first swap refuses at the pre-rotation
// checkpoint byte-identically to master (the pre-write's persist hits EACCES
// and the fallback clear hits EACCES, on a dir that can neither create nor
// unlink). Master behaves the same except for the install log — which is the
// fix: the unwritable dir is named at startup, not discovered mid-swap.
func TestAnUnwritableSessionDirLogsAtInstallAndRefusesAtTheCheckpointLikeMaster(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the unwritable-dir probe needs a non-root uid")
	}
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))

	// A prior record exists — written while the dir was healthy; the dir then
	// degraded to readable-but-unwritable, the shape #7907 models.
	require.NoError(t, newAccountSwapStore(path).save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var logs bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &logs)

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key),
		"an unwritable dir still must not stop the agent-server from starting")

	assert.Contains(t, logs.String(), dir,
		"the unwritable dir is surfaced loudly at install (master: no install log — discovered at the first swap)")
	require.NotNil(t, inst.durableAccountSwapStore,
		"the store is wired whatever the dir's writability")

	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	// The readable record rehydrates exactly as on master — a verdict written
	// while healthy stays answerable across the restart.
	o, _, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o)
	assert.Equal(t, "swap-1", id)

	// The first swap refuses at the pre-rotation checkpoint, byte-identical
	// to master: the persist and the fallback clear both hit EACCES.
	ran := false
	_, err := las.acctSwap.run("swap-2", func() (string, error) { ran = true; return "claude", nil })
	require.Error(t, err, "the swap refuses when neither the checkpoint nor the clear can land, exactly as on master")
	assert.False(t, ran, "swap() never ran — the rotation did not move the host")
	assert.Contains(t, err.Error(), "swap-2")
	o, _, id = las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o, "the in-memory slot is restored to the prior swap's record")
	assert.Equal(t, "swap-1", id)

	// The record the swap refused over is unchanged — it still vouches for
	// swap-1, and a repaired-permissions restart rehydrates it.
	d, lerr := newAccountSwapStore(path).load()
	require.NoError(t, lerr)
	assert.Equal(t, "swap-1", d.ID)
}

// The same unwritable dir with NO earlier record (#7907): the install logs
// loudly, and the first swap RUNS exactly as on master — the checkpoint's
// persist fails at create, but the fallback clear tolerates the absent
// record (ENOENT) and the dir sync needs only read, so the swap proceeds
// with no durable backing, and the degrade is logged at the moment it
// happens (the R3 arm). A restart then finds no record and takes the
// reconcile's `none` probe path, exactly like master's empty recorder.
func TestAnUnwritableSessionDirWithNoRecordLogsAtInstallAndRunsLikeMaster(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the unwritable-dir probe needs a non-root uid")
	}
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var logs bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &logs)

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	assert.Contains(t, logs.String(), dir,
		"the unwritable dir is surfaced loudly at install")

	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	ran := false
	_, err := las.acctSwap.run("swap-2", func() (string, error) { ran = true; return "claude", nil })
	require.NoError(t, err,
		"the swap runs: the persist fails at create but the fallback clear tolerates the absent record, exactly as on master")
	assert.True(t, ran, "swap() ran — the rotation was not refused")
	assert.Contains(t, logs.String(), "degraded",
		"the checkpoint's clear-and-proceed degrade is logged at the moment it happens")
	o, _, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o)
	assert.Equal(t, "swap-2", id)
	assert.NoFileExists(t, path, "nothing durable was written for the swap")

	// A restart on the still-unwritable dir rehydrates nothing — there is no
	// record — and the restarted recorder answers anonymous "none", so a
	// reconcile takes master's probe path against the host's own markers.
	inst2 := &Instance{Title: "title"}
	require.NoError(t, inst2.InstallDurableAccountSwapRecord(dir, key))
	require.NotNil(t, inst2.durableAccountSwapStore)
	las2, ok := inst2.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, _, id = las2.acctSwap.status()
	assert.Equal(t, swapOutcomeNone, o,
		"no record survives to be reloaded — the restart reconcile probes the host exactly like a recorder with no store")
	assert.Empty(t, id)
}

// The R2 regression the independent review closed: a record written while the
// dir was healthy stays READABLE on a dir that has since degraded to
// read-only, and the install rehydrates it whatever the dir's writability —
// a settled verdict stays answerable across a restart instead of fencing the
// settled host forever (the #7793 wedge the unbacked posture would have
// recreated).
func TestAReadableRecordInAnUnwritableDirStillRehydratesAtInstall(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions; the unwritable-dir probe needs a non-root uid")
	}
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))
	require.NoError(t, newAccountSwapStore(path).save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var logs bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &logs)

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	assert.Contains(t, logs.String(), dir)
	require.NotNil(t, inst.durableAccountSwapStore)

	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	o, p, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o,
		"the record written while healthy rehydrates whatever the dir's writability")
	assert.Equal(t, "claude", p)
	assert.Equal(t, "swap-1", id)
}

// The probe's own hygiene: a process killed between the probe's create and
// its remove leaves an empty .account-swap-write-probe-* sibling that nothing
// reads, and the next install sweeps it — before running its own probe, so a
// live probe file is never swept.
func TestInstallSweepsAStaleWriteProbeLeftByACrashedInstall(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))
	require.NoError(t, newAccountSwapStore(path).save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))
	stale := filepath.Join(dir, ".account-swap-write-probe-crashed")
	require.NoError(t, os.WriteFile(stale, nil, 0o600))

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	assert.NoFileExists(t, stale, "a crashed install's leftover probe is swept at install")
	assert.FileExists(t, path, "the record itself is untouched")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the dir holds the record and nothing else")
	assert.Equal(t, filepath.Base(path), entries[0].Name())
}

// The healthy-dir case: a session dir that proves writable installs a
// durably-backed recorder exactly as on master — the prior record is
// loaded — and the writability probe leaves nothing behind:
// it can never collide with the record or a *.corrupt-* sibling because it
// carries its own prefix and is removed before the install returns. The
// backed assertions are unchanged-on-master behavior; the no-leftover
// assertions pin the probe's own contract.
func TestAWritableSessionDirInstallsABackedRecorderAndLeavesNoProbeBehind(t *testing.T) {
	dir := t.TempDir()
	key := RebuildTeardownKey("/repo", "title")
	path := filepath.Join(dir, accountSwapRecordMarkerName(key))
	require.NoError(t, newAccountSwapStore(path).save(accountSwapRecordData{
		Outcome: swapOutcomeCommitted, Program: "claude", ID: "swap-1"}))

	inst := &Instance{Title: "title"}
	require.NoError(t, inst.InstallDurableAccountSwapRecord(dir, key))
	require.NotNil(t, inst.durableAccountSwapStore,
		"a writable dir installs a durably-backed recorder")
	las, ok := inst.AgentServer().(*localAgentServer)
	require.True(t, ok)
	require.NotNil(t, las.acctSwap.store)
	o, _, id := las.acctSwap.status()
	assert.Equal(t, swapOutcomeCommitted, o, "the prior record rehydrated, exactly as on master")
	assert.Equal(t, "swap-1", id)

	// The literal prefix (not the const) keeps this file compiling on master,
	// where the backed-path assertions pass as the regression guard they are.
	probes, err := filepath.Glob(filepath.Join(dir, ".account-swap-write-probe-*"))
	require.NoError(t, err)
	assert.Empty(t, probes, "the probe file is removed before the install returns")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the dir holds the record and nothing else")
	assert.Equal(t, filepath.Base(path), entries[0].Name())
}
