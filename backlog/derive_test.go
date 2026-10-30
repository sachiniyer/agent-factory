package backlog

import "testing"

func stubFacts(m map[string]SessionFacts) Facts {
	return func(id string) SessionFacts { return m[id] }
}

func TestDeriveLeaf(t *testing.T) {
	facts := stubFacts(map[string]SessionFacts{
		"active":   {Live: true, RunActive: true},
		"idle":     {Live: true},
		"archived": {Reason: "session archived"},
		"lost":     {Lost: true, AutoRestore: true, Reason: "session lost"},
		"lostno":   {Lost: true, Reason: "session lost"},
	})
	q := []Message{{ID: 1, Kind: KindQuestion, Text: "?"}}
	run := func(state, sid string) []RunRef { return []RunRef{{SessionID: sid, State: state}} }
	cases := []struct {
		name string
		it   Item
		want Derived
	}{
		{"draft", Item{Status: StatusDraft}, Derived{State: StateDraft}},
		{"queued", Item{Status: StatusQueued}, Derived{State: StateQueued}},
		{"done wins over question", Item{Status: StatusDone, Thread: q}, Derived{State: StateDone}},
		{"dropped", Item{Status: StatusDropped}, Derived{State: StateDropped}},
		{"question wins over review", Item{Status: StatusReview, Thread: q}, Derived{State: StateNeedsYou}},
		{"question on running", Item{Status: StatusRunning, Thread: q, Runs: run(RunStarted, "active")}, Derived{State: StateNeedsYou}},
		{"answered question", Item{Status: StatusReview, Thread: append(q, Message{ID: 2, Kind: KindAnswer, Answers: 1})}, Derived{State: StateInReview}},
		{"closed question", Item{Status: StatusReview, Thread: []Message{{ID: 1, Kind: KindQuestion, Closed: ClosedAnsweredInSession}}}, Derived{State: StateInReview}},
		{"review", Item{Status: StatusReview}, Derived{State: StateInReview}},
		{"working", Item{Status: StatusRunning, Runs: run(RunStarted, "active")}, Derived{State: StateWorking, HoldsSlot: true}},
		{"starting", Item{Status: StatusRunning, Runs: run(RunStarting, "")}, Derived{State: StateStarting, HoldsSlot: true}},
		{"stopped archived", Item{Status: StatusRunning, Runs: run(RunStarted, "archived")}, Derived{State: StateStopped, Reason: "session archived"}},
		{"stopped idle", Item{Status: StatusRunning, Runs: run(RunStarted, "idle")}, Derived{State: StateStopped, Reason: "no active run"}},
		{"stopped lost restorable", Item{Status: StatusRunning, Runs: run(RunStarted, "lost")}, Derived{State: StateStopped, Reason: "session lost", HoldsSlot: true}},
		{"stopped lost not restorable", Item{Status: StatusRunning, Runs: run(RunStarted, "lostno")}, Derived{State: StateStopped, Reason: "session lost"}},
		{"running no runs", Item{Status: StatusRunning}, Derived{State: StateStopped, Reason: "no session"}},
		{"latest run counts", Item{Status: StatusRunning, Runs: append(run(RunStarted, "active"), RunRef{SessionID: "archived", State: RunStarted})}, Derived{State: StateStopped, Reason: "session archived"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.it.ID = 1
			b := mk(t, c.it)
			if got := b.Derive(1, facts); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
	if got := mk(t, Item{ID: 1, Status: StatusRunning, Runs: run(RunStarted, "x")}).Derive(1, nil); got.State != StateStopped {
		t.Fatalf("nil facts: %+v", got)
	}
	if got := New().Derive(5, nil); got != (Derived{}) {
		t.Fatal(got)
	}
}

func TestDeriveWaiting(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
		id    int
		want  Derived
	}{
		{
			"waits on open item",
			[]Item{{ID: 5, Status: StatusRunning}, {ID: 6, Status: StatusQueued, WaitsOn: []int{5}}},
			6, Derived{State: StateWaiting, Reason: "Waiting on B-5"},
		},
		{
			"waits on done item",
			[]Item{{ID: 5, Status: StatusDone}, {ID: 6, Status: StatusQueued, WaitsOn: []int{5}}},
			6, Derived{State: StateQueued},
		},
		{
			"waits on dropped item",
			[]Item{{ID: 4, Status: StatusDropped}, {ID: 6, Status: StatusQueued, WaitsOn: []int{4}}},
			6, Derived{State: StateWaiting, Reason: "Waiting on B-4 (dropped)"},
		},
		{
			"first unmet wait named",
			[]Item{{ID: 4, Status: StatusDone}, {ID: 5, Status: StatusReview}, {ID: 6, Status: StatusQueued, WaitsOn: []int{4, 5}}},
			6, Derived{State: StateWaiting, Reason: "Waiting on B-5"},
		},
		{
			"draft is not waiting",
			[]Item{{ID: 5}, {ID: 6, Status: StatusDraft, WaitsOn: []int{5}}},
			6, Derived{State: StateDraft},
		},
		{
			"sequential earlier sibling open",
			[]Item{{ID: 1, Sequential: true}, {ID: 2, Parent: 1, Status: StatusReview}, {ID: 3, Parent: 1, Status: StatusQueued}},
			3, Derived{State: StateWaiting, Reason: "Waiting on B-2 (runs in order)"},
		},
		{
			"sequential earlier sibling done or dropped",
			[]Item{{ID: 1, Sequential: true}, {ID: 2, Parent: 1, Status: StatusDone}, {ID: 3, Parent: 1, Status: StatusDropped}, {ID: 4, Parent: 1, Status: StatusQueued}},
			4, Derived{State: StateQueued},
		},
		{
			"not sequential",
			[]Item{{ID: 1}, {ID: 2, Parent: 1, Status: StatusReview}, {ID: 3, Parent: 1, Status: StatusQueued}},
			3, Derived{State: StateQueued},
		},
		{
			"first sequential child",
			[]Item{{ID: 1, Sequential: true}, {ID: 2, Parent: 1, Status: StatusQueued}, {ID: 3, Parent: 1, Status: StatusQueued}},
			2, Derived{State: StateQueued},
		},
		{
			"inherits parent's wait",
			[]Item{{ID: 1, WaitsOn: []int{9}}, {ID: 2, Parent: 1, Status: StatusQueued}, {ID: 9, Status: StatusQueued}},
			2, Derived{State: StateWaiting, Reason: "Waiting on B-9"},
		},
		{
			"inherits grandparent's sequence",
			[]Item{{ID: 1, Sequential: true}, {ID: 2, Parent: 1, Status: StatusQueued}, {ID: 3, Parent: 1}, {ID: 4, Parent: 3, Status: StatusQueued}},
			4, Derived{State: StateWaiting, Reason: "Waiting on B-2 (runs in order)"},
		},
		{
			"waits on parent not done",
			[]Item{{ID: 1}, {ID: 2, Parent: 1, Status: StatusDone}, {ID: 3, Parent: 1, Status: StatusReview}, {ID: 6, Status: StatusQueued, WaitsOn: []int{1}}},
			6, Derived{State: StateWaiting, Reason: "Waiting on B-1"},
		},
		{
			"waits on parent rolled up done",
			[]Item{{ID: 1}, {ID: 2, Parent: 1, Status: StatusDone}, {ID: 3, Parent: 1, Status: StatusDropped}, {ID: 6, Status: StatusQueued, WaitsOn: []int{1}}},
			6, Derived{State: StateQueued},
		},
		{
			"waits on parent all dropped",
			[]Item{{ID: 1}, {ID: 2, Parent: 1, Status: StatusDropped}, {ID: 6, Status: StatusQueued, WaitsOn: []int{1}}},
			6, Derived{State: StateWaiting, Reason: "Waiting on B-1 (dropped)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := mk(t, c.items...)
			if got := b.Derive(c.id, nil); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDeriveCycleDoesNotHang(t *testing.T) {
	b := mk(t, Item{ID: 1, Status: StatusQueued, WaitsOn: []int{2}}, Item{ID: 2, Status: StatusQueued})
	b.Item(2).WaitsOn = []int{1}
	if got := b.Derive(1, nil); got != (Derived{State: StateWaiting, Reason: "Waiting on B-2"}) {
		t.Fatal(got)
	}
}

func TestDeriveRollup(t *testing.T) {
	facts := stubFacts(map[string]SessionFacts{"active": {Live: true, RunActive: true}})
	q := []Message{{ID: 1, Kind: KindQuestion}}
	working := []RunRef{{SessionID: "active", State: RunStarted}}
	kid := func(id int, s Status) Item { return Item{ID: id, Parent: 1, Status: s} }
	cases := []struct {
		name string
		kids []Item
		want Derived
	}{
		{"needs you wins", []Item{{ID: 2, Parent: 1, Status: StatusReview, Thread: q}, {ID: 3, Parent: 1, Status: StatusRunning, Runs: working}}, Derived{State: StateNeedsYou, Reason: "0 of 2 done"}},
		{"working", []Item{{ID: 2, Parent: 1, Status: StatusRunning, Runs: working}, kid(3, StatusReview)}, Derived{State: StateWorking, Reason: "0 of 2 done"}},
		{"starting counts as working", []Item{{ID: 2, Parent: 1, Status: StatusRunning, Runs: []RunRef{{State: RunStarting}}}, kid(3, StatusDone)}, Derived{State: StateWorking, Reason: "1 of 2 done"}},
		{"all done", []Item{kid(2, StatusDone), kid(3, StatusDone)}, Derived{State: StateDone, Reason: "2 of 2 done"}},
		{"done and dropped", []Item{kid(2, StatusDone), kid(3, StatusDropped)}, Derived{State: StateDone, Reason: "1 of 2 done"}},
		{"all dropped", []Item{kid(2, StatusDropped), kid(3, StatusDropped)}, Derived{State: StateDropped, Reason: "0 of 2 done"}},
		{"review over stopped", []Item{kid(2, StatusReview), kid(3, StatusRunning), kid(4, StatusDone)}, Derived{State: StateInReview, Reason: "1 of 3 done"}},
		{"stopped over queued", []Item{kid(2, StatusQueued), kid(3, StatusRunning)}, Derived{State: StateStopped, Reason: "0 of 2 done"}},
		{"queued over waiting", []Item{{ID: 2, Parent: 1, Status: StatusQueued, WaitsOn: []int{9}}, kid(3, StatusQueued)}, Derived{State: StateQueued, Reason: "0 of 2 done"}},
		{"waiting over draft", []Item{{ID: 2, Parent: 1, Status: StatusQueued, WaitsOn: []int{9}}, kid(3, StatusDraft)}, Derived{State: StateWaiting, Reason: "Waiting on B-9"}},
		{"draft", []Item{kid(2, StatusDraft), kid(3, StatusDone)}, Derived{State: StateDraft, Reason: "1 of 2 done"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items := append([]Item{{ID: 1, Status: StatusRunning}}, c.kids...)
			items = append(items, Item{ID: 9, Status: StatusQueued})
			b := mk(t, items...)
			if got := b.Derive(1, facts); got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDeriveNestedRollupAndAll(t *testing.T) {
	b := mk(t,
		Item{ID: 1},
		Item{ID: 2, Parent: 1},
		Item{ID: 3, Parent: 2, Status: StatusReview, Thread: []Message{{ID: 1, Kind: KindQuestion}}},
		Item{ID: 4, Parent: 1, Status: StatusDone},
	)
	all := b.DeriveAll(nil)
	if len(all) != 4 || all[1].State != StateNeedsYou || all[2].State != StateNeedsYou || all[3].State != StateNeedsYou || all[4].State != StateDone {
		t.Fatalf("%+v", all)
	}
}

func TestParseState(t *testing.T) {
	for st, label := range stateLabels {
		for _, in := range []string{string(st), label} {
			if got, err := ParseState(in); err != nil || got != st {
				t.Errorf("ParseState(%q) = %q, %v", in, got, err)
			}
		}
		if st.Label() != label {
			t.Errorf("%s label %q", st, st.Label())
		}
	}
	if _, err := ParseState("blocked"); err == nil {
		t.Fatal("blocked accepted")
	}
}
