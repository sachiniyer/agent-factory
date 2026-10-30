package backlog

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

const specBatch = `{
  "proposal": "backlog",
  "items": [
    {"key": "p", "title": "Backlog store", "sequential": true},
    {"key": "a", "parent": "p", "title": "backlog package and file", "body": "…"},
    {"key": "b", "parent": "p", "title": "daemon RPCs", "waits_on": ["a"]},
    {"key": "d", "title": "Docs", "waits_on": ["p", 7]}
  ]
}`

func parseBatch(t *testing.T, s string) Batch {
	t.Helper()
	var in Batch
	if err := json.Unmarshal([]byte(s), &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestAddBatch(t *testing.T) {
	b := mk(t, Item{ID: 7, Title: "existing"})
	ids, err := b.AddBatch(parseBatch(t, specBatch), t0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"p": 8, "a": 9, "b": 10, "d": 11}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v", ids)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{7, 8, 9, 10, 11}) {
		t.Fatalf("order = %v", got)
	}
	p, a, bb, d := b.Item(8), b.Item(9), b.Item(10), b.Item(11)
	if !p.Sequential || a.Parent != 8 || bb.Parent != 8 || d.Parent != 0 || a.Body != "…" {
		t.Fatalf("links wrong: %+v %+v", p, a)
	}
	if !reflect.DeepEqual(bb.WaitsOn, []int{9}) || !reflect.DeepEqual(d.WaitsOn, []int{8, 7}) {
		t.Fatalf("waits wrong: %v %v", bb.WaitsOn, d.WaitsOn)
	}
	for _, it := range b.Items[1:] {
		if it.Status != StatusDraft || it.Source == nil || it.Source.Proposal != "backlog" {
			t.Fatalf("%+v", it)
		}
	}

	// Re-running updates by (proposal, key) and keeps status and position.
	b.Item(9).Status = StatusQueued
	if err := b.Move(11, Move{Top: true}, t0); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(time.Hour)
	again := parseBatch(t, `{"proposal": "backlog", "items": [
		{"key": "p", "title": "Backlog store v2", "sequential": true},
		{"key": "a", "parent": "p", "title": "package"},
		{"key": "b", "parent": "p", "title": "daemon RPCs"},
		{"key": "d", "title": "Docs"},
		{"key": "e", "parent": "p", "title": "new step", "waits_on": ["b"]}
	]}`)
	ids, err = b.AddBatch(again, later)
	if err != nil {
		t.Fatal(err)
	}
	if ids["p"] != 8 || ids["e"] != 12 || len(b.Items) != 6 {
		t.Fatalf("ids = %v, items = %d", ids, len(b.Items))
	}
	if got := order(b); !reflect.DeepEqual(got, []int{11, 7, 8, 9, 10, 12}) {
		t.Fatalf("order = %v", got)
	}
	if b.Item(8).Title != "Backlog store v2" || b.Item(9).Status != StatusQueued || b.Item(9).Body != "" || b.Item(10).WaitsOn != nil || !b.Item(9).CreatedAt.Equal(t0) || !b.Item(9).UpdatedAt.Equal(later) {
		t.Fatalf("update wrong: %+v", b.Item(9))
	}

	// A batch without a proposal always adds.
	ids, err = b.AddBatch(parseBatch(t, `{"items": [{"key": "p", "title": "dup"}]}`), t0)
	if err != nil || ids["p"] != 13 || b.Item(13).Source != nil {
		t.Fatal(ids, err)
	}
}

func TestAddBatchParentChange(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2}, Item{ID: 3, Source: &Source{Proposal: "x", Key: "c"}})
	_, err := b.AddBatch(Batch{Proposal: "x", Items: []ItemInput{{Key: "c", Title: "c", Parent: Ref{ID: 1}}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{1, 3, 2}) || b.Item(3).Parent != 1 {
		t.Fatalf("order = %v", got)
	}
}

func TestAddBatchChildBeforeParentInFile(t *testing.T) {
	b := New()
	ids, err := b.AddBatch(parseBatch(t, `{"items": [
		{"key": "c", "parent": "p", "title": "child"},
		{"key": "p", "title": "parent"}
	]}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{ids["p"], ids["c"]}) {
		t.Fatalf("order = %v", got)
	}
}

func TestAddBatchErrors(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", `{"items": []}`, "no items"},
		{"no title", `{"items": [{"key": "a"}]}`, "item 1 in the batch needs a title"},
		{"dup key", `{"items": [{"key": "a", "title": "x"}, {"key": "a", "title": "y"}]}`, `key "a" is used twice`},
		{"unknown key", `{"items": [{"key": "a", "title": "x", "waits_on": ["zz"]}]}`, `key "zz" is not in the batch`},
		{"unknown id", `{"items": [{"key": "a", "title": "x", "parent": 99}]}`, "B-99 not found"},
		{"own parent", `{"items": [{"key": "a", "title": "x", "parent": "a"}]}`, "own parent"},
		{"parent loop", `{"items": [{"key": "a", "title": "x", "parent": "b"}, {"key": "b", "title": "y", "parent": "a"}]}`, "loop"},
		{"wait cycle", `{"items": [{"key": "a", "title": "x", "waits_on": ["b"]}, {"key": "b", "title": "y", "waits_on": ["a"]}]}`, "already waits on"},
		{"wait on parent", `{"items": [{"key": "a", "title": "x"}, {"key": "b", "title": "y", "parent": "a", "waits_on": ["a"]}]}`, "contains it"},
		{"parent has runs", `{"items": [{"key": "a", "title": "x", "parent": 1}]}`, "B-1 has runs"},
		{"self wait", `{"items": [{"key": "a", "title": "x", "waits_on": ["a"]}]}`, "cannot wait on itself"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := mk(t, Item{ID: 1, Status: StatusReview, Runs: []RunRef{{SessionID: "s"}}})
			_, err := b.AddBatch(parseBatch(t, c.in), t0)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if len(b.Items) != 1 || b.NextID != 2 {
				t.Fatal("changed on error")
			}
		})
	}
}

func TestRefJSON(t *testing.T) {
	var r []Ref
	if err := json.Unmarshal([]byte(`["a", 7]`), &r); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r, []Ref{{Key: "a"}, {ID: 7}}) {
		t.Fatal(r)
	}
	out, _ := json.Marshal(r)
	if string(out) != `["a",7]` {
		t.Fatal(string(out))
	}
	for _, bad := range []string{`0`, `-3`, `1.5`, `true`} {
		var x Ref
		if err := json.Unmarshal([]byte(bad), &x); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	in, _ := json.Marshal(ItemInput{Title: "x"})
	if strings.Contains(string(in), "parent") {
		t.Fatalf("empty parent serialized: %s", in)
	}
}

// A proposal update that removes an item's parent makes it top-level, instead of
// leaving it under the old parent (Codex P2 on #6286).
func TestAddBatchDetachesAnExistingChildWhenTheParentIsRemoved(t *testing.T) {
	b := New()
	if _, err := b.AddBatch(Batch{Proposal: "x", Items: []ItemInput{
		{Key: "parent", Title: "parent"},
		{Key: "child", Title: "child", Parent: Ref{Key: "parent"}},
	}}, t0); err != nil {
		t.Fatal(err)
	}
	ids, err := b.AddBatch(Batch{Proposal: "x", Items: []ItemInput{{Key: "child", Title: "child"}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Item(ids["child"]).Parent; got != 0 {
		t.Fatalf("child still has parent %d after the update removed it", got)
	}
}

// The dependency check runs on the finished batch, not after each move: a valid
// final tree may pass through a cyclic one (Codex P2 on #6286). A waits on B and
// both sit under P; the update makes P sequential and moves P and B under Q, so
// P is moved while it still holds both A and B in sequence.
func TestAddBatchChecksDependenciesOnTheFinishedTree(t *testing.T) {
	src := func(key string) *Source { return &Source{Proposal: "x", Key: key} }
	b := mk(t,
		Item{ID: 1, Source: src("q")},
		Item{ID: 2, Source: src("p")},
		Item{ID: 3, Parent: 2},
		Item{ID: 4, Parent: 2, Source: src("b")},
	)
	if err := b.setWaits(3, []int{4}); err != nil {
		t.Fatal(err)
	}
	_, err := b.AddBatch(Batch{Proposal: "x", Items: []ItemInput{
		{Key: "q", Title: "q"},
		{Key: "p", Title: "p", Parent: Ref{Key: "q"}, Sequential: true},
		{Key: "b", Title: "b", Parent: Ref{Key: "q"}},
	}}, t0)
	if err != nil {
		t.Fatalf("a valid final tree was refused: %v", err)
	}
	if b.Item(4).Parent != 1 || b.Item(3).Parent != 2 || b.Item(2).Parent != 1 {
		t.Fatalf("wrong final tree: %+v", b.Items)
	}
}

// A batch whose FINAL tree has a cycle is still refused, and b is unchanged.
func TestAddBatchStillRefusesAFinalCycle(t *testing.T) {
	b := New()
	_, err := b.AddBatch(Batch{Items: []ItemInput{
		{Key: "a", Title: "a", WaitsOn: []Ref{{Key: "b"}}},
		{Key: "b", Title: "b", WaitsOn: []Ref{{Key: "a"}}},
	}}, t0)
	if err == nil || len(b.Items) != 0 {
		t.Fatalf("err = %v, items = %d", err, len(b.Items))
	}
}

// The final parent of an input can be an item the batch leaves alone whose own
// ancestor the batch is about to relocate (Codex P2 on #6286, round 3): with the
// tree A > C > B, listing A under B and then C at the top level is valid
// (C > B > A) in either input order.
func TestAddBatchOrdersMovesThroughUnchangedIntermediateParents(t *testing.T) {
	build := func() *Backlog {
		src := func(key string) *Source { return &Source{Proposal: "x", Key: key} }
		return mk(t,
			Item{ID: 1, Source: src("a")},
			Item{ID: 2, Parent: 1, Source: src("c")},
			Item{ID: 3, Parent: 2},
		)
	}
	forward := []ItemInput{
		{Key: "a", Title: "a", Parent: Ref{ID: 3}},
		{Key: "c", Title: "c"},
	}
	reversed := []ItemInput{forward[1], forward[0]}
	for name, items := range map[string][]ItemInput{"listed in file order": forward, "reversed": reversed} {
		t.Run(name, func(t *testing.T) {
			b := build()
			if _, err := b.AddBatch(Batch{Proposal: "x", Items: items}, t0); err != nil {
				t.Fatalf("a valid final tree was refused: %v", err)
			}
			if b.Item(2).Parent != 0 || b.Item(3).Parent != 2 || b.Item(1).Parent != 3 {
				t.Fatalf("want C > B > A, got %+v", b.Items)
			}
		})
	}
}

// A final tree whose parent links loop is refused whatever the input order, and
// leaves the backlog unchanged.
func TestAddBatchRefusesAFinalParentLoop(t *testing.T) {
	b := New()
	_, err := b.AddBatch(Batch{Items: []ItemInput{
		{Key: "a", Title: "a", Parent: Ref{Key: "b"}},
		{Key: "b", Title: "b", Parent: Ref{Key: "a"}},
	}}, t0)
	if err == nil || len(b.Items) != 0 {
		t.Fatalf("err = %v, items = %d", err, len(b.Items))
	}
}

// Property: for any starting forest and any acyclic requested parent assignment,
// in any input order, AddBatch succeeds and the finished tree has exactly the
// requested parents. Two review rounds on #6286 found input-order bugs one at a
// time; this checks the whole class.
func TestAddBatchReachesTheRequestedTreeInAnyOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(6286))
	const n = 6
	for trial := 0; trial < 400; trial++ {
		items := make([]Item, n)
		for i := range items {
			items[i] = Item{ID: i + 1, Source: &Source{Proposal: "x", Key: fmt.Sprint(i + 1)}}
		}
		// A random starting forest: ranks give an acyclic parent choice.
		start := rng.Perm(n)
		for rank, id := range start {
			if rank > 0 && rng.Intn(4) > 0 {
				items[id].Parent = start[rng.Intn(rank)] + 1
			}
		}
		b := New()
		for _, it := range items {
			it.Status, it.Title = StatusDraft, FormatID(it.ID)
			b.Items = append(b.Items, it)
		}
		if b.normalize() != nil {
			t.Fatalf("trial %d: generator built an invalid start", trial)
		}
		// Request new parents for a random subset; the rest keep theirs. Skip
		// the draws whose finished tree would loop (AddBatch must refuse those).
		want := map[int]int{}
		pos := rng.Perm(n)
		for rank, id := range pos {
			if rng.Intn(2) == 0 {
				continue
			}
			if rank > 0 && rng.Intn(3) > 0 {
				want[id+1] = pos[rng.Intn(rank)] + 1
			} else {
				want[id+1] = 0
			}
		}
		if len(want) == 0 {
			continue
		}
		final := map[int]int{}
		for _, it := range items {
			final[it.ID] = it.Parent
		}
		for id, p := range want {
			final[id] = p
		}
		loops := false
		for id := range final {
			for cur, steps := id, 0; cur != 0; steps++ {
				if steps > n {
					loops = true
					break
				}
				cur = final[cur]
			}
		}
		if loops {
			continue
		}
		var in []ItemInput
		for _, idx := range rng.Perm(n) {
			id := idx + 1
			p, listed := want[id]
			if !listed {
				continue
			}
			x := ItemInput{Key: fmt.Sprint(id), Title: FormatID(id)}
			if p != 0 {
				x.Parent = Ref{ID: p}
			}
			in = append(in, x)
		}
		if _, err := b.AddBatch(Batch{Proposal: "x", Items: in}, t0); err != nil {
			t.Fatalf("trial %d: a valid final tree was refused: %v\nstart %+v\nwant %v", trial, err, items, want)
		}
		for id, p := range final {
			if b.Item(id).Parent != p {
				t.Fatalf("trial %d: %s has parent %d, want %d", trial, FormatID(id), b.Item(id).Parent, p)
			}
		}
	}
}

// A batch with a null reference survives a marshal/unmarshal round trip (Codex
// P2 on #6288): the CLI re-serializes the file it read before sending it to a
// remote daemon.
func TestBatchWithANullReferenceRoundTrips(t *testing.T) {
	in := parseBatch(t, `{"items":[{"key":"a","title":"a","waits_on":[null,"b"]},{"key":"b","title":"b","parent":null}]}`)
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Batch
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("the re-serialized batch is refused: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the batch:\n%+v\n%+v", in, out)
	}
}
