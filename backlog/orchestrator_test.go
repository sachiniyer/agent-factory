package backlog

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func orch(id string) Orchestrator {
	return Orchestrator{SessionID: id, Title: "t-" + id, Program: "claude"}
}

func TestBindOrchestratorOnlyOnGroups(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3, Parent: 2}, Item{ID: 4})
	for _, id := range []int{2, 3, 4} {
		if err := b.BindOrchestrator(id, orch("s"), t0); err == nil || !strings.Contains(err.Error(), "not a top-level parent") {
			t.Errorf("%s: %v", FormatID(id), err)
		}
	}
	if err := b.BindOrchestrator(9, orch("s"), t0); err == nil {
		t.Error("bound a missing item")
	}
	if err := b.BindOrchestrator(1, orch(" "), t0); err == nil {
		t.Error("bound an empty session id")
	}
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	o := b.Item(1).Orchestrator
	if o == nil || o.SessionID != "s" || o.Title != "t-s" || !o.BoundAt.Equal(t0) || !b.Item(1).UpdatedAt.Equal(t0) {
		t.Fatalf("%+v", o)
	}
	if b.OrchestratedBy("s") != 1 || b.OrchestratedBy("x") != 0 {
		t.Fatal(b.OrchestratedBy("s"))
	}
}

func TestBindOrchestratorOneGroupPerSession(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3}, Item{ID: 4, Parent: 3})
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	err := b.BindOrchestrator(3, orch("s"), t0)
	if err == nil || err.Error() != `session "s" already orchestrates B-1; clear it first` {
		t.Fatal(err)
	}
	if b.Item(3).Orchestrator != nil {
		t.Fatal("refused bind changed the item")
	}
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatalf("rebind same group: %v", err)
	}
	if _, err := b.ClearOrchestrator(1, t0); err != nil {
		t.Fatal(err)
	}
	if err := b.BindOrchestrator(3, orch("s"), t0); err != nil {
		t.Fatalf("bind after clear: %v", err)
	}
}

func TestBindOrchestratorReplacementKeepsOutbox(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1})
	if err := b.BindOrchestrator(1, orch("old"), t0); err != nil {
		t.Fatal(err)
	}
	g := b.Item(1)
	g.Orchestrator.Handoff = "notes"
	g.Orchestrator.Meter = map[string]Meter{"x": {Cost: 1}}
	g.Orchestrator.Events = []OrchEvent{{Kind: EventQuestion, Item: 2, Message: 1, At: t0}}
	later := t0.Add(1)
	if err := b.BindOrchestrator(1, orch("new"), later); err != nil {
		t.Fatal(err)
	}
	o := b.Item(1).Orchestrator
	if o.SessionID != "new" || o.Handoff != "notes" || o.Meter["x"].Cost != 1 || len(o.Events) != 1 || !o.BoundAt.Equal(later) {
		t.Fatalf("%+v", o)
	}
	if b.OrchestratedBy("old") != 0 {
		t.Fatal("old session still bound")
	}
}

func TestClearOrchestrator(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1})
	if prev, err := b.ClearOrchestrator(1, t0); err != nil || prev != nil {
		t.Fatal(prev, err)
	}
	if _, err := b.ClearOrchestrator(9, t0); err == nil {
		t.Fatal("cleared a missing item")
	}
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	prev, err := b.ClearOrchestrator(1, t0)
	if err != nil || prev == nil || prev.SessionID != "s" || b.Item(1).Orchestrator != nil {
		t.Fatal(prev, err)
	}
}

func TestOrchestratedGroupStaysGroup(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3}, Item{ID: 4, Parent: 3})
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	if err := b.Move(1, Move{Into: 3}, t0); err == nil || !strings.Contains(err.Error(), "has an orchestrator") {
		t.Fatalf("moved into a parent: %v", err)
	}
	if err := b.Move(2, Move{Into: 3}, t0); err == nil {
		t.Fatal("moved the last child out")
	}
	if _, err := b.Delete(2); err == nil || !strings.Contains(err.Error(), "last item in B-1") {
		t.Fatalf("deleted the last child: %v", err)
	}
	if b.Item(1).Orchestrator == nil || b.Item(2) == nil {
		t.Fatal("refused change applied")
	}
	if err := b.Move(1, Move{After: 3}, t0); err != nil {
		t.Fatalf("top-level move: %v", err)
	}
	if _, err := b.Add(NewItem{Title: "more", Parent: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Delete(2); err != nil {
		t.Fatalf("delete with a sibling left: %v", err)
	}
}

func TestNormalizeDropsBadOrchestrators(t *testing.T) {
	b := mk(t,
		Item{ID: 1, Orchestrator: &Orchestrator{SessionID: "a"}},
		Item{ID: 2, Parent: 1, Orchestrator: &Orchestrator{SessionID: "b"}},
		Item{ID: 3, Orchestrator: &Orchestrator{SessionID: "c"}},
		Item{ID: 4, Orchestrator: &Orchestrator{SessionID: "a"}},
		Item{ID: 5, Parent: 4},
		Item{ID: 6, Orchestrator: &Orchestrator{}},
		Item{ID: 7, Parent: 6},
	)
	if b.Item(1).Orchestrator == nil {
		t.Fatal("dropped a good binding")
	}
	for _, id := range []int{2, 3, 4, 6} {
		if b.Item(id).Orchestrator != nil {
			t.Errorf("%s kept its binding", FormatID(id))
		}
	}
}

func TestCloneCopiesOrchestrator(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1})
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	o := b.Item(1).Orchestrator
	o.Meter = map[string]Meter{"x": {Cost: 1}}
	o.Events = []OrchEvent{{Kind: EventReview, Item: 2}}
	c := b.clone()
	co := c.Item(1).Orchestrator
	co.SessionID = "other"
	co.Meter["x"] = Meter{Cost: 2}
	co.Events[0].Kind = EventStalled
	if o.SessionID != "s" || o.Meter["x"].Cost != 1 || o.Events[0].Kind != EventReview {
		t.Fatalf("clone shares state: %+v", o)
	}
}

// TestUnboundBacklogStaysV2: the stamp follows the data. A file with no
// orchestrator loads and saves as a plain version 2 file, which an af that
// knows only version 2 reads and writes back without losing anything, and a
// read never rewrites it.
func TestUnboundBacklogStaysV2(t *testing.T) {
	home(t)
	path, _ := BacklogPath(repoID)
	writeRaw(t, path, `{"schema_version": 2, "next_id": 3, "items": [
		{"id": 1, "title": "group", "status": "draft", "paused": true},
		{"id": 2, "title": "child", "status": "queued", "parent": 1}]}`)
	before, _ := os.ReadFile(path)
	b, err := Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != 2 || !b.Item(1).Paused || b.Item(1).Orchestrator != nil || b.Item(2).Status != StatusQueued {
		t.Fatalf("%+v", b)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a read rewrote the file: %s", after)
	}
	if _, err := Update(repoID, func(b *Backlog) error {
		b.Item(2).Title = "child, renamed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["schema_version"] != float64(2) || strings.Contains(string(raw), `"orchestrator"`) {
		t.Fatalf("an unbound save must stay a plain version 2 file: %s", raw)
	}
}

// TestBoundOrchestratorRoundTrips: a binding survives a save and load, and
// writes none of its empty parts.
func TestBoundOrchestratorRoundTrips(t *testing.T) {
	home(t)
	if _, err := Update(repoID, func(b *Backlog) error {
		if _, err := b.Add(NewItem{Title: "group"}, t0); err != nil {
			return err
		}
		if _, err := b.Add(NewItem{Title: "child", Parent: 1}, t0); err != nil {
			return err
		}
		return b.BindOrchestrator(1, orch("s"), t0)
	}); err != nil {
		t.Fatal(err)
	}
	path, _ := BacklogPath(repoID)
	raw, _ := os.ReadFile(path)
	b, err := Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if o := b.Item(1).Orchestrator; o == nil || o.SessionID != "s" || b.Item(2).Orchestrator != nil || b.SchemaVersion != 3 {
		t.Fatalf("%s", raw)
	}
	if !strings.Contains(string(raw), `"schema_version": 3`) {
		t.Fatalf("a bound save must write version 3, so an older af refuses it rather than drop the binding: %s", raw)
	}
	if strings.Contains(string(raw), `"handoff"`) || strings.Contains(string(raw), `"events"`) || strings.Contains(string(raw), `"meter"`) {
		t.Fatalf("empty fields written: %s", raw)
	}
}

func TestBatchReimportMovesLastChildAndAddsOne(t *testing.T) {
	b := New()
	if _, err := b.AddBatch(Batch{Proposal: "p", Items: []ItemInput{
		{Key: "g", Title: "group"},
		{Key: "a", Title: "a", Parent: Ref{Key: "g"}},
		{Key: "h", Title: "other"},
		{Key: "h1", Title: "h1", Parent: Ref{Key: "h"}},
	}}, t0); err != nil {
		t.Fatal(err)
	}
	g, a, h := b.bySource("p", "g"), b.bySource("p", "a"), b.bySource("p", "h")
	if err := b.BindOrchestrator(g, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	ids, err := b.AddBatch(Batch{Proposal: "p", Items: []ItemInput{
		{Key: "h", Title: "other"},
		{Key: "h1", Title: "h1", Parent: Ref{Key: "h"}},
		{Key: "g", Title: "group"},
		{Key: "a", Title: "a", Parent: Ref{Key: "h"}},
		{Key: "b", Title: "b", Parent: Ref{Key: "g"}},
	}}, t0)
	if err != nil {
		t.Fatalf("valid end state refused: %v", err)
	}
	if b.Item(a).Parent != h || b.Item(ids["b"]).Parent != g || b.Item(g).Orchestrator == nil {
		t.Fatalf("%+v", b.Items)
	}

	_, err = b.AddBatch(Batch{Proposal: "p", Items: []ItemInput{
		{Key: "b", Title: "b", Parent: Ref{Key: "h"}},
		{Key: "h", Title: "other"},
	}}, t0)
	if err == nil || err.Error() != "B-1 has an orchestrator and must keep at least one child; clear it first" {
		t.Fatalf("emptied the group: %v", err)
	}
	if b.Item(ids["b"]).Parent != g {
		t.Fatal("refused batch applied")
	}
}

func TestMoveBoundGroupBeforeOrAfterChild(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3}, Item{ID: 4, Parent: 3})
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	for _, mv := range []Move{{Before: 4}, {After: 4}} {
		err := b.Move(1, mv, t0)
		if err == nil || err.Error() != "B-1 has an orchestrator and must stay a top-level parent; clear it first" {
			t.Fatalf("%+v: %v", mv, err)
		}
		if b.Item(1).Parent != 0 {
			t.Fatal("refused move applied")
		}
	}
	if err := b.Move(2, Move{Into: 3}, t0); err == nil || !strings.Contains(err.Error(), "must keep at least one child") {
		t.Fatal(err)
	}
}

func TestNormalizeTrimsSessionID(t *testing.T) {
	b := mk(t,
		Item{ID: 1, Orchestrator: &Orchestrator{SessionID: " s "}},
		Item{ID: 2, Parent: 1},
		Item{ID: 3, Orchestrator: &Orchestrator{SessionID: "s"}},
		Item{ID: 4, Parent: 3},
		Item{ID: 5, Orchestrator: &Orchestrator{SessionID: " "}},
		Item{ID: 6, Parent: 5},
	)
	if o := b.Item(1).Orchestrator; o == nil || o.SessionID != "s" {
		t.Fatalf("%+v", o)
	}
	if b.Item(3).Orchestrator != nil || b.Item(5).Orchestrator != nil {
		t.Fatal("kept a duplicate or blank session id")
	}
}

func TestOrchestratorJSONRoundTrip(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1})
	if err := b.BindOrchestrator(1, orch("s"), t0); err != nil {
		t.Fatal(err)
	}
	o := b.Item(1).Orchestrator
	o.Handoff = "notes"
	o.Meter = map[string]Meter{"/t.jsonl": {Offset: 10, Cost: 1.5, Last: "m1"}}
	o.Events = []OrchEvent{{Kind: EventQuestion, Item: 2, Message: 3, At: t0}, {Kind: EventFinished, At: t0}}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	got := New()
	if err := json.Unmarshal(raw, got); err != nil {
		t.Fatal(err)
	}
	if err := got.normalize(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Item(1).Orchestrator, o) {
		t.Fatalf("got %+v, want %+v", got.Item(1).Orchestrator, o)
	}
}

// TestClearingTheLastBindingWritesV2: the version follows the bindings both
// ways. While any group is bound the file is version 3; clearing one of two
// keeps it there; clearing the last brings that save back to version 2, which
// an older af reads again.
func TestClearingTheLastBindingWritesV2(t *testing.T) {
	home(t)
	path, _ := BacklogPath(repoID)
	version := func() float64 {
		t.Helper()
		raw, _ := os.ReadFile(path)
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj["schema_version"].(float64)
	}
	update := func(fn func(*Backlog) error) {
		t.Helper()
		if _, err := Update(repoID, fn); err != nil {
			t.Fatal(err)
		}
	}
	update(func(b *Backlog) error {
		for _, n := range []NewItem{{Title: "g1"}, {Title: "c1", Parent: 1}, {Title: "g2"}, {Title: "c2", Parent: 3}} {
			if _, err := b.Add(n, t0); err != nil {
				return err
			}
		}
		return nil
	})
	if v := version(); v != 2 {
		t.Fatalf("unbound: %v", v)
	}
	update(func(b *Backlog) error { return b.BindOrchestrator(1, orch("s1"), t0) })
	update(func(b *Backlog) error { return b.BindOrchestrator(3, orch("s3"), t0) })
	if v := version(); v != 3 {
		t.Fatalf("bound: %v", v)
	}
	update(func(b *Backlog) error { _, err := b.ClearOrchestrator(1, t0); return err })
	if v := version(); v != 3 {
		t.Fatalf("one binding left: %v", v)
	}
	update(func(b *Backlog) error { _, err := b.ClearOrchestrator(3, t0); return err })
	if v := version(); v != 2 {
		t.Fatalf("the last binding cleared: %v", v)
	}
}

// TestLoadsV3: a version 3 file loads. A bound one keeps its binding; one with
// no binding (the fork stamped every file v3) loads as unbound and its next
// save writes version 2.
func TestLoadsV3(t *testing.T) {
	home(t)
	path, _ := BacklogPath(repoID)
	writeRaw(t, path, `{"schema_version": 3, "next_id": 3, "items": [
		{"id": 1, "title": "group", "status": "draft"},
		{"id": 2, "title": "child", "status": "draft", "parent": 1}]}`)
	b, err := Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if b.SchemaVersion != 2 || b.Item(1).Orchestrator != nil {
		t.Fatalf("%+v", b)
	}
	if _, err := Update(repoID, func(*Backlog) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), `"schema_version": 2`) {
		t.Fatalf("an unbound v3 file saves as v2: %s", raw)
	}
}
