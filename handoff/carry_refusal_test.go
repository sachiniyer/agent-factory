package handoff

import (
	"testing"
	"time"
)

// carryParam is the carry code on d's carry-refused event, and whether there
// is one.
func carryParam(d Decision) (string, bool) {
	for _, e := range d.Events {
		if e.Kind == EventIgnored && e.Reason == reasonCarryRefused {
			return e.Params["carry"], true
		}
	}
	return "", false
}

// carry-refused says why with a carry refusal code, and only ever a code: the
// carry's prose can embed a path or an account name (#7156 review P3-2).
func TestDecideCarryRefusedNamesWhy(t *testing.T) {
	for _, tc := range []struct{ refusal, want string }{
		{"", CarryUnrecorded},
		{CarryBacktracked, CarryBacktracked},
		{CarryRemoteWorkspace, CarryRemoteWorkspace},
		{"/home/u/.codex/sessions is a symbolic link", CarryUnconfirmed},
	} {
		f := wallConfirmedFacts()
		f.Config.RequireCarry = true
		f.Session.CarryPossible, f.Session.CarryRefusal = false, tc.refusal
		got, ok := carryParam(Decide(f))
		if !ok || got != tc.want {
			t.Fatalf("refusal %q: carry=%q (event %v), want %q", tc.refusal, got, ok, tc.want)
		}
	}
}

// A new reason for the same refusal is a new verdict: the shadow log records
// it rather than folding it into the standing carry-refused edge.
func TestCarryRefusedEdgeChangesWithItsReason(t *testing.T) {
	refused := func(code string) []Event {
		f := wallConfirmedFacts()
		f.Config.RequireCarry = true
		f.Session.CarryPossible, f.Session.CarryRefusal = false, code
		return Decide(f).Events
	}
	tracker := NewEdgeTracker()
	if len(tracker.Filter("s", refused(CarryStale), testNow, time.Hour)) == 0 {
		t.Fatal("the first carry-refused verdict was not recorded")
	}
	if len(tracker.Filter("s", refused(CarryStale), testNow.Add(time.Minute), time.Hour)) != 0 {
		t.Fatal("an unchanged carry-refused verdict re-emitted inside the heartbeat")
	}
	if len(tracker.Filter("s", refused(CarryBacktracked), testNow.Add(2*time.Minute), time.Hour)) == 0 {
		t.Fatal("a carry refused for a new reason was folded into the old edge")
	}
}
