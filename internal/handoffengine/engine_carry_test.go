package handoffengine

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/handoff"
)

// carryRefusedReason is the leaf's require_carry blocker code.
const carryRefusedReason = "carry-refused"

// movableTick runs one shadow tick over a session the engine would move: a
// wall proven by a hot reading on the session's own home, an empty composer,
// and two eligible candidates. It returns the rows written and how many times
// the session's CarryCheck was asked.
func movableTick(t *testing.T, requireCarry bool, edit func(*SessionInput), check func() string) ([]handoff.EventRow, int) {
	t.Helper()
	d := &depCounts{mode: "shadow", requireCarry: requireCarry}
	in := shadowSession()
	in.Account, in.LimitAccount = "third", "third"
	in.HomeDir = "/home/codex/third"
	hot := in.HomeDir // the home the 98% reading belongs to
	in.Content = "You've hit your usage limit\n› Ask Codex to do anything\n"
	asked := 0
	if check != nil {
		in.CarryCheck = func() string {
			asked++
			return check()
		}
		// The target side carries unless a test says otherwise.
		in.CarryCheckTo = func(string) string { return "" }
	}
	if edit != nil {
		edit(&in)
	}
	deps := d.deps()
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		if dir == hot {
			return handoff.ReadingFacts{
				Status:     handoff.ReadingObserved,
				Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: 98}},
				RecordedAt: testNow.Add(-time.Minute), Complete: true,
			}
		}
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	if _, err := New(deps).Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	return d.rows, asked
}

func rowsHold(rows []handoff.EventRow, kind handoff.EventKind, reason string) bool {
	for _, row := range rows {
		if row.Kind == kind && (reason == "" || row.ReasonCode == reason) {
			return true
		}
	}
	return false
}

// carryRefusedCode is the carry code on the carry-refused row, or "".
func carryRefusedCode(rows []handoff.EventRow) string {
	for _, row := range rows {
		if row.Kind == handoff.EventIgnored && row.ReasonCode == carryRefusedReason {
			return row.Params["carry"]
		}
	}
	return ""
}

func checkAnswers(code string) func() string { return func() string { return code } }

// require_carry refuses a move whose live carry would start fresh, not only
// one with no recorded conversation (#7152).
func TestTickRequireCarryRefusesAMoveTheLiveCarryWouldNotCarry(t *testing.T) {
	rows, asked := movableTick(t, true, nil, checkAnswers(handoff.CarryBacktracked))
	if asked != 1 {
		t.Fatalf("CarryCheck asked %d times, want 1", asked)
	}
	if rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("require_carry moved a session whose carry would start fresh: %+v", rows)
	}
	if !rowsHold(rows, handoff.EventIgnored, carryRefusedReason) {
		t.Fatalf("no carry-refused edge in %+v", rows)
	}
	if got := carryRefusedCode(rows); got != handoff.CarryBacktracked {
		t.Fatalf("carry-refused says carry=%q, want the check's %q", got, handoff.CarryBacktracked)
	}

	rows, asked = movableTick(t, true, nil, checkAnswers(""))
	if asked != 1 || !rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("a carry the live swap would make must still move (asked %d): %+v", asked, rows)
	}
	if rowsHold(rows, handoff.EventIgnored, carryRefusedReason) {
		t.Fatalf("a confirmed carry was refused: %+v", rows)
	}

	// A feeder that gives no full answer cannot confirm the carry.
	rows, _ = movableTick(t, true, nil, nil)
	if rowsHold(rows, handoff.EventMove, "") || !rowsHold(rows, handoff.EventIgnored, carryRefusedReason) {
		t.Fatalf("an unconfirmable carry moved under require_carry: %+v", rows)
	}
	if got := carryRefusedCode(rows); got != handoff.CarryUnconfirmed {
		t.Fatalf("no check: carry=%q, want %q", got, handoff.CarryUnconfirmed)
	}
}

// The carry-refused row says why with a code, never the check's prose: a value
// outside the vocabulary reads as unconfirmed (#7156 review P3-2).
func TestTickCarryRefusedCarriesOnlyACode(t *testing.T) {
	rows, _ := movableTick(t, true, nil, checkAnswers("/home/u/.codex/sessions is a symbolic link"))
	if got := carryRefusedCode(rows); got != handoff.CarryUnconfirmed {
		t.Fatalf("carry=%q reached the row; want %q", got, handoff.CarryUnconfirmed)
	}
	// The in-memory precondition's own code reaches the row too.
	rows, _ = movableTick(t, true, func(in *SessionInput) {
		in.CarryPossible, in.CarryRefusal = false, handoff.CarryRemoteWorkspace
	}, nil)
	if got := carryRefusedCode(rows); got != handoff.CarryRemoteWorkspace {
		t.Fatalf("precondition: carry=%q, want %q", got, handoff.CarryRemoteWorkspace)
	}
	rows, _ = movableTick(t, true, func(in *SessionInput) { in.CarryPossible = false }, nil)
	if got := carryRefusedCode(rows); got != handoff.CarryUnrecorded {
		t.Fatalf("precondition without a code: carry=%q, want %q", got, handoff.CarryUnrecorded)
	}
}

// The full answer reads the provider's store, so it is asked only when it can
// change the verdict.
func TestTickAsksTheLiveCarryOnlyWhenItCanChangeTheVerdict(t *testing.T) {
	never := func() string {
		t.Fatalf("CarryCheck asked when it could not change the verdict")
		return handoff.CarryUnconfirmed
	}
	rows, asked := movableTick(t, false, nil, never)
	if asked != 0 || !rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("without require_carry: asked %d, rows %+v", asked, rows)
	}
	// No trigger: the session is healthy and idle below the threshold.
	_, asked = movableTick(t, true, func(in *SessionInput) {
		in.Parked = false
		in.Content = "› Ask Codex to do anything\n"
		in.HomeDir = "/home/codex/cool"
	}, never)
	if asked != 0 {
		t.Fatalf("asked %d times for a session with no trigger", asked)
	}
	// The in-memory precondition already refuses.
	rows, asked = movableTick(t, true, func(in *SessionInput) { in.CarryPossible = false }, never)
	if asked != 0 || !rowsHold(rows, handoff.EventIgnored, carryRefusedReason) {
		t.Fatalf("precondition false: asked %d, rows %+v", asked, rows)
	}
}

// targetCheck records the targets require_carry's answer against a target
// was asked for, and answers code.
func targetCheck(code string, asked *[]string) func(*SessionInput) {
	return func(in *SessionInput) {
		in.CarryCheckTo = func(target string) string {
			*asked = append(*asked, target)
			return code
		}
	}
}

// moveTarget is the To of the move row, or "".
func moveTarget(rows []handoff.EventRow) string {
	for _, row := range rows {
		if row.Kind == handoff.EventMove {
			return row.To
		}
	}
	return ""
}

// The check-ahead names no target, so a refusal only the target's home can
// show — a copy of the conversation filed elsewhere there, a thread/revert
// rollout, a copy the carry would refuse — reached the move as a fresh start.
// require_carry now asks the live plan against the account Decide chose, and a
// refusal there defers the move with the same carry-refused edge (#7273).
func TestTickRequireCarryRefusesAMoveTheTargetCarryWouldNotCarry(t *testing.T) {
	var confirmed []string
	rows, _ := movableTick(t, true, targetCheck("", &confirmed), checkAnswers(""))
	to := moveTarget(rows)
	if to == "" {
		t.Fatalf("a carry both sides confirm must move: %+v", rows)
	}
	if len(confirmed) != 1 || confirmed[0] != to {
		t.Fatalf("asked the target check for %v, want exactly the move's target %q", confirmed, to)
	}

	var refused []string
	rows, asked := movableTick(t, true, targetCheck(handoff.CarryDuplicated, &refused), checkAnswers(""))
	if rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("require_carry moved a session whose carry into the target would start fresh: %+v", rows)
	}
	if got := carryRefusedCode(rows); got != handoff.CarryDuplicated {
		t.Fatalf("carry-refused says carry=%q, want the target check's %q (rows %+v)", got, handoff.CarryDuplicated, rows)
	}
	if asked != 1 {
		t.Fatalf("source check asked %d times, want 1", asked)
	}
	// Refuse outright: the next candidate is not tried, so a refusal costs
	// one target's store read, not one per candidate.
	if len(refused) != 1 || refused[0] != to {
		t.Fatalf("asked the target check for %v, want only the chosen target %q", refused, to)
	}
}

// A feeder with no answer against a target cannot confirm the carry; the
// prose of a refusal never reaches the row.
func TestTickRequireCarryWithoutATargetAnswerIsUnconfirmed(t *testing.T) {
	rows, _ := movableTick(t, true, func(in *SessionInput) { in.CarryCheckTo = nil }, checkAnswers(""))
	if rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("an unconfirmable target carry moved under require_carry: %+v", rows)
	}
	if got := carryRefusedCode(rows); got != handoff.CarryUnconfirmed {
		t.Fatalf("no target check: carry=%q, want %q", got, handoff.CarryUnconfirmed)
	}
	var asked []string
	rows, _ = movableTick(t, true, targetCheck("the new account already holds /a/path", &asked), checkAnswers(""))
	if got := carryRefusedCode(rows); got != handoff.CarryUnconfirmed {
		t.Fatalf("carry=%q reached the row; want %q", got, handoff.CarryUnconfirmed)
	}
}

// The target answer reads the target's store, so it is asked only when it can
// change the verdict: under require_carry, for a move, once the source side
// has confirmed.
func TestTickAsksTheTargetCarryOnlyWhenItCanChangeTheVerdict(t *testing.T) {
	var asked []string
	rows, _ := movableTick(t, false, targetCheck(handoff.CarryDuplicated, &asked), checkAnswers(""))
	if len(asked) != 0 || !rowsHold(rows, handoff.EventMove, "") {
		t.Fatalf("without require_carry: asked %v, rows %+v", asked, rows)
	}
	rows, _ = movableTick(t, true, targetCheck("", &asked), checkAnswers(handoff.CarryBacktracked))
	if len(asked) != 0 || carryRefusedCode(rows) != handoff.CarryBacktracked {
		t.Fatalf("a source refusal stands without asking the target: asked %v, rows %+v", asked, rows)
	}
	_, _ = movableTick(t, true, func(in *SessionInput) {
		targetCheck("", &asked)(in)
		in.Parked = false
		in.Content = "› Ask Codex to do anything\n"
		in.HomeDir = "/home/codex/cool"
	}, checkAnswers(""))
	if len(asked) != 0 {
		t.Fatalf("asked the target check for %v with no trigger", asked)
	}
}
