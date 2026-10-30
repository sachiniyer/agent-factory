package backlog

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestCheckStatusChange(t *testing.T) {
	all := []Status{StatusDraft, StatusQueued, StatusRunning, StatusReview, StatusDone, StatusDropped}
	type key struct {
		from, to Status
		by       Actor
	}
	allowed := map[key]bool{}
	allow := func(by Actor, to Status, froms ...Status) {
		for _, f := range froms {
			allowed[key{f, to, by}] = true
		}
	}
	allow(ActorUser, StatusQueued, StatusDraft, StatusReview)
	allow(ActorUser, StatusDraft, StatusQueued, StatusReview, StatusDone, StatusDropped)
	allow(ActorAF, StatusRunning, StatusDraft, StatusQueued, StatusReview, StatusRunning)
	for _, by := range []Actor{ActorUser, ActorAgent, ActorAF} {
		allow(by, StatusReview, StatusRunning)
	}
	allow(ActorAgent, StatusReview, StatusReview)
	allow(ActorUser, StatusDone, StatusDraft, StatusQueued, StatusRunning, StatusReview)
	allow(ActorUser, StatusDropped, StatusDraft, StatusQueued, StatusRunning, StatusReview)

	for _, by := range []Actor{ActorUser, ActorAgent, ActorAF} {
		for _, from := range all {
			for _, to := range all {
				err := CheckStatusChange(StatusChange{ID: 4, From: from, To: to, By: by})
				if want := allowed[key{from, to, by}]; (err == nil) != want {
					t.Errorf("%s: %s -> %s: err = %v, want allowed=%v", by, from, to, err, want)
				}
			}
		}
	}
}

func TestCheckStatusChangeCases(t *testing.T) {
	cases := []struct {
		name string
		c    StatusChange
		want string
	}{
		{"start with live run", StatusChange{ID: 3, From: StatusRunning, To: StatusRunning, By: ActorAF, RunActive: true}, "B-3 is already working"},
		{"user cannot start", StatusChange{ID: 3, From: StatusQueued, To: StatusRunning, By: ActorUser}, `use "af backlog start 3"`},
		{"same status", StatusChange{ID: 3, From: StatusQueued, To: StatusQueued, By: ActorUser}, "B-3 is already queued"},
		{"queue a done item", StatusChange{ID: 3, From: StatusDone, To: StatusQueued, By: ActorUser}, `reopen it first with "af backlog reopen 3"`},
		{"review from draft", StatusChange{ID: 3, From: StatusDraft, To: StatusReview, By: ActorUser}, "only a running item goes to review"},
		{"agent cannot queue", StatusChange{ID: 3, From: StatusDraft, To: StatusQueued, By: ActorAgent}, "an agent cannot set it to queued"},
		{"unknown", StatusChange{ID: 3, From: StatusDraft, To: "bogus", By: ActorUser}, `unknown status "bogus"`},
		{"failed start by user", StatusChange{ID: 3, From: StatusRunning, To: StatusQueued, By: ActorUser, StartFailed: true, PrevStatus: StatusQueued}, "cannot set"},
		{"failed start wrong prev", StatusChange{ID: 3, From: StatusRunning, To: StatusDraft, By: ActorAF, StartFailed: true, PrevStatus: StatusQueued}, "cannot set"},
		{"failed start not running", StatusChange{ID: 3, From: StatusQueued, To: StatusQueued, By: ActorAF, StartFailed: true, PrevStatus: StatusQueued}, "already queued"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckStatusChange(c.c)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	for _, prev := range []Status{StatusDraft, StatusQueued, StatusReview} {
		if err := CheckStatusChange(StatusChange{ID: 3, From: StatusRunning, To: prev, By: ActorAF, StartFailed: true, PrevStatus: prev}); err != nil {
			t.Errorf("revert to %s: %v", prev, err)
		}
	}
}

func TestSetStatusLeaf(t *testing.T) {
	b := mk(t, Item{ID: 1, Status: StatusRunning, Thread: []Message{
		{ID: 1, Kind: KindQuestion, Text: "a?"},
		{ID: 2, Kind: KindQuestion, Text: "b?"},
		{ID: 3, Kind: KindAnswer, Answers: 1},
	}})
	changed, err := b.SetStatus(1, StatusDone, ActorUser, nil, t0)
	if err != nil || !reflect.DeepEqual(changed, []int{1}) {
		t.Fatal(changed, err)
	}
	it := b.Item(1)
	if it.Status != StatusDone || it.Thread[0].Closed != "" || it.Thread[1].Closed != ClosedDone || it.OpenQuestion() != nil {
		t.Fatalf("%+v", it)
	}

	b = mk(t, Item{ID: 1, Status: StatusQueued, Thread: []Message{{ID: 1, Kind: KindQuestion}}})
	if _, err := b.SetStatus(1, StatusDropped, ActorUser, nil, t0); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Thread[0].Closed != ClosedDropped {
		t.Fatal(b.Item(1).Thread)
	}

	b = mk(t, Item{ID: 1, Status: StatusRunning})
	active := func(*Item) bool { return true }
	if _, err := b.SetStatus(1, StatusRunning, ActorAF, active, t0); err == nil {
		t.Fatal("started an item with an active run")
	}
	if _, err := b.SetStatus(9, StatusDone, ActorUser, nil, t0); err == nil || !strings.Contains(err.Error(), "B-9 not found") {
		t.Fatal(err)
	}
}

func TestSetStatusParent(t *testing.T) {
	build := func(t *testing.T) *Backlog {
		return mk(t,
			Item{ID: 1},
			Item{ID: 2, Parent: 1, Status: StatusDraft},
			Item{ID: 3, Parent: 1, Status: StatusQueued},
			Item{ID: 4, Parent: 1},
			Item{ID: 5, Parent: 4, Status: StatusDraft},
			Item{ID: 6, Parent: 4, Status: StatusDone},
			Item{ID: 7, Parent: 1, Status: StatusRunning},
		)
	}
	b := build(t)
	changed, err := b.SetStatus(1, StatusQueued, ActorUser, nil, t0)
	if err != nil || !reflect.DeepEqual(changed, []int{2, 5}) {
		t.Fatal(changed, err)
	}
	if b.Item(1).Status != StatusDraft || b.Item(4).Status != StatusDraft {
		t.Fatal("parent status changed")
	}

	b = build(t)
	changed, err = b.SetStatus(1, StatusDropped, ActorUser, nil, t0)
	if err != nil || !reflect.DeepEqual(changed, []int{2, 3, 5, 7}) {
		t.Fatal(changed, err)
	}
	if b.Item(6).Status != StatusDone {
		t.Fatal("done child dropped")
	}

	cases := []struct {
		to   Status
		by   Actor
		want string
	}{
		{StatusRunning, ActorAF, "B-1 is a parent; parents run through their children — start a child"},
		{StatusDone, ActorUser, "only queue, drop and reopen apply"},
		{StatusReview, ActorUser, "only queue, drop and reopen apply"},
		{StatusQueued, ActorAgent, "an agent cannot"},
	}
	for _, c := range cases {
		b := build(t)
		if _, err := b.SetStatus(1, c.to, c.by, nil, t0); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s by %s: err = %v", c.to, c.by, err)
		}
	}

	b = build(t)
	if _, err := b.SetStatus(1, StatusDropped, ActorUser, nil, t0); err != nil {
		t.Fatal(err)
	}
	changed, err = b.SetStatus(1, StatusDraft, ActorUser, nil, t0)
	if err != nil || !reflect.DeepEqual(changed, []int{2, 3, 5, 6, 7}) {
		t.Fatal(changed, err)
	}
	for _, id := range changed {
		if b.Item(id).Status != StatusDraft {
			t.Fatalf("B-%d is %s after reopen", id, b.Item(id).Status)
		}
	}
	if _, err := b.SetStatus(1, StatusDraft, ActorUser, nil, t0); err == nil || !strings.Contains(err.Error(), "no children to reopen") {
		t.Fatal(err)
	}

	b = mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1, Status: StatusQueued})
	if _, err := b.SetStatus(1, StatusQueued, ActorUser, nil, t0); err == nil || !strings.Contains(err.Error(), "no children to queue") {
		t.Fatal(err)
	}
}

// Dropping a parent closes the open questions on the parent and on every
// intermediate parent, not only on the leaves (Codex P2 on #6286): otherwise the
// derived state stays needs_you and a retry fails with "no children to drop".
func TestDropAParentClosesItsOwnAndIntermediateQuestions(t *testing.T) {
	q := []Message{{ID: 1, Kind: KindQuestion, Text: "?"}}
	b := mk(t,
		Item{ID: 1, Thread: q},
		Item{ID: 2, Parent: 1, Thread: q},
		Item{ID: 3, Parent: 2},
	)
	changed, err := b.SetStatus(1, StatusDropped, ActorUser, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{1, 2} {
		if b.Item(id).OpenQuestion() != nil {
			t.Fatalf("%s still has an open question", FormatID(id))
		}
		if got := b.Item(id).Thread[0].Closed; got != ClosedDropped {
			t.Fatalf("%s question closed as %q", FormatID(id), got)
		}
	}
	sort.Ints(changed)
	if !reflect.DeepEqual(changed, []int{1, 2, 3}) {
		t.Fatalf("changed = %v, want the leaf and the two parents whose questions closed", changed)
	}
	if st := b.Derive(1, nil).State; st != StateDropped {
		t.Fatalf("parent derives %q, want dropped", st)
	}
}
