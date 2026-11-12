package daemon

// account_handoff_test.go pins the shadow engine's daemon-level "off is off"
// contract (#6817 round-2 review): with no [account_handoff] table anywhere,
// the status-poll observation hook stores nothing and the ticker does LITERALLY
// nothing — no input walk, no per-repo resolution, and no periodic rescan.
// Discovery runs only at daemon start and on config apply; a hand-edited
// personal-project file takes effect on the next `af config` write or daemon
// restart, never on a timer.

import (
	"encoding/json"
	"sync"
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
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	inst := &session.Instance{ID: "sess-1", Title: "observer", Path: "/repo"}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{"r/observer": inst},
		pausedPolls: map[string]map[string]time.Time{
			statusPollIDKey("sess-1"): {"tui": nowFunc().Add(time.Hour)},
		},
	}
	if got := m.deliverHandoffReport("observer", "sess-1", "[af handoff] kind=x"); got != reportHeld {
		t.Fatalf("attached recipient outcome = %v, want reportHeld", got)
	}
}

// #6818 review: a vanished recipient drops; pending digests die when the
// emitting session's mode or report_to changed — and a changed spec on the
// next job replaces the backlog.
func TestReportPendingDropsOnVanishAndSpecChange(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	m := &Manager{cfg: &config.Config{}, instances: map[string]*session.Instance{}}
	if got := m.deliverHandoffReport("observer", "sess-gone", "x"); got != reportDropped {
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
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	inst := &session.Instance{ID: "sess-2", Title: "observer", Path: "/repo"}
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{"r/observer": inst},
	}
	if got := m.deliverHandoffReport("observer", "sess-2", "[af handoff] kind=x"); got != reportHeld {
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
		LoadState: func() (handoff.Snapshot, error) {
			return handoff.Snapshot{State: handoff.State{SchemaVersion: 1}}, nil
		},
		Registered:      func(string) ([]string, error) { return nil, nil },
		LimitedAccounts: func(string) (map[string]bool, error) { return map[string]bool{}, nil },
		AccountDir:      func(string, string) (string, error) { return "", nil },
		ReadUsage: func(string, map[string]int64) handoff.ReadingFacts {
			return handoff.ReadingFacts{Status: handoff.ReadingNone}
		},
		DriftKey:   func(string, string, string, string) (string, bool) { return "", false },
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
	store, err := handoff.DefaultRailsStore()
	require.NoError(t, err)
	rails, err := store.Load()
	require.NoError(t, err)
	if rails.ReportsHeld == nil || !rails.ReportsHeld.Since.Equal(entry.heldSince) {
		t.Fatalf("ReportsHeld = %+v, want since %v", rails.ReportsHeld, entry.heldSince)
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
	rails, _ = store.Load()
	if rails.ReportsHeld == nil {
		t.Fatalf("record cleared while another backlog is held")
	}
	m.clearReportsHeldIf(map[string]*reportPending{"k": entry}, entry)
	rails, _ = store.Load()
	if rails.ReportsHeld != nil {
		t.Fatalf("record survived the last held backlog draining")
	}
}

// #6818 review: a persisted row that never materialized (failed restore)
// still claims its title — accountHandoffInputs must feed it as a Persisted
// claim so a bare report_to cannot read unique against it.
func TestAccountHandoffInputsFeedsPersistedTitleClaims(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	raw, err := json.Marshal([]session.InstanceData{{
		ID: "ghost-obs-id", Title: "observer", Path: "/repoB",
	}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repoB", raw))

	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{},
	}
	m.instances[daemonInstanceKey("repoA", "observer")] = &session.Instance{Title: "observer"}

	var ghost *handoffengine.SessionInput
	inputs := m.accountHandoffInputs()
	for i := range inputs {
		if inputs[i].Persisted {
			ghost = &inputs[i]
		}
	}
	if ghost == nil {
		t.Fatalf("persisted unloadable row missing from inputs: %+v", inputs)
	}
	if ghost.Title != "observer" || ghost.RepoID != "repoB" || ghost.RepoRoot != "/repoB" {
		t.Fatalf("persisted claim identity wrong: %+v", ghost)
	}
	if ghost.Live || ghost.Started {
		t.Fatalf("persisted claim masqueraded as live: %+v", ghost)
	}
}

// A persisted row that DID materialize counts once — the live input, not a
// second ambiguity claim against itself.
func TestAccountHandoffInputsDedupesMaterializedRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	raw, err := json.Marshal([]session.InstanceData{{
		ID: "live-obs-id", Title: "observer", Path: "/repoA",
	}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repoA", raw))

	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{},
	}
	m.instances[daemonInstanceKey("repoA", "observer")] = &session.Instance{ID: "live-obs-id", Title: "observer"}

	var observerInputs int
	for _, in := range m.accountHandoffInputs() {
		if in.Title == "observer" {
			observerInputs++
			if in.Persisted {
				t.Fatalf("materialized session fed as persisted: %+v", in)
			}
		}
	}
	if observerInputs != 1 {
		t.Fatalf("observer fed %d times, want 1 (persisted+live dedupe)", observerInputs)
	}
}

// #6818 review: a restart abandons the in-memory backlog and warned latch —
// the report worker reconciles the durable held record at start so it can
// never stay asserted with no live hold behind it.
func TestHandoffReportWorkerReconcilesHeldMarkerOnStart(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	store, err := handoff.DefaultRailsStore()
	require.NoError(t, err)
	require.NoError(t, store.NoteReportsHeld(time.Now().Add(-time.Hour)))
	rails, err := store.Load()
	require.NoError(t, err)
	require.NotNil(t, rails.ReportsHeld)

	m := &Manager{
		cfg:             &config.Config{},
		instances:       map[string]*session.Instance{},
		handoffReportCh: make(chan handoffReportJob, 1),
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	startHandoffReportLoop(m, stop, &wg)

	deadline := time.Now().Add(5 * time.Second)
	for {
		rails, err := store.Load()
		require.NoError(t, err)
		if rails.ReportsHeld == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stale held record survived the report worker's restart reconcile")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

// #6818 review: delivery must re-resolve its spec at send time every time —
// a same-titled session created while the digest sat held makes the spec
// ambiguous, and the queued rows retire rather than ride a stale resolution
// to whichever session it would now guess.
func TestDeliverHandoffReportRevalidatesSpecAtSendTime(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{},
	}
	keyA := daemonInstanceKey("repoA", "observer")
	m.instances[keyA] = &session.Instance{Title: "observer"}
	m.instances[daemonInstanceKey("repoB", "observer")] = &session.Instance{Title: "observer"}
	m.instances[daemonInstanceKey("repoC", "monitor")] = &session.Instance{Title: "monitor"}

	// Two live "observer" claims: the spec is now ambiguous — drop, never
	// send on the stale resolution.
	if got := m.deliverHandoffReport("observer", keyA, "digest"); got != reportDropped {
		t.Fatalf("ambiguous spec sent: outcome %v", got)
	}
	// A spec resolving to a DIFFERENT session also drops.
	if got := m.deliverHandoffReport("monitor", keyA, "digest"); got != reportDropped {
		t.Fatalf("retargeted spec sent to the queued recipient: outcome %v", got)
	}
	// Absent spec drops too.
	if got := m.deliverHandoffReport("nobody", keyA, "digest"); got != reportDropped {
		t.Fatalf("absent spec sent: outcome %v", got)
	}
	// Restore uniqueness: the spec resolves to keyA again and delivery
	// reaches the send gates — the unstarted target holds.
	delete(m.instances, daemonInstanceKey("repoB", "observer"))
	if got := m.deliverHandoffReport("observer", keyA, "digest"); got != reportHeld {
		t.Fatalf("uniquely-resolved recipient never reached the send gates: %v", got)
	}
}

// #6818 review: a spec-changed backlog's warned latch is lost with the entry
// — its durable held record must retire in the same move, while another
// recipient's warned episode keeps the marker standing.
func TestQueueReportRowReplaceClearsHeldMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	store, err := handoff.DefaultRailsStore()
	require.NoError(t, err)
	require.NoError(t, store.NoteReportsHeld(time.Now()))

	m := &Manager{cfg: &config.Config{}, instances: map[string]*session.Instance{}}
	pending := map[string]*reportPending{"k": {spec: "observer", warned: true}}
	m.queueReportRow(pending,
		handoffReportJob{key: "k", spec: "monitoring/observer"}, handoff.EventRow{})
	rails, err := store.Load()
	require.NoError(t, err)
	if rails.ReportsHeld != nil {
		t.Fatal("replaced backlog's held marker survived its retirement")
	}

	// A warned sibling keeps the marker standing through the replacement.
	require.NoError(t, store.NoteReportsHeld(time.Now()))
	pending = map[string]*reportPending{
		"k":     {spec: "observer", warned: true},
		"other": {spec: "watcher", warned: true},
	}
	m.queueReportRow(pending,
		handoffReportJob{key: "k", spec: "monitoring/observer"}, handoff.EventRow{})
	rails, err = store.Load()
	require.NoError(t, err)
	if rails.ReportsHeld == nil {
		t.Fatal("marker cleared while another recipient's warned backlog still stands")
	}
}

// #6818 review: a persisted claim's root is the worktree's resolved repo
// path — the identity a materialized session reports through GetRepoPath —
// not the possibly-symlinked launch Path. A symlinked create must keep its
// ambiguity veto when its restore fails; TestDaemonSaveUsesResolvedRepoPath
// ForSymlinkedRepo pins that this shape is real on disk.
func TestAccountHandoffInputsPersistedClaimUsesWorktreeRepoPath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))

	raw, err := json.Marshal([]session.InstanceData{{
		ID:    "ghost-obs-id",
		Title: "observer",
		Path:  "/links/alias", // symlinked launch path
		Worktree: session.GitWorktreeData{
			RepoPath: "/a/repo", // canonical root the row was created under
		},
	}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repoA", raw))

	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{},
	}
	m.instances[daemonInstanceKey("repoB", "observer")] = &session.Instance{Title: "observer", Path: "/b/repo"}

	var ghost *handoffengine.SessionInput
	inputs := m.accountHandoffInputs()
	for i := range inputs {
		if inputs[i].Persisted {
			ghost = &inputs[i]
		}
	}
	if ghost == nil {
		t.Fatalf("persisted unloadable row missing from inputs: %+v", inputs)
	}
	if ghost.RepoRoot != "/a/repo" {
		t.Fatalf("persisted claim rooted at %q — qualified specs see a different identity than materialized sessions", ghost.RepoRoot)
	}
	// repo/observer must now read ambiguous: the ghost claims the same
	// basename root as the live /b/repo session — skip, never redirect.
	if _, found, _ := handoffengine.ResolveRecipient("repo/observer", inputs); found {
		t.Fatalf("qualified spec resolved despite the persisted same-root claim")
	}
}

// require_carry's full answer reaches the engine for every materialized
// session (#7152): the input carries the session's own AccountSwapCarryRefusal,
// to be asked after m.mu is released. A session with no recorded conversation
// cannot carry, and both answers say why with a code (#7156 review P3-2).
func TestAccountHandoffInputsCarryTheLiveCarryAnswer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	require.NoError(t, config.SaveConfig(config.DefaultConfig()))
	m := &Manager{
		cfg:       &config.Config{},
		instances: map[string]*session.Instance{},
	}
	m.instances[daemonInstanceKey("repoA", "worker")] = &session.Instance{
		ID: "carry-id", Title: "worker", Program: tmux.ProgramCodex,
	}
	var found bool
	for _, in := range m.accountHandoffInputs() {
		if in.Title != "worker" || in.Persisted {
			continue
		}
		found = true
		require.NotNil(t, in.CarryCheck, "the engine cannot honour require_carry without the live carry's answer")
		require.False(t, in.CarryPossible)
		require.Equal(t, handoff.CarryUnrecorded, in.CarryRefusal)
		require.NotEmpty(t, in.CarryCheck())
	}
	require.True(t, found)
}
