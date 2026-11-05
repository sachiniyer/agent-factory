package daemon

// handoff_status_test.go pins the slice-5 surface of the durable rails
// (#6776): the engine tick's one-move-at-a-time mutex, the status/reset
// control calls behind `af accounts handoff`, the sandbox scope denial, and
// the contract that a recorded mover halt gates only engine-originated moves
// — never the ordinary wait-for-reset resume path.

import (
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/handoffengine"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// The one-move-at-a-time rail: a mover in flight holds handoffMoveMu across
// the whole swap, and the engine tick TryLocks — a busy mover skips the tick
// rather than queueing behind it, so a minutes-long swap can never stack
// ticks behind it. Shadow mode moves nothing, but the mutex exists now so
// the live slice inherits a tested seam.
func TestAccountHandoffTick_SkipsWhileMoveInFlight(t *testing.T) {
	ticked := 0
	m := &Manager{
		cfg:       &config.Config{AccountHandoff: config.AccountHandoffConfig{Mode: config.AccountHandoffModeShadow}},
		instances: map[string]*session.Instance{"k": {Title: "t", Path: "/repo"}},
	}
	m.handoffEngine = handoffengine.New(handoffengine.Deps{
		Now: nowFunc,
		ResolveConfig: func(root string) (*config.ResolvedConfig, error) {
			ticked++
			return &config.ResolvedConfig{}, nil
		},
	})
	m.handoffActive.Store(true)
	m.noteHandoffObservation("k", true, "pane text")

	m.handoffMoveMu.Lock()
	m.accountHandoffTick()
	m.handoffMoveMu.Unlock()
	if ticked != 0 {
		t.Fatalf("tick ran the engine while a move holds the rail")
	}
	m.accountHandoffTick()
	if ticked != 1 {
		t.Fatalf("free tick ran %d resolves, want 1", ticked)
	}
}

// handoffStatusFixture builds a controlServer over a temp af home whose
// config resolves the given mode and whose state file the test writes.
func handoffStatusFixture(t *testing.T, mode string) *controlServer {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	cfg := config.DefaultConfig()
	cfg.AccountHandoff.Mode = mode
	require.NoError(t, config.SaveConfig(cfg))
	return &controlServer{manager: &Manager{instances: map[string]*session.Instance{}}}
}

func TestHandoffStatus_ReportsModeRecordsAndEngagement(t *testing.T) {
	s := handoffStatusFixture(t, config.AccountHandoffModeShadow)
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	haltAt := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	until := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	moveAt := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	require.NoError(t, store.RecordHalt("move-failed", haltAt))
	require.NoError(t, store.SetRefusedUntil("sess-gone", until))
	require.NoError(t, store.RecordMove("sess-1", "account-b", moveAt))
	s.manager.handoffActive.Store(true)

	var resp HandoffStatusResponse
	require.NoError(t, s.HandoffStatus(HandoffStatusRequest{}, &resp))
	require.Equal(t, "shadow", resp.Mode)
	require.True(t, resp.Engaged)
	require.True(t, resp.Halted)
	require.Equal(t, "move-failed", resp.HaltReason)
	require.NotNil(t, resp.HaltAt)
	require.True(t, resp.HaltAt.Equal(haltAt))
	require.Len(t, resp.Backoffs, 1)
	require.Equal(t, "sess-gone", resp.Backoffs[0].SessionKey)
	require.Empty(t, resp.Backoffs[0].Title, "a record for a session af no longer tracks still prints under its key")
	require.Len(t, resp.RecentMoves, 1)
	require.Equal(t, "account-b", resp.RecentMoves[0].Account)
}

// A live session's title rides its stable key as a label — the key stays the
// identity (R13): titles are never what the rail records.
func TestHandoffStatus_LabelsLiveSessionsByKey(t *testing.T) {
	s := handoffStatusFixture(t, config.AccountHandoffModeOff)
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	inst := &session.Instance{Title: "alpha", ID: "sess-1"}
	s.manager.instances[daemonInstanceKey("repo", "alpha")] = inst
	require.NoError(t, store.SetRefusedUntil(stableSessionKey("repo", inst), time.Now().Add(time.Hour)))

	var resp HandoffStatusResponse
	require.NoError(t, s.HandoffStatus(HandoffStatusRequest{}, &resp))
	require.Len(t, resp.Backoffs, 1)
	require.Equal(t, "sess-1", resp.Backoffs[0].SessionKey)
	require.Equal(t, "alpha", resp.Backoffs[0].Title)
}

// A corrupt rails file IS the answer status exists to find — it errors
// rather than rendering an authoritative empty status.
func TestHandoffStatus_FailsClosedOnCorruptState(t *testing.T) {
	s := handoffStatusFixture(t, config.AccountHandoffModeOff)
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	require.NoError(t, store.RecordHalt("move-failed", time.Now()))
	require.NoError(t, os.WriteFile(store.Path(), []byte("{not json"), 0o600))

	var resp HandoffStatusResponse
	require.Error(t, s.HandoffStatus(HandoffStatusRequest{}, &resp))
	// And the reset path fails closed the same way — it must never rebuild
	// over a file it cannot read.
	require.Error(t, s.HandoffReset(HandoffResetRequest{Record: "halt"}, &HandoffResetResponse{}))
}

// The reset grammar's safety property: clearing one record leaves every
// other rail intact, so clearing a halt can never silently take the loop
// counters or the backoffs with it.
func TestHandoffReset_ClearsOnlyTheNamedRecord(t *testing.T) {
	s := handoffStatusFixture(t, config.AccountHandoffModeShadow)
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	require.NoError(t, store.RecordHalt("move-failed", time.Now()))
	require.NoError(t, store.SetRefusedUntil("sess-1", time.Now().Add(time.Hour)))
	require.NoError(t, store.RecordMove("sess-1", "account-b", time.Now()))

	var resp HandoffResetResponse
	require.NoError(t, s.HandoffReset(HandoffResetRequest{Record: "halt"}, &resp))
	require.True(t, resp.Changed)
	state, err := store.Load()
	require.NoError(t, err)
	require.Nil(t, state.Halted)
	require.Len(t, state.RefusedUntil, 1, "a halt reset must not touch the backoffs")
	require.Len(t, state.RecentMoves, 1, "a halt reset must not touch the move ledger")

	// A scoped clear drops only that session's entries.
	require.NoError(t, s.HandoffReset(HandoffResetRequest{Record: "backoff", SessionKey: "sess-1"}, &resp))
	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.RefusedUntil)
	require.Len(t, state.RecentMoves, 1)

	require.NoError(t, s.HandoffReset(HandoffResetRequest{Record: "moves"}, &resp))
	state, err = store.Load()
	require.NoError(t, err)
	require.Empty(t, state.RecentMoves)
}

func TestHandoffReset_ValidatesTheNamedRecord(t *testing.T) {
	s := handoffStatusFixture(t, config.AccountHandoffModeOff)
	var resp HandoffResetResponse
	require.Error(t, s.HandoffReset(HandoffResetRequest{}, &resp), "an unnamed reset refuses")
	require.Error(t, s.HandoffReset(HandoffResetRequest{Record: "everything"}, &resp), "an unknown record refuses")
	require.Error(t, s.HandoffReset(HandoffResetRequest{Record: "halt", SessionKey: "sess-1"}, &resp),
		"a session scope on the mover-level halt refuses")
	// A reset that matches nothing is a truthful answer, not an error.
	require.NoError(t, s.HandoffReset(HandoffResetRequest{Record: "halt"}, &resp))
	require.False(t, resp.Changed)
	require.Contains(t, resp.Summary, "no halt")
}

// The sandbox decision: a sandbox credential earns neither verb. status
// reports the operator's fleet state — session identities, account names, a
// mover's halt — with no session owner to narrow to; reset is operator
// authority over the mover's rails themselves.
func TestHandoffRoutesAreNotSandboxReachable(t *testing.T) {
	for _, path := range []string{"/v1/HandoffStatus", "/v1/HandoffReset"} {
		require.Falsef(t, sandboxAllowedPaths[path],
			"%s must stay out of the sandbox scope — fleet rails are operator state", path)
	}
}

// A recorded mover halt gates only engine-originated moves: the ordinary
// wait-for-reset resume path never consults the rails file, so a parked
// session must resume on its own schedule while the mover is halted. This is
// the contract the live-mover slice cannot later drift (design R16).
func TestResumeLimitedSessions_IgnoresRecordedHandoffHalt(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	resetAt := base.Add(time.Hour)
	manager, _, _, backend := newAutoResumeManager(t, "", true, "finish the migration", resetAt)

	// The halt record exists in the af home this manager runs over —
	// newAutoResumeManager's newStatusTestManager pins AGENT_FACTORY_HOME.
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	require.NoError(t, store.RecordHalt("move-failed", base))

	advance(time.Hour + limitResumeGrace + time.Second)
	manager.ResumeLimitedSessions()

	_, _, prompts := backend.snapshot()
	if len(prompts) != 1 || prompts[0] != "finish the migration" {
		t.Fatalf("the recorded handoff halt gated the ordinary resume: prompts=%v", prompts)
	}
}
