package handoff

import (
	"strings"
	"testing"
)

// eventOf is d's first event of kind, or fails.
func eventOf(t *testing.T, d Decision, kind EventKind) Event {
	t.Helper()
	for _, e := range d.Events {
		if e.Kind == kind {
			return e
		}
	}
	t.Fatalf("decision lacks a %s event (kind=%s reason=%q events=%v)", kind, d.Kind, d.Reason, d.Events)
	return Event{}
}

// Under require_carry, a candidate whose own home refuses the carry is a
// stable state of that one home: retrying it cannot clear it, and another
// candidate's home may carry. So it is skipped the way a drifted candidate
// is, with the account and its code on the row, rather than holding the
// session back (#7282).
func TestDecideSkipsACandidateWhoseHomeRefusesTheCarry(t *testing.T) {
	f := wallConfirmedFacts()
	f.Config.RequireCarry = true
	first := Decide(f).To
	if first == "" {
		t.Fatal("fixture: the session must move")
	}
	for i := range f.Accounts {
		if f.Accounts[i].Name == first {
			f.Accounts[i].CarryRefusal = CarryDuplicated
		}
	}
	d := Decide(f)
	wantDecision(t, d, KindMove, reasonTriggerWall)
	if d.To == first || d.To == "" {
		t.Fatalf("moved to %q; want the next candidate after %q, whose home refused", d.To, first)
	}
	if got := eventOf(t, d, EventMove).Params["skipped"]; !strings.Contains(got, first+"=carry:duplicated") {
		t.Fatalf("skipped=%q does not name %q and its carry code", got, first)
	}

	// Every candidate's home refuses: nothing can take the session with its
	// conversation, and the row names each home and why.
	second := d.To
	for i := range f.Accounts {
		if f.Accounts[i].Name == second {
			f.Accounts[i].CarryRefusal = CarryBacktracked
		}
	}
	d = Decide(f)
	wantDecision(t, d, KindParkedNoAccount, reasonNoEligible)
	got := eventOf(t, d, EventNoAccount).Params["skipped"]
	for _, want := range []string{first + "=carry:duplicated", second + "=carry:backtracked"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipped=%q lacks %q", got, want)
		}
	}
	wantNoEvent(t, d, EventMove)
}

// A require_carry refusal that one target account caused names that
// target on its row, so the row says which account's home to look at; a
// source-side refusal names none (#7282).
func TestDecideCarryRefusedNamesTheTarget(t *testing.T) {
	f := wallConfirmedFacts()
	f.Config.RequireCarry = true
	f.Session.CarryPossible, f.Session.CarryRefusal, f.Session.CarryRefusalTo = false, CarryUnconfirmed, "backup"
	d := Decide(f)
	wantDecision(t, d, KindDefer, reasonCarryRefused)
	e := eventOf(t, d, EventIgnored)
	if e.Reason != reasonCarryRefused || e.To != "backup" || e.Params["carry"] != CarryUnconfirmed {
		t.Fatalf("carry-refused event = %+v, want to=backup carry=%s", e, CarryUnconfirmed)
	}

	f.Session.CarryRefusalTo = ""
	if e := eventOf(t, Decide(f), EventIgnored); e.To != "" {
		t.Fatalf("a source-side refusal named target %q", e.To)
	}
}
