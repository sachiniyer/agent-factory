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

	"github.com/sachiniyer/agent-factory/config"
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
