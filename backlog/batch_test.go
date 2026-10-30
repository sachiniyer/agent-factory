package backlog

import (
	"encoding/json"
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
