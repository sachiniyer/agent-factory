package backlog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNewParentBecomesDraft(t *testing.T) {
	for _, st := range []Status{StatusQueued, StatusReview, StatusDone, StatusDropped} {
		b := mk(t, Item{ID: 1, Status: st}, Item{ID: 2})
		if err := b.Move(2, Move{Into: 1}, t0); err != nil {
			t.Fatal(err)
		}
		if b.Item(1).Status != StatusDraft || !b.Item(1).UpdatedAt.Equal(t0) {
			t.Fatalf("%s parent kept status %s", st, b.Item(1).Status)
		}
		// When the child leaves, the old parent does not become runnable.
		if err := b.Move(2, Move{After: 1}, t0); err != nil {
			t.Fatal(err)
		}
		if b.Item(1).Status != StatusDraft {
			t.Fatal(b.Item(1).Status)
		}
	}

	b := mk(t, Item{ID: 1, Status: StatusQueued})
	if _, err := b.Add(NewItem{Title: "c", Parent: 1}, t0); err != nil || b.Item(1).Status != StatusDraft {
		t.Fatal(err, b.Item(1).Status)
	}
	b = mk(t, Item{ID: 1, Status: StatusQueued})
	if _, err := b.AddBatch(Batch{Items: []ItemInput{{Title: "c", Parent: Ref{ID: 1}}}}, t0); err != nil || b.Item(1).Status != StatusDraft {
		t.Fatal(err, b.Item(1).Status)
	}
	b = mk(t, Item{ID: 1, Status: StatusQueued}, Item{ID: 2, Parent: 1})
	if b.Item(1).Status != StatusDraft {
		t.Fatal("normalize kept a queued parent")
	}
}

func TestNormalizeCutsWaitCycles(t *testing.T) {
	b := &Backlog{Items: []Item{
		{ID: 1, Status: StatusQueued, WaitsOn: []int{2}},
		{ID: 2, Status: StatusQueued, WaitsOn: []int{1}},
		{ID: 3, Status: StatusQueued, WaitsOn: []int{4}},
		{ID: 4, Status: StatusQueued, WaitsOn: []int{5}},
		{ID: 5, Status: StatusQueued, WaitsOn: []int{3, 1}},
		{ID: 6, Status: StatusDraft},
	}}
	if err := b.normalize(); err != nil {
		t.Fatal(err)
	}
	if c := b.findCycle(); c != nil {
		t.Fatalf("cycle left: %v", c)
	}
	links := 0
	for _, it := range b.Items {
		links += len(it.WaitsOn)
	}
	if links != 4 {
		t.Fatalf("dropped %d links, want one per cycle", 6-links)
	}
	if _, err := b.Add(NewItem{Title: "unrelated"}, t0); err != nil {
		t.Fatalf("write after a hand-edited cycle: %v", err)
	}
}

func TestNormalizeParentLoopKeepsOutsideParent(t *testing.T) {
	// 7 points into the loop 1 -> 2 -> 1 and is listed first.
	b := &Backlog{Items: []Item{
		{ID: 7, Parent: 1, Status: StatusDraft},
		{ID: 1, Parent: 2, Status: StatusDraft},
		{ID: 2, Parent: 1, Status: StatusDraft},
	}}
	if err := b.normalize(); err != nil {
		t.Fatal(err)
	}
	if b.Item(7).Parent != 1 {
		t.Fatalf("B-7 lost its parent: %d", b.Item(7).Parent)
	}
	cut := 0
	for _, id := range []int{1, 2} {
		if b.Item(id).Parent == 0 {
			cut++
		}
	}
	if cut != 1 {
		t.Fatalf("cut %d loop edges, want 1", cut)
	}
	if b.parentLoops() {
		t.Fatal("loop left")
	}
}

func (b *Backlog) parentLoops() bool {
	for _, it := range b.Items {
		seen := map[int]bool{}
		for cur := b.Item(it.ID); cur != nil && cur.Parent != 0; cur = b.Item(cur.Parent) {
			if seen[cur.ID] {
				return true
			}
			seen[cur.ID] = true
		}
	}
	return false
}

func TestCheckWaitsOnMessageWording(t *testing.T) {
	b := mk(t, Item{ID: 1, WaitsOn: []int{2}}, Item{ID: 2, WaitsOn: []int{3}}, Item{ID: 3})
	err := b.CheckWaitsOn(3, 1)
	if err == nil || err.Error() != "B-3 cannot wait on B-1: B-1 already waits on B-3 (B-3 → B-1 → B-2 → B-3)" {
		t.Fatal(err)
	}
	b = mk(t, Item{ID: 1, Sequential: true}, Item{ID: 2, Parent: 1}, Item{ID: 3, Parent: 1})
	err = b.CheckWaitsOn(2, 3)
	if err == nil || err.Error() != "B-2 cannot wait on B-3: that would make a wait cycle (B-2 → B-3 → B-2)" {
		t.Fatal(err)
	}
}

func TestBatchProposalNeedsKey(t *testing.T) {
	b := New()
	_, err := b.AddBatch(Batch{Proposal: "p", Items: []ItemInput{{Key: "a", Title: "a"}, {Title: "no key"}}}, t0)
	if err == nil || err.Error() != `item 2 in the batch needs a key, because the batch names proposal "p"` {
		t.Fatal(err)
	}
	if len(b.Items) != 0 {
		t.Fatal("changed on error")
	}
}

func TestRefNull(t *testing.T) {
	var in ItemInput
	if err := json.Unmarshal([]byte(`{"title": "x", "parent": null, "waits_on": [null, "a"]}`), &in); err != nil {
		t.Fatal(err)
	}
	if !in.Parent.IsZero() || !reflect.DeepEqual(in.WaitsOn, []Ref{{}, {Key: "a"}}) {
		t.Fatalf("%+v", in)
	}
	b := New()
	ids, err := b.AddBatch(Batch{Items: []ItemInput{{Key: "a", Title: "a"}, {Key: "b", Title: "b", WaitsOn: []Ref{{}, {Key: "a"}}}}}, t0)
	if err != nil || !reflect.DeepEqual(b.Item(ids["b"]).WaitsOn, []int{ids["a"]}) {
		t.Fatal(err)
	}
}

func TestAgentDoneAgainStaysInReview(t *testing.T) {
	b := mk(t, Item{ID: 1, Status: StatusReview})
	if _, err := b.SetStatus(1, StatusReview, ActorAgent, nil, t0); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusReview {
		t.Fatal(b.Item(1).Status)
	}
	if _, err := b.SetStatus(1, StatusReview, ActorUser, nil, t0); err == nil || !strings.Contains(err.Error(), "already review") {
		t.Fatal(err)
	}
}
