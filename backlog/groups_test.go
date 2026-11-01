package backlog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// group is B-1 > B-2 > B-3 (queued), with B-4 a queued top-level leaf.
func group(t *testing.T) *Backlog {
	return mk(t,
		Item{ID: 1},
		Item{ID: 2, Parent: 1},
		Item{ID: 3, Parent: 2, Status: StatusQueued},
		Item{ID: 4, Status: StatusQueued},
	)
}

func TestPauseSkipsSubtree(t *testing.T) {
	b := group(t)
	if _, err := b.Edit(1, Edit{Paused: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	states := b.DeriveAll(nil)
	if got, want := states[3], (Derived{State: StateQueued, Reason: "Queued · paused (B-1)"}); got != want {
		t.Fatalf("B-3 = %+v, want %+v", got, want)
	}
	if got := b.Runnable(states, 5, nil); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("Runnable = %v, want [4]", got)
	}

	if _, err := b.Edit(2, Edit{Paused: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	if got := b.Derive(3, nil).Reason; got != "Queued · paused (B-2)" {
		t.Fatalf("nearest paused ancestor: reason %q", got)
	}

	for _, id := range []int{1, 2} {
		if _, err := b.Edit(id, Edit{Paused: ptr(false)}, t0); err != nil {
			t.Fatal(err)
		}
	}
	states = b.DeriveAll(nil)
	if got := b.Runnable(states, 5, nil); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Fatalf("after unpause Runnable = %v", got)
	}
}

func TestPauseWaitingKeepsWaitReason(t *testing.T) {
	b := mk(t, Item{ID: 1, Paused: true}, Item{ID: 2, Parent: 1, Status: StatusQueued, WaitsOn: []int{3}}, Item{ID: 3})
	if got := b.Derive(2, nil); got.State != StateWaiting {
		t.Fatalf("B-2 = %+v, want waiting", got)
	}
}

func TestPausePlanRunner(t *testing.T) {
	b := group(t)
	b.Settings.Runner = true
	b.Item(1).Paused = true
	b.Item(4).Status = StatusDraft
	plan := b.PlanRunner(RunnerInput{States: b.DeriveAll(nil), Limit: -1})
	if len(plan.Start) != 0 || plan.View.State != RunnerIdle {
		t.Fatalf("plan = %+v, want idle and no starts", plan)
	}
}

func TestPauseErrors(t *testing.T) {
	b := group(t)
	_, err := b.Edit(3, Edit{Paused: ptr(true)}, t0)
	if err == nil || err.Error() != "B-3 is not a parent; only parents can be paused" {
		t.Fatalf("pause leaf: %v", err)
	}
	if b.Item(3).Paused {
		t.Fatal("failed edit changed the item")
	}
}

func TestArchive(t *testing.T) {
	b := group(t)
	if _, err := b.Edit(1, Edit{Archived: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	if g := b.Item(1); !g.Archived || !g.Paused {
		t.Fatalf("archive: %+v, want archived and paused", g)
	}
	if b.ArchivedBy(3) != 1 || b.ArchivedBy(4) != 0 {
		t.Fatalf("ArchivedBy: B-3 %d, B-4 %d", b.ArchivedBy(3), b.ArchivedBy(4))
	}
	_, err := b.Edit(1, Edit{Paused: ptr(false)}, t0)
	if err == nil || !strings.Contains(err.Error(), "B-1 is archived; unarchive it first") {
		t.Fatalf("unpause archived: %v", err)
	}
	if err := b.Move(1, Move{Into: 4}, t0); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("move archived into parent: %v", err)
	}
	if _, err := b.Edit(1, Edit{Archived: ptr(false)}, t0); err != nil {
		t.Fatal(err)
	}
	if g := b.Item(1); g.Archived || !g.Paused {
		t.Fatalf("unarchive: %+v, want paused and not archived", g)
	}
}

func TestArchiveRefusals(t *testing.T) {
	cases := []struct {
		name string
		id   int
		edit func(b *Backlog)
		want string
	}{
		{"leaf", 4, nil, "B-4 is not a top-level parent; only groups can be archived"},
		{"nested parent", 2, nil, "B-2 is not a top-level parent; only groups can be archived"},
		{"running item", 1, func(b *Backlog) { b.Item(3).Status = StatusRunning },
			"B-1 has a running item (B-3); stop it before you archive B-1"},
		{"open question", 1, func(b *Backlog) {
			b.Item(3).Thread = []Message{{ID: 1, Kind: KindQuestion, Text: "?"}}
		}, "B-1 has an open question on B-3; answer it before you archive B-1"},
		{"answer on its way", 1, func(b *Backlog) {
			b.Item(3).Thread = []Message{{ID: 1, Kind: KindQuestion, Text: "?"},
				{ID: 2, Kind: KindAnswer, Answers: 1, Text: "!", Delivery: DeliveryPending}}
		}, "B-1 has an answer on its way to B-3; wait for it before you archive B-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := group(t)
			if tc.edit != nil {
				tc.edit(b)
			}
			_, err := b.Edit(tc.id, Edit{Archived: ptr(true)}, t0)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if b.Item(tc.id).Archived || b.Item(tc.id).Paused {
				t.Fatal("failed edit changed the item")
			}
		})
	}
}

func TestArchivedGroupRefusesMoveAndAdd(t *testing.T) {
	b := group(t)
	if _, err := b.Edit(1, Edit{Archived: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	const want = "B-1 is archived; unarchive it first"
	if _, err := b.Add(NewItem{Title: "new", Parent: 2}, t0); err == nil || err.Error() != want {
		t.Fatalf("add into archived: %v", err)
	}
	for _, mv := range []Move{{Into: 2}, {Before: 3}, {After: 2}} {
		if err := b.Move(4, mv, t0); err == nil || err.Error() != want {
			t.Fatalf("move %+v into archived: %v", mv, err)
		}
	}
	// A batch places its items through the same move, so it is refused too.
	if _, err := b.AddBatch(Batch{Items: []ItemInput{{Title: "batched", Parent: Ref{ID: 2}}}}, t0); err == nil || err.Error() != want {
		t.Fatalf("batch add into archived: %v", err)
	}
	if b.Item(4).Parent != 0 || len(b.Items) != 4 {
		t.Fatal("refused move or add changed the backlog")
	}
	if err := b.Move(3, Move{Top: true}, t0); err != nil {
		t.Fatalf("reorder inside archived group: %v", err)
	}
}

func TestUnpauseLeafRefused(t *testing.T) {
	b := group(t)
	_, err := b.Edit(3, Edit{Paused: ptr(false)}, t0)
	if err == nil || err.Error() != "B-3 is not a parent; only parents can be paused" {
		t.Fatalf("unpause leaf: %v", err)
	}
}

func TestRunNowSkipsPaused(t *testing.T) {
	b := group(t)
	b.Item(1).Paused = true
	if pick, _ := b.RunNow(b.DeriveAll(nil), 0, 5); !reflect.DeepEqual(pick, []int{4}) {
		t.Fatalf("RunNow = %v, want [4]", pick)
	}
	b.Item(4).Status = StatusDraft
	if pick, reason := b.RunNow(b.DeriveAll(nil), 0, 5); len(pick) != 0 || !strings.HasPrefix(reason, "nothing queued") {
		t.Fatalf("RunNow = %v %q, want nothing", pick, reason)
	}
}

// The admission check for an automatic start runs after the pass chose the
// item, so a group paused in between must stop it there too.
func TestRunnerEligibleRefusesAnItemPausedAfterThePassChoseIt(t *testing.T) {
	b := group(t)
	var leaf int
	for _, it := range b.Items {
		if it.Parent != 0 && !b.IsParent(it.ID) {
			leaf = it.ID
			break
		}
	}
	if leaf == 0 {
		t.Fatal("fixture has no leaf under a parent")
	}
	b.Item(leaf).Status = StatusQueued
	states := b.DeriveAll(nil)
	if !b.RunnerEligible(leaf, states) {
		t.Fatalf("queued leaf %s not eligible before the pause: %+v", FormatID(leaf), states[leaf])
	}
	if _, err := b.Edit(b.Item(leaf).Parent, Edit{Paused: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	if b.RunnerEligible(leaf, b.DeriveAll(nil)) {
		t.Fatalf("%s is eligible under a paused parent", FormatID(leaf))
	}
}

// The pause can also land after the admission check and before the start is
// recorded, so the transaction that records it refuses a runner start, and a
// start an "af backlog run" pass chose, too (Codex P2s on #6468). A start by
// hand still goes through.
func TestBeginStartRefusesARunnerStartUnderAPausedParent(t *testing.T) {
	// Unpaused, a pass start is recorded as the user's, as before.
	if p, err := group(t).BeginStart(3, StartedByRunPass, nil, t0); err != nil || p.Run.StartedBy != StartedByUser {
		t.Fatalf("an unpaused pass start: %+v %v", p, err)
	}
	b := group(t)
	if _, err := b.Edit(2, Edit{Paused: ptr(true)}, t0); err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{StartedByRunner, StartedByRunPass} {
		if _, err := b.BeginStart(3, by, nil, t0); !errors.Is(err, ErrNotRunnable) {
			t.Fatalf("%s start under a paused parent: %v", by, err)
		}
	}
	if it := b.Item(3); it.Status != StatusQueued || len(it.Runs) != 0 {
		t.Fatalf("the refused start changed the item: %+v", it)
	}
	if _, err := b.BeginStart(3, StartedByUser, nil, t0); err != nil {
		t.Fatalf("a start by hand under a paused parent: %v", err)
	}
}
