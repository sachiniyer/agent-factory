package backlog

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// mk builds a normalized backlog; items default to draft.
func mk(t *testing.T, items ...Item) *Backlog {
	t.Helper()
	b := New()
	for _, it := range items {
		if it.Status == "" {
			it.Status = StatusDraft
		}
		if it.Title == "" {
			it.Title = FormatID(it.ID)
		}
		b.Items = append(b.Items, it)
	}
	if err := b.normalize(); err != nil {
		t.Fatal(err)
	}
	return b
}

func order(b *Backlog) []int {
	var ids []int
	for _, it := range b.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

func TestParseID(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"12", 12, true},
		{"B-12", 12, true},
		{"b-7", 7, true},
		{" B-3 ", 3, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"B-", 0, false},
		{"B-0", 0, false},
		{"#12", 0, false},
		{"+5", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, err := ParseID(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseID(%q) = %d, %v; want %d ok=%v", c.in, got, err, c.want, c.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "B-12") {
			t.Errorf("ParseID(%q) error %q does not say what to use", c.in, err)
		}
	}
	if FormatID(12) != "B-12" {
		t.Fatal(FormatID(12))
	}
}

func TestNotFoundError(t *testing.T) {
	e := &NotFoundError{ID: 12, Project: "agent-factory"}
	want := `B-12 not found in project agent-factory — run "af backlog list --all"`
	if e.Error() != want {
		t.Fatalf("got %q", e.Error())
	}
	if !strings.HasPrefix((&NotFoundError{ID: 3}).Error(), "B-3 not found — run") {
		t.Fatal((&NotFoundError{ID: 3}).Error())
	}
}

// tree: 1 { 2, 3 { 4 } }, 5, 6
func sample(t *testing.T) *Backlog {
	return mk(t,
		Item{ID: 1}, Item{ID: 2, Parent: 1}, Item{ID: 3, Parent: 1}, Item{ID: 4, Parent: 3},
		Item{ID: 5}, Item{ID: 6},
	)
}

func TestMove(t *testing.T) {
	cases := []struct {
		name       string
		id         int
		to         Move
		wantOrder  []int
		wantParent int
		wantErr    string
	}{
		{"before top-level", 5, Move{Before: 1}, []int{5, 1, 2, 3, 4, 6}, 0, ""},
		{"after a parent skips its subtree", 6, Move{After: 1}, []int{1, 2, 3, 4, 6, 5}, 0, ""},
		{"subtree moves whole", 3, Move{After: 6}, []int{1, 2, 5, 6, 3, 4}, 0, ""},
		{"before a child joins its parent", 5, Move{Before: 2}, []int{1, 5, 2, 3, 4, 6}, 1, ""},
		{"after a child joins its parent", 6, Move{After: 4}, []int{1, 2, 3, 4, 6, 5}, 3, ""},
		{"into appends at the bottom", 5, Move{Into: 1}, []int{1, 2, 3, 4, 5, 6}, 1, ""},
		{"into a leaf makes it a parent", 6, Move{Into: 5}, []int{1, 2, 3, 4, 5, 6}, 5, ""},
		{"top keeps parent", 3, Move{Top: true}, []int{1, 3, 4, 2, 5, 6}, 1, ""},
		{"bottom keeps parent", 2, Move{Bottom: true}, []int{1, 3, 4, 2, 5, 6}, 1, ""},
		{"top of list", 6, Move{Top: true}, []int{6, 1, 2, 3, 4, 5}, 0, ""},
		{"bottom of list", 1, Move{Bottom: true}, []int{5, 6, 1, 2, 3, 4}, 0, ""},
		{"no target", 1, Move{}, nil, 0, "exactly one"},
		{"two targets", 1, Move{Top: true, Into: 5}, nil, 0, "exactly one"},
		{"into own descendant", 1, Move{Into: 4}, nil, 0, "inside it"},
		{"before itself", 1, Move{Before: 1}, nil, 0, "itself"},
		{"unknown item", 9, Move{Top: true}, nil, 0, "B-9 not found"},
		{"unknown anchor", 1, Move{After: 9}, nil, 0, "B-9 not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := sample(t)
			before := order(b)
			err := b.Move(c.id, c.to, t0)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				if !reflect.DeepEqual(order(b), before) {
					t.Fatal("backlog changed on error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := order(b); !reflect.DeepEqual(got, c.wantOrder) {
				t.Fatalf("order = %v, want %v", got, c.wantOrder)
			}
			if p := b.Item(c.id).Parent; p != c.wantParent {
				t.Fatalf("parent = %d, want %d", p, c.wantParent)
			}
		})
	}
}

func TestMoveRefusesParentWithRuns(t *testing.T) {
	for _, target := range []Item{
		{ID: 5, Status: StatusRunning},
		{ID: 5, Status: StatusReview, Runs: []RunRef{{SessionID: "s"}}},
	} {
		b := mk(t, Item{ID: 1}, target)
		err := b.Move(1, Move{Into: 5}, t0)
		if err == nil || err.Error() != "B-5 has runs; it cannot become a parent" {
			t.Fatalf("err = %v", err)
		}
		if _, err := b.Add(NewItem{Title: "x", Parent: 5}, t0); err == nil || !strings.Contains(err.Error(), "has runs") {
			t.Fatalf("add err = %v", err)
		}
	}
}

func TestMoveRefusesCycle(t *testing.T) {
	// 2 waits on 1; moving 1 into 2 makes 2 wait on its own child.
	b := mk(t, Item{ID: 1}, Item{ID: 2, WaitsOn: []int{1}})
	err := b.Move(1, Move{Into: 2}, t0)
	if err == nil || !strings.Contains(err.Error(), "wait cycle") {
		t.Fatalf("err = %v", err)
	}
	if b.Item(1).Parent != 0 {
		t.Fatal("changed on error")
	}
}

func TestAdd(t *testing.T) {
	b := sample(t)
	it, err := b.Add(NewItem{Title: "  New  ", Body: "b", Program: "codex"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != 7 || it.Title != "New" || it.Status != StatusDraft || it.Program != "codex" || !it.CreatedAt.Equal(t0) {
		t.Fatalf("item = %+v", it)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatal(got)
	}
	if b.NextID != 8 {
		t.Fatal(b.NextID)
	}

	child, err := b.Add(NewItem{Title: "child", Parent: 1, WaitsOn: []int{5, 5}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 8, 5, 6, 7}) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(child.WaitsOn, []int{5}) {
		t.Fatal(child.WaitsOn)
	}

	if _, err := b.Add(NewItem{Title: "first", Parent: 1, Before: 2}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add(NewItem{Title: "top", After: 6}, t0); err != nil {
		t.Fatal(err)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{1, 9, 2, 3, 4, 8, 5, 6, 10, 7}) {
		t.Fatal(got)
	}
}

func TestAddErrors(t *testing.T) {
	cases := []struct {
		name string
		in   NewItem
		want string
	}{
		{"empty title", NewItem{Title: " "}, "needs a title"},
		{"missing parent", NewItem{Title: "x", Parent: 9}, "B-9 not found"},
		{"missing wait", NewItem{Title: "x", WaitsOn: []int{9}}, "B-9 not found"},
		{"both anchors", NewItem{Title: "x", Before: 1, After: 5}, "not both"},
		{"anchor not a sibling", NewItem{Title: "x", Parent: 1, Before: 5}, "B-5 is not a child of B-1"},
		{"top-level anchor needs no parent", NewItem{Title: "x", Before: 2}, "not a child of the top level"},
		{"wait on own parent", NewItem{Title: "x", Parent: 1, WaitsOn: []int{1}}, "B-1 contains it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := sample(t)
			if _, err := b.Add(c.in, t0); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if len(b.Items) != 6 || b.NextID != 7 {
				t.Fatal("changed on error")
			}
		})
	}
}

func TestEdit(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, WaitsOn: []int{1}}, Item{ID: 3})
	title, body, prog, seq := "T", "body", "aider", true
	waits := []int{3}
	it, err := b.Edit(2, Edit{Title: &title, Body: &body, Program: &prog, Sequential: &seq, WaitsOn: &waits}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "T" || it.Body != "body" || it.Program != "aider" || !it.Sequential || !reflect.DeepEqual(it.WaitsOn, []int{3}) || !it.UpdatedAt.Equal(t0) {
		t.Fatalf("%+v", it)
	}
	// Replacing the list drops the old edge before checking the new ones.
	back := []int{2}
	if _, err := b.Edit(1, Edit{WaitsOn: &back}, t0); err != nil {
		t.Fatal(err)
	}
	cyc := []int{1}
	if _, err := b.Edit(3, Edit{WaitsOn: &cyc}, t0); err == nil || !strings.Contains(err.Error(), "already waits on") {
		t.Fatalf("err = %v", err)
	}
	empty := ""
	if _, err := b.Edit(3, Edit{Title: &empty}, t0); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := b.Edit(9, Edit{}, t0); err == nil {
		t.Fatal("missing item accepted")
	}
	if b.Item(3).WaitsOn != nil {
		t.Fatal("changed on error")
	}
}

func TestEditSequentialCycle(t *testing.T) {
	// 2 waits on 3; making 1 sequential makes 3 wait on 2.
	b := mk(t, Item{ID: 1}, Item{ID: 2, Parent: 1, WaitsOn: []int{3}}, Item{ID: 3, Parent: 1})
	seq := true
	if _, err := b.Edit(1, Edit{Sequential: &seq}, t0); err == nil || !strings.Contains(err.Error(), "wait cycle") {
		t.Fatalf("err = %v", err)
	}
	if b.Item(1).Sequential {
		t.Fatal("changed on error")
	}
}

func TestNormalize(t *testing.T) {
	b := &Backlog{Items: []Item{
		{ID: 4, Parent: 2, Status: StatusDraft},
		{ID: 1, Status: StatusDraft, WaitsOn: []int{1, 9, 4, 4}},
		{ID: 2, Parent: 1, Status: StatusDraft},
		{ID: 3, Parent: 99, Status: StatusDraft},
		{ID: 5, Parent: 6, Status: StatusDraft},
		{ID: 6, Parent: 5, Status: StatusDraft},
	}, Settings: Settings{MaxRunning: 40, OnDone: "bogus"}}
	if err := b.normalize(); err != nil {
		t.Fatal(err)
	}
	if got := order(b); !reflect.DeepEqual(got, []int{1, 2, 4, 3, 5, 6}) {
		t.Fatal(got)
	}
	if b.Item(3).Parent != 0 || b.Item(5).Parent != 0 || b.Item(6).Parent != 5 {
		t.Fatal("parents not repaired")
	}
	if b.Item(1).WaitsOn != nil {
		t.Fatalf("wait on own descendant kept: %v", b.Item(1).WaitsOn)
	}
	if b.NextID != 7 || b.Settings.MaxRunning != MaxMaxRunning || b.Settings.OnDone != OnDoneKeep {
		t.Fatalf("%+v %d", b.Settings, b.NextID)
	}

	for _, bad := range [][]Item{
		{{ID: 0, Status: StatusDraft}},
		{{ID: 1, Status: StatusDraft}, {ID: 1, Status: StatusDraft}},
		{{ID: 1, Status: "bogus"}},
	} {
		if err := (&Backlog{Items: bad}).normalize(); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	z := &Backlog{}
	if err := z.normalize(); err != nil || z.Items == nil || z.Settings.MaxRunning != 1 || z.NextID != 1 {
		t.Fatalf("%+v %v", z, err)
	}
}
