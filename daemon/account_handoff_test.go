package daemon

// account_handoff_test.go pins the shadow engine's daemon-level "off is off"
// contract (#6817 round-2 review): with no [account_handoff] table anywhere,
// the status-poll observation hook stores nothing and the ticker does LITERALLY
// nothing — no input walk, no per-repo resolution, and no periodic rescan.
// Discovery runs only at daemon start and on config apply; a hand-edited
// personal-project file takes effect on the next `af config` write or daemon
// restart, never on a timer.

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/handoffengine"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestAccountHandoffDisabledDoesNoWork(t *testing.T) {
	resolved := 0
	m := &Manager{
		cfg:       &config.Config{}, // no [account_handoff] table — off
		instances: map[string]*session.Instance{"k": {Title: "t", Path: "/repo"}},
	}
	m.handoffEngine = handoffengine.New(handoffengine.Deps{
		Now: nowFunc,
		ResolveConfig: func(root string) (*config.ResolvedConfig, error) {
			resolved++
			return &config.ResolvedConfig{}, nil
		},
	})

	// The poll hook is inert while inactive: no lock, no store.
	m.noteHandoffObservation("k", true, "pane text")
	if m.handoffObserved != nil {
		t.Fatalf("observation stored while account-handoff is off")
	}

	// A tick while inactive is NOTHING — the rescan timer is gone (#6817
	// round-2 review): no input walk, no per-repo resolution, ever.
	for i := 0; i < 3; i++ {
		m.accountHandoffTick()
	}
	if resolved != 0 {
		t.Fatalf("disabled tick resolved configs %d times", resolved)
	}

	// Discovery — the only way a project opt-in is found — walks the live
	// sessions' roots through the real resolver (the engine's dep is never
	// invoked); finding no shadow config keeps the engine inactive.
	m.accountHandoffDiscover()
	if resolved != 0 {
		t.Fatalf("discovery invoked the engine's ResolveConfig %d times", resolved)
	}
	if m.handoffActive.Load() {
		t.Fatalf("engine reported active with nothing governed")
	}
	m.accountHandoffTick()
	if resolved != 0 {
		t.Fatalf("post-discovery inactive tick resolved configs %d times", resolved)
	}

	// While the engine IS active the hook stores again — the gate is the
	// flag, not the config text.
	m.handoffActive.Store(true)
	m.noteHandoffObservation("k", true, "pane text")
	if len(m.handoffObserved) != 1 {
		t.Fatalf("observation dropped while account-handoff is active")
	}
}

func TestAccountHandoffActiveTickWaitsForCaptures(t *testing.T) {
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

	// No poll observation has landed since activation — the tick waits so a
	// walled session's first verdict never logs a false "draft unprovable"
	// (#6817 round-2 review).
	m.accountHandoffTick()
	if ticked != 0 {
		t.Fatalf("engine ticked with no captures on record")
	}

	// Once the poll records one observation — even a failed capture — the
	// tick runs the input walk.
	m.noteHandoffObservation("k", false, "")
	m.accountHandoffTick()
	if ticked != 1 {
		t.Fatalf("tick ran %d resolves after first observation, want 1", ticked)
	}
}

// #6818 review: a held delivery waits rather than retrying — a recipient
// whose pane a TUI holds reports reportHeld and keeps its backlog.
func TestDeliverHandoffReportHoldsWhileAttached(t *testing.T) {
	inst := &session.Instance{ID: "sess-1", Title: "observer", Path: "/repo"}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{"r/observer": inst},
		pausedPolls: map[string]map[string]time.Time{
			statusPollIDKey("sess-1"): {"tui": nowFunc().Add(time.Hour)},
		},
	}
	if got := m.deliverHandoffReport("sess-1", "[af handoff] kind=x"); got != reportHeld {
		t.Fatalf("attached recipient outcome = %v, want reportHeld", got)
	}
}

// #6818 review: a vanished recipient drops; pending digests die when the
// emitting session's mode or report_to changed — and a changed spec on the
// next job replaces the backlog.
func TestReportPendingDropsOnVanishAndSpecChange(t *testing.T) {
	m := &Manager{cfg: &config.Config{}, instances: map[string]*session.Instance{}}
	if got := m.deliverHandoffReport("sess-gone", "x"); got != reportDropped {
		t.Fatalf("missing recipient outcome = %v, want reportDropped", got)
	}
	pending := map[string]*reportPending{}
	job := handoffReportJob{spec: "observer", key: "k1"}
	m.queueReportRow(pending, job, handoff.EventRow{SessionKey: "emitter"})
	if pending["k1"] == nil || len(pending["k1"].rows) != 1 {
		t.Fatalf("row was not queued: %+v", pending)
	}
	// The emitter is gone → liveReportRows empties the backlog.
	if rows := m.liveReportRows(pending["k1"]); len(rows) != 0 {
		t.Fatalf("gone emitter kept %d rows", len(rows))
	}
	// A different spec on the next job replaces, not appends.
	changed := handoffReportJob{spec: "other", key: "k1"}
	m.queueReportRow(pending, changed, handoff.EventRow{SessionKey: "emitter"})
	if len(pending["k1"].rows) != 1 || pending["k1"].spec != "other" {
		t.Fatalf("spec change did not replace backlog: %+v", pending["k1"])
	}
	// The cap bounds a held recipient's backlog.
	for i := 0; i < reportPendingCap+10; i++ {
		m.queueReportRow(pending, job, handoff.EventRow{SessionKey: "emitter"})
	}
	// spec differs again — replaced to 1 row, then fill to cap:
	pending["k1"].rows = pending["k1"].rows[:0]
	pending["k1"].spec = job.spec
	for i := 0; i < reportPendingCap+10; i++ {
		m.queueReportRow(pending, job, handoff.EventRow{SessionKey: "emitter"})
	}
	if len(pending["k1"].rows) != reportPendingCap {
		t.Fatalf("backlog exceeded cap: %d", len(pending["k1"].rows))
	}
}

// #6818 review: shadow mode has no immediate-delivery path — a halted row
// goes through the same digest backlog as every other row. Immediate
// delivery returns with the live mover in slice 6, where halts exist.
func TestQueueReportJobBatchesHaltedRows(t *testing.T) {
	m := &Manager{cfg: &config.Config{}, instances: map[string]*session.Instance{}}
	pending := map[string]*reportPending{}
	m.queueReportJob(pending, handoffReportJob{
		spec: "observer", key: "k1",
		rows: []handoff.EventRow{
			{Kind: handoff.EventHalted, ReasonCode: "mover-halted", SessionKey: "mover"},
			{Kind: handoff.EventWall, ReasonCode: "wall", SessionKey: "emitter"},
		},
	})
	if len(pending["k1"].rows) != 2 {
		t.Fatalf("halted row did not batch into the digest backlog: %+v", pending["k1"])
	}
}

// #6818 round-2 review: a report never delivers over a composer af cannot
// read — a fresh capture right before the send must prove it empty. A
// recipient with no live runtime is the unprovable case: the send is held
// for a later flush rather than pasted over an unseen draft.
func TestDeliverHandoffReportHoldsOnUnprovableComposer(t *testing.T) {
	inst := &session.Instance{ID: "sess-2", Title: "observer", Path: "/repo"}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{"r/observer": inst},
	}
	if got := m.deliverHandoffReport("sess-2", "[af handoff] kind=x"); got != reportHeld {
		t.Fatalf("unprovable-composer recipient outcome = %v, want reportHeld", got)
	}
}

// #6817 round-3 review: the most recent config decision wins. A tick that
// began while active must not re-activate from its stale governed count
// after ApplyConfig's discovery has turned shadow off mid-tick.
func TestAccountHandoffTickNeverReactivatesAfterApplyConfig(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{}, // no [account_handoff] table — off
		instances: map[string]*session.Instance{"k": {Title: "t", Path: "/repo"}},
	}
	m.handoffEngine = handoffengine.New(handoffengine.Deps{
		Now: nowFunc,
		ResolveConfig: func(root string) (*config.ResolvedConfig, error) {
			// Simulate ApplyConfig landing mid-tick: discovery recomputes
			// from the live config — no opt-in anywhere — and stores off.
			m.accountHandoffDiscover()
			cfg := &config.ResolvedConfig{ProjectRoot: root}
			cfg.AccountHandoff.Mode = config.AccountHandoffModeShadow
			return cfg, nil
		},
		LoadState:       func() (handoff.State, error) { return handoff.State{SchemaVersion: 1}, nil },
		Registered:      func(string) ([]string, error) { return nil, nil },
		LimitedAccounts: func(string) (map[string]bool, error) { return map[string]bool{}, nil },
		AccountDir:      func(string, string) (string, error) { return "", nil },
		ReadUsage: func(string, map[string]int64) handoff.ReadingFacts {
			return handoff.ReadingFacts{Status: handoff.ReadingNone}
		},
		DriftKey:   func(string, string, string) (string, bool) { return "", false },
		DetectWall: func(string, string) bool { return false },
		WriteRows:  func([]handoff.EventRow) error { return nil },
		LogWarn:    func(string, ...any) {},
	})
	m.handoffActive.Store(true)
	m.noteHandoffObservation("k", false, "") // unlock the input walk
	m.accountHandoffTick()
	if m.handoffActive.Load() {
		t.Fatalf("tick re-activated shadow after ApplyConfig turned it off")
	}
}

// #6817 final review: session create and materialise must never run
// account-handoff discovery — they resolve every repo's config while
// holding the create's reservation and start lock, for users who never
// opted in. [account_handoff] is rejected in in-repo config, and a global
// or personal opt-in arrives through `af config` writes, which run
// discovery on apply. Counting the seam proves create fires none.
func TestCreateSessionWhileOffTriggersNoDiscovery(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	calls := 0
	old := accountHandoffDiscovery
	accountHandoffDiscovery = func(*Manager) { calls++ }
	t.Cleanup(func() { accountHandoffDiscovery = old })

	_, err = manager.CreateSession(nil, CreateSessionRequest{
		Title:    "no-discovery-on-create",
		RepoPath: repoPath,
		Program:  tmux.ProgramClaude,
	})
	require.NoError(t, err)
	if calls != 0 {
		t.Fatalf("session create triggered %d account-handoff discoveries", calls)
	}
}

// #6818 final review: a held recipient must never stay silent — and must
// not trigger a full resolve+restore every minute. noteReportHeld doubles
// the retry backoff to a cap and warns once per held episode, durably
// enough for af quota's mover line to carry it.
func TestReportHeldBackoffAndWarnOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	m := &Manager{cfg: &config.Config{}, instances: map[string]*session.Instance{}}
	entry := &reportPending{spec: "observer"}

	now := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = oldNow })

	// The first hold schedules the next attempt a flush out — no warn yet.
	m.noteReportHeld("k", entry)
	if entry.backoff != reportFlushInterval || !entry.nextAttempt.Equal(now.Add(reportFlushInterval)) {
		t.Fatalf("first hold backoff=%v next=%v", entry.backoff, entry.nextAttempt)
	}
	if entry.warned {
		t.Fatalf("warned before the threshold")
	}

	// Past the warn threshold the latch fires once and the record lands for
	// af quota — with the OLDEST hold time.
	now = now.Add(reportHeldWarnAfter + time.Minute)
	m.noteReportHeld("k", entry)
	if !entry.warned {
		t.Fatalf("no warn past the threshold")
	}
	if entry.backoff != 2*reportFlushInterval {
		t.Fatalf("backoff did not double: %v", entry.backoff)
	}
	store, err := handoff.DefaultStore()
	require.NoError(t, err)
	state, err := store.Load()
	require.NoError(t, err)
	if state.ReportsHeld == nil || !state.ReportsHeld.Since.Equal(entry.heldSince) {
		t.Fatalf("ReportsHeld = %+v, want since %v", state.ReportsHeld, entry.heldSince)
	}

	// The latch holds: further holds never re-warn and backoff caps.
	for i := 0; i < 20; i++ {
		m.noteReportHeld("k", entry)
	}
	if entry.backoff > reportHeldBackoffMax {
		t.Fatalf("backoff exceeded cap: %v", entry.backoff)
	}

	// Draining the last warned backlog clears the durable record; a
	// still-warned sibling keeps it standing.
	m.clearReportsHeldIf(map[string]*reportPending{"k": entry, "other": {warned: true}}, entry)
	state, _ = store.Load()
	if state.ReportsHeld == nil {
		t.Fatalf("record cleared while another backlog is held")
	}
	m.clearReportsHeldIf(map[string]*reportPending{"k": entry}, entry)
	state, _ = store.Load()
	if state.ReportsHeld != nil {
		t.Fatalf("record survived the last held backlog draining")
	}
}
