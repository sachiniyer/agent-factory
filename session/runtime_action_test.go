package session

import (
	"strings"
	"testing"
)

// TestRuntimeAction_EveryActionHasAnEligibleState is the exhaustiveness ledger
// for runtime-entry actions. A new action must name its valid lifecycle state in
// ValidateRuntimeAction instead of falling through to accidental permission.
func TestRuntimeAction_EveryActionHasAnEligibleState(t *testing.T) {
	valid := map[RuntimeAction]LifecycleView{
		RuntimeActionRestoreArchived:   {Title: "archived", Liveness: LiveArchived},
		RuntimeActionRestoreLostOrDead: {Title: "dead", Liveness: LiveDead, Started: true},
		RuntimeActionRecoverLost:       {Title: "lost", Liveness: LiveLost, Started: true},
		RuntimeActionResumeLimit:       {Title: "limited", Liveness: LiveLimitReached, Started: true},
		RuntimeActionHandoff:           {Title: "live", Liveness: LiveRunning, Started: true},
	}
	for action := RuntimeAction(0); action < numRuntimeActions; action++ {
		view, ok := valid[action]
		if !ok {
			t.Fatalf("runtime action %d is missing from the eligibility ledger", action)
		}
		if err := view.ValidateRuntimeAction(action); err != nil {
			t.Fatalf("runtime action %d rejected its canonical state: %v", action, err)
		}
	}
	if len(valid) != int(numRuntimeActions) {
		t.Fatalf("eligibility ledger has %d entries for %d runtime actions", len(valid), numRuntimeActions)
	}
}

// TestRuntimeAction_PendingKillVetoesEveryAction pins the shared invariant that
// a kill tombstone is terminal intent. It is asserted across the exhaustive
// action ledger so no future runtime-starting verb can forget the veto.
func TestRuntimeAction_PendingKillVetoesEveryAction(t *testing.T) {
	valid := []LifecycleView{
		{Title: "archived", Liveness: LiveArchived},
		{Title: "dead", Liveness: LiveDead, Started: true},
		{Title: "lost", Liveness: LiveLost, Started: true},
		{Title: "limited", Liveness: LiveLimitReached, Started: true},
		{Title: "live", Liveness: LiveRunning, Started: true},
	}
	for action := RuntimeAction(0); action < numRuntimeActions; action++ {
		view := valid[action]
		view.UserKilled = true
		err := view.ValidateRuntimeAction(action)
		if err == nil || !strings.Contains(err.Error(), "pending kill") {
			t.Fatalf("runtime action %d with a tombstone returned %v, want a pending-kill refusal", action, err)
		}
	}
}

func TestRuntimeAction_HandoffRejectsTerminalStates(t *testing.T) {
	for _, liveness := range []Liveness{LiveArchived, LiveLost, LiveDead} {
		view := LifecycleView{Title: "terminal", Liveness: liveness, Started: true}
		if err := view.ValidateRuntimeAction(RuntimeActionHandoff); err == nil {
			t.Fatalf("handoff accepted terminal liveness %v", liveness)
		}
	}
}

// TestRuntimeAction_HandoffRejectsTheReservedTitle pins the reserved-root rule to
// the SHARED predicate rather than to any one caller (#2436).
//
// The daemon has always refused this handoff, but it did so with its own
// IsReservedTitle check after calling here — so the TUI, which asks this
// predicate and nothing else, believed a root handoff was eligible and only
// learned otherwise after making the user pick an agent and confirm. A rule that
// two callers have to remember independently is a rule one of them will forget;
// this is the question both of them already ask.
//
// Root is otherwise perfectly eligible — live, started, no in-flight op — so the
// title is doing all the work here.
func TestRuntimeAction_HandoffRejectsTheReservedTitle(t *testing.T) {
	view := LifecycleView{Title: RootSessionTitle, Liveness: LiveRunning, Started: true}
	err := view.ValidateRuntimeAction(RuntimeActionHandoff)
	if err == nil {
		t.Fatal("handoff accepted the reserved root title; the daemon would reject it after the user confirmed")
	}
	if !strings.Contains(err.Error(), RootSessionTitle) {
		t.Fatalf("refusal must name the session; got %v", err)
	}

	// Case-insensitive on the trimmed title, matching IsReservedTitle — otherwise
	// " ROOT " would route around the guard the daemon still enforces.
	for _, title := range []string{"Root", " ROOT ", "rOoT"} {
		variant := LifecycleView{Title: title, Liveness: LiveRunning, Started: true}
		if err := variant.ValidateRuntimeAction(RuntimeActionHandoff); err == nil {
			t.Fatalf("handoff accepted reserved-title variant %q", title)
		}
	}

	// A name that merely CONTAINS the reserved word is an ordinary session.
	ordinary := LifecycleView{Title: "rootcause", Liveness: LiveRunning, Started: true}
	if err := ordinary.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		t.Fatalf("an ordinary session whose name contains the reserved word must still hand off: %v", err)
	}
}
