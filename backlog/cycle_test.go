package backlog

import (
	"strings"
	"testing"
)

func TestCheckWaitsOn(t *testing.T) {
	// 1 { 2, 3 { 4 } }, 5 waits on 6, 6, 7 waits on 5
	build := func(t *testing.T) *Backlog {
		return mk(t,
			Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3, Parent: 1}, Item{ID: 4, Parent: 3},
			Item{ID: 5, WaitsOn: []int{6}}, Item{ID: 6}, Item{ID: 7, WaitsOn: []int{5}},
		)
	}
	cases := []struct {
		name     string
		id, on   int
		seq      bool
		want     string
		wantNone bool
	}{
		{name: "sibling", id: 2, on: 3, wantNone: true},
		{name: "unrelated", id: 5, on: 2, wantNone: true},
		{name: "child waits on other parent", id: 4, on: 6, wantNone: true},
		{name: "parent waits on unrelated", id: 1, on: 7, wantNone: true},
		{name: "self", id: 5, on: 5, want: "B-5 cannot wait on itself"},
		{name: "direct", id: 6, on: 5, want: "B-6 cannot wait on B-5: B-5 already waits on B-6 (B-6 → B-5 → B-6)"},
		{name: "indirect", id: 6, on: 7, want: "B-6 cannot wait on B-7: B-7 already waits on B-6 (B-6 → B-7 → B-5 → B-6)"},
		{name: "ancestor", id: 4, on: 1, want: "B-4 cannot wait on B-1: B-1 contains it"},
		{name: "descendant", id: 1, on: 4, want: "B-1 cannot wait on B-4: B-4 is inside it"},
		{name: "missing", id: 1, on: 9, want: "B-9 not found"},
		{name: "through parent", id: 6, on: 1, wantNone: true},
		{name: "sequential earlier waits on later", id: 2, on: 3, seq: true, want: "B-2 cannot wait on B-3: that would make a wait cycle (B-2 → B-3"},
		{name: "sequential later waits on earlier", id: 3, on: 2, seq: true, wantNone: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := build(t)
			b.Item(1).Sequential = c.seq
			err := b.CheckWaitsOn(c.id, c.on)
			if c.wantNone {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestCycleThroughParentWaits(t *testing.T) {
	// 1 { 2 } waits on 3; 3 waiting on 2 means 2 inherits a wait on itself.
	b := mk(t, Item{ID: 1, WaitsOn: []int{3}}, Item{ID: 2, Parent: 1}, Item{ID: 3})
	err := b.CheckWaitsOn(3, 2)
	if err == nil || err.Error() != "B-3 cannot wait on B-2: that would make a wait cycle (B-3 → B-2 → B-1 → B-3)" {
		t.Fatalf("err = %v", err)
	}
	if b.findCycle() != nil {
		t.Fatal("check mutated the backlog")
	}
}

func TestFindCycle(t *testing.T) {
	b := mk(t, Item{ID: 1, WaitsOn: []int{2}}, Item{ID: 2, WaitsOn: []int{3}}, Item{ID: 3}, Item{ID: 4})
	b.Item(3).WaitsOn = []int{1}
	c := b.findCycle()
	if len(c) != 4 || c[0] != c[3] {
		t.Fatalf("cycle = %v", c)
	}
	err := b.checkCycles()
	if err == nil || !strings.Contains(err.Error(), "wait cycle (B-1 → B-2 → B-3 → B-1)") {
		t.Fatalf("err = %v", err)
	}
	if mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3, WaitsOn: []int{1}}).findCycle() != nil {
		t.Fatal("false cycle")
	}
}
