package handoff

// decide_windowless_test.go pins the fix for "emptiest ordering prefers
// windowless Observed readings over real usage evidence": under
// target_order = "emptiest", the orderCandidates percent tiebreak must not
// treat a zero-window Observed reading as 0% used. worstWindow returns
// ok=false for a windowless reading ("no window is not a zero reading", per
// quota/rate_limits.go's contract), and the comparator must honor that flag:
// a windowless reading is keyed to the batch's highest reported percent, so
// it ranks with the worst reported candidate — never above it as a phantom
// 0% — and otherwise falls through to occupancy/listed order. Keying every
// candidate once (rather than skipping the percent comparison pairwise when
// either side is windowless) keeps the comparator transitive, mirroring the
// `reported` guard in tier() and readingBelowAllFresh's len(r.Windows)==0
// rejection (#7371 review, #7322/#7314).

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// windowlessObservedReading is the production shape parsed from a rate_limits
// snapshot whose primary/secondary slots are all null — StatusObserved with
// zero windows and Complete=false (the completeness loop in
// daemon/account_handoff.go readingFactsFrom unsets the initially-true flag
// when it ranges over no windows). It is "evidence of absence of data, not
// evidence of a healthy account" (quota/rate_limits.go) and must never read as
// a 0% usage reading in the emptiest comparator.
func windowlessObservedReading() ReadingFacts {
	return ReadingFacts{
		Status:     ReadingObserved,
		RecordedAt: testNow.Add(-time.Minute),
		Complete:   false,
		Windows:    nil,
	}
}

// TestOrderCandidatesWindowlessObservedDoesNotWinAsZeroPercent pins the
// orderCandidates comparator fix. Before the fix, when both candidates were
// ReadingObserved the comparator discarded worstWindow's ok flag, so a
// windowless reading read as UsedPercent 0 and won the percent tiebreak over
// any candidate with a real reported window. The fix honors ok: a windowless
// reading is keyed to the batch's highest reported percent, so it ties the
// worst reported candidate and otherwise falls through to occupancy and
// listed order — never beating a reported candidate on percent. All cases
// below share tier 3 (an incomplete below-prefer observed reading and a
// windowless observed reading both land there alongside ReadingNone, per
// #7314/#7322).
func TestOrderCandidatesWindowlessObservedDoesNotWinAsZeroPercent(t *testing.T) {
	cfg := shadowCfg()
	cfg.TargetOrder = config.AccountHandoffTargetOrderEmptiest
	order := func(accounts ...AccountFacts) []string {
		eligible := make([]scoredAccount, len(accounts))
		for i, a := range accounts {
			eligible[i] = scoredAccount{facts: a, index: i}
		}
		return orderCandidates(eligible, cfg)
	}
	want := func(t *testing.T, got []string, names ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Fatalf("order = %v, want %v", got, names)
		}
	}
	blind := func(name string, opts ...func(*AccountFacts)) AccountFacts {
		a := AccountFacts{Name: name, Registered: true, Reading: windowlessObservedReading()}
		for _, opt := range opts {
			opt(&a)
		}
		return a
	}
	withOccupancy := func(n int) func(*AccountFacts) {
		return func(a *AccountFacts) { a.Occupancy = n }
	}

	t.Run("windowless does not outrank a reported partial reading listed first", func(t *testing.T) {
		// The core repro: partial (one 10% window, tier 3) listed BEFORE
		// blind (zero windows, tier 3). Before the fix, blind read as 0%
		// and won the percent tiebreak; after the fix blind is keyed to
		// the batch's only reported percent (10%), ties partial on
		// percent, and falls through to listed order -> partial first.
		want(t, order(
			AccountFacts{Name: "partial", Registered: true, Reading: partialReading(10, future())},
			blind("blind"),
		), "partial", "blind")
	})

	t.Run("windowless does not win on phantom zero against a higher-percent reported reading", func(t *testing.T) {
		// blind is windowless and listed FIRST with occupancy 2; reported
		// is a partial 50% reading with occupancy 0. Before the fix,
		// blind's phantom 0% beat 50% on the percent tiebreak; after the
		// fix blind is keyed to the batch's only reported percent (50%),
		// ties reported on percent, and occupancy decides -> reported
		// (0) ranks ahead of blind (2) even though blind is listed first.
		got := order(
			blind("blind", withOccupancy(2)),
			AccountFacts{Name: "reported", Registered: true, Reading: partialReading(50, future()), Occupancy: 0},
		)
		want(t, got, "reported", "blind")
	})

	t.Run("percent tiebreak still orders two complete reported readings", func(t *testing.T) {
		// No regression: two complete reported readings still order by
		// worst window percent — 20% ranks below 50%.
		want(t, order(
			AccountFacts{Name: "hot", Registered: true, Reading: observedReading(50, future())},
			AccountFacts{Name: "calm", Registered: true, Reading: observedReading(20, future())},
		), "calm", "hot")
	})

	t.Run("percent tiebreak still orders two partial reported readings", func(t *testing.T) {
		// No regression: two incomplete-but-reported readings still order
		// by reported percent — 10% ranks below 40%.
		want(t, order(
			AccountFacts{Name: "p40", Registered: true, Reading: partialReading(40, future())},
			AccountFacts{Name: "p10", Registered: true, Reading: partialReading(10, future())},
		), "p10", "p40")
	})

	t.Run("two windowless readings fall through to occupancy then listed order", func(t *testing.T) {
		// Both windowless -> both keyed to the same (no-reported) value,
		// tie on percent -> occupancy breaks the tie; equal occupancy ->
		// listed order.
		want(t, order(
			blind("a"),
			blind("b"),
		), "a", "b")

		// Occupancy still breaks the tie between two windowless readings.
		got := order(
			blind("busy", withOccupancy(3)),
			blind("idle", withOccupancy(0)),
		)
		want(t, got, "idle", "busy")
	})

	t.Run("a windowless Observed reading orders the same as a no-reading account", func(t *testing.T) {
		// With neither a reported window in the batch, maxReported is 0,
		// so a windowless Observed reading and a ReadingNone candidate
		// both key to 0 and fall through to occupancy/listed order — no
		// phantom 0% comparison either way. Pin both listings to
		// confirm there is no regression and the two shapes are peers.
		noReading := ReadingFacts{Status: ReadingNone}
		want(t, order(
			blind("blind"),
			AccountFacts{Name: "none", Registered: true, Reading: noReading},
		), "blind", "none")
		want(t, order(
			AccountFacts{Name: "none", Registered: true, Reading: noReading},
			blind("blind"),
		), "none", "blind")
	})

	t.Run("a no-reading candidate does not form a transitivity cycle with reported and windowless ones", func(t *testing.T) {
		// The comparator applies one per-candidate percentKey to every
		// same-tier candidate rather than only when both sides are
		// ReadingObserved. Without that, a no-reading candidate skipped
		// the percent branch, so reported/windowless pairs used percent
		// while pairs involving no-reading used listed order — a cycle:
		// 50% < windowless < none < 10% by listed order, but 10% < 50%
		// by percent, so sort.SliceStable could leave 50% first. After
		// the fix no-reading is keyed to maxReported (50%), tying the
		// worst reported candidate, so percent decides: 10% < 50% =
		// windowless = none, then listed order. Decide picks 10%.
		noReading := ReadingFacts{Status: ReadingNone}
		got := order(
			AccountFacts{Name: "fifty", Registered: true, Reading: partialReading(50, future())},
			blind("blind"),
			AccountFacts{Name: "none", Registered: true, Reading: noReading},
			AccountFacts{Name: "ten", Registered: true, Reading: partialReading(10, future())},
		)
		want(t, got, "ten", "fifty", "blind", "none")
	})
}

// TestDecideEmptiestWindowlessObservedDoesNotBeatReportedEvidence reproduces
// the bug end-to-end: a walled codex session under target_order="emptiest"
// with two eligible tier-3 candidates — "blind" (zero-window Observed reading,
// evidence of absence of data) and "partial" (one 10% window, real but
// incomplete evidence). Before the fix, Decide moved the session onto "blind"
// (phantom 0% beat 10%); after the fix it moves onto "partial", the candidate
// with real (if partial) usage evidence, and reports Candidates in the same
// order. The shadow event log's move row therefore names the evidence-bearing
// target, and a future live mover would land on it too.
func TestDecideEmptiestWindowlessObservedDoesNotBeatReportedEvidence(t *testing.T) {
	f := wallConfirmedFacts()
	f.Config.TargetOrder = config.AccountHandoffTargetOrderEmptiest
	// work is the parked session's current account (walled -> skipped as
	// "current"); partial and blind are the two eligible tier-3 candidates.
	// Listed order favours partial (it appears before blind in the resolved
	// limit_account_candidates list).
	f.Accounts = []AccountFacts{
		{Name: "work", Registered: true, Reading: observedReading(99, future())},
		{Name: "partial", Registered: true, Reading: partialReading(10, future())},
		{Name: "blind", Registered: true, Reading: windowlessObservedReading()},
	}

	d := Decide(f)
	wantDecision(t, d, KindMove, reasonTriggerWall)
	if d.To != "partial" {
		t.Fatalf("emptiest must move to the partial-evidence account, not the windowless one: got To=%q Candidates=%v", d.To, d.Candidates)
	}
	wantCandidates := []string{"partial", "blind"}
	if strings.Join(d.Candidates, ",") != strings.Join(wantCandidates, ",") {
		t.Fatalf("Candidates = %v, want %v", d.Candidates, wantCandidates)
	}
	wantEvent(t, d, EventMove, reasonTriggerWall)
	move := eventOf(t, d, EventMove)
	if move.To != "partial" {
		t.Fatalf("move event To = %q, want partial", move.To)
	}

	// Sanity: reversing the listed order (blind before partial) keeps the
	// fix honest — blind is keyed to the batch's only reported percent
	// (partial's 10%), ties it on percent, and listed order alone decides,
	// so blind (now listed first) heads the order. This documents that the
	// fix removes the phantom-0% comparison rather than adding a reverse
	// rule that windowless is strictly below reported evidence: a
	// windowless reading ties the worst reported candidate, never beats it.
	f.Accounts[1], f.Accounts[2] = f.Accounts[2], f.Accounts[1]
	d2 := Decide(f)
	wantDecision(t, d2, KindMove, reasonTriggerWall)
	if d2.To != "blind" {
		t.Fatalf("with blind listed first, listed order must decide: got To=%q Candidates=%v", d2.To, d2.Candidates)
	}
	if strings.Join(d2.Candidates, ",") != "blind,partial" {
		t.Fatalf("Candidates = %v, want [blind,partial]", d2.Candidates)
	}
}
