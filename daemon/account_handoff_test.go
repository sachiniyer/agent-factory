package daemon

// account_handoff_test.go pins the shadow engine's daemon-level "off is off"
// contract (#6817 review): with no [account_handoff] table anywhere, the
// status-poll observation hook stores nothing, and the ticker does no work
// beyond the cheap check of the already-loaded global config — no per-repo
// resolution, no input walk. The discovery rescan still runs on its slower
// cadence so a personal project's opt-in is found within the interval.

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/handoffengine"
	"github.com/sachiniyer/agent-factory/session"
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

	// A tick inside the rescan window is the cheap path alone: no input
	// walk, no per-repo resolution.
	m.handoffLastScan = nowFunc()
	m.accountHandoffTick()
	if resolved != 0 {
		t.Fatalf("disabled tick resolved configs %d times", resolved)
	}

	// A stale scan does run discovery — that is how a personal project's
	// opt-in is found — and finds nothing here, keeping the engine inactive.
	m.handoffLastScan = nowFunc().Add(-2 * accountHandoffRescanInterval)
	m.accountHandoffTick()
	if resolved != 1 {
		t.Fatalf("discovery scan resolved %d times, want 1", resolved)
	}
	if m.handoffActive.Load() {
		t.Fatalf("engine reported active with nothing governed")
	}

	// While the engine IS active the hook stores again — the gate is the
	// flag, not the config text.
	m.handoffActive.Store(true)
	m.noteHandoffObservation("k", true, "pane text")
	if len(m.handoffObserved) != 1 {
		t.Fatalf("observation dropped while account-handoff is active")
	}
}

func TestAccountHandoffGlobalOptInTicksEveryPass(t *testing.T) {
	resolved := 0
	m := &Manager{
		cfg:       &config.Config{AccountHandoff: config.AccountHandoffConfig{Mode: config.AccountHandoffModeShadow}},
		instances: map[string]*session.Instance{},
	}
	m.handoffEngine = handoffengine.New(handoffengine.Deps{
		Now: nowFunc,
		ResolveConfig: func(root string) (*config.ResolvedConfig, error) {
			resolved++
			return &config.ResolvedConfig{}, nil
		},
	})
	m.handoffLastScan = nowFunc()
	m.accountHandoffTick()
	if !m.handoffActive.Load() {
		t.Fatalf("global shadow mode left the engine inactive")
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
