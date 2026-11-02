package backlog

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

// eventsGroup is group 1 bound to "o" with leaves 2 and 3 (3 under parent 4),
// and group 5 with leaf 6, unbound.
func eventsGroup(t *testing.T) *Backlog {
	t.Helper()
	run := func(s string) []RunRef {
		return []RunRef{{SessionID: s, State: RunStarted, StartedAt: t0}}
	}
	b := mk(t,
		Item{ID: 1, Title: "Effort"},
		Item{ID: 2, Parent: 1, Status: StatusRunning, Runs: run("s2")},
		Item{ID: 4, Parent: 1},
		Item{ID: 3, Parent: 4, Status: StatusQueued},
		Item{ID: 5},
		Item{ID: 6, Parent: 5, Status: StatusRunning, Runs: run("s6")},
	)
	if err := b.BindOrchestrator(1, orch("o"), t0); err != nil {
		t.Fatal(err)
	}
	return b
}

func kinds(b *Backlog, group int) []OrchEvent {
	var out []OrchEvent
	for _, e := range b.Item(group).Orchestrator.Events {
		out = append(out, OrchEvent{Kind: e.Kind, Item: e.Item, Message: e.Message})
	}
	return out
}

func TestQueueOrchChangesQuestion(t *testing.T) {
	b := eventsGroup(t)
	prev := b.clone()
	if _, err := b.Ask(2, "s2", Question{Text: "which?"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Ask(6, "s6", Question{Text: "unbound"}, t0); err != nil {
		t.Fatal(err)
	}
	b.Item(2).appendMessage(Message{Kind: KindQuestion, Author: AuthorAF, Text: "af asks"}, t0)
	if got := b.queueOrchChanges(prev, t0); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("groups %v", got)
	}
	want := []OrchEvent{{Kind: EventQuestion, Item: 2, Message: 1}}
	if got := kinds(b, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
	if got := b.queueOrchChanges(b.clone(), t0); got != nil {
		t.Fatalf("no change queued %v", got)
	}
}

func TestQueueOrchChangesReviewAndFinished(t *testing.T) {
	b := eventsGroup(t)
	prev := b.clone()
	b.Item(2).Status = StatusReview
	b.queueOrchChanges(prev, t0)
	if got := kinds(b, 1); !reflect.DeepEqual(got, []OrchEvent{{Kind: EventReview, Item: 2}}) {
		t.Fatalf("events %+v", got)
	}

	prev = b.clone()
	b.Item(2).Status = StatusDone
	b.queueOrchChanges(prev, t0)
	if got := kinds(b, 1); len(got) != 1 {
		t.Fatalf("finished before the nested leaf closed: %+v", got)
	}

	prev = b.clone()
	b.Item(3).Status = StatusDropped
	b.queueOrchChanges(prev, t0)
	want := []OrchEvent{{Kind: EventReview, Item: 2}, {Kind: EventFinished, Item: 1}}
	if got := kinds(b, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}

	b.Item(1).Orchestrator.Events = nil
	b.queueOrchChanges(b.clone(), t0)
	if got := kinds(b, 1); got != nil {
		t.Fatalf("finished twice: %+v", got)
	}
}

func TestQueueOrchChangesEscalationAnswered(t *testing.T) {
	b := eventsGroup(t)
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	q.Route = RouteUser
	prev := b.clone()
	if _, err := b.Answer(2, 0, "this one", t0); err != nil {
		t.Fatal(err)
	}
	b.queueOrchChanges(prev, t0)
	want := []OrchEvent{{Kind: EventEscalationAnswered, Item: 2, Message: 1}}
	if got := kinds(b, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
}

func TestQueueOrchChangesRoutedAnswerIsNotEscalation(t *testing.T) {
	b := eventsGroup(t)
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	q.Route = RouteOrchestrator
	prev := b.clone()
	if _, err := b.Answer(2, 0, "this one", t0); err != nil {
		t.Fatal(err)
	}
	if got := b.queueOrchChanges(prev, t0); got != nil {
		t.Fatalf("queued on %v", got)
	}
}

func TestServiceWriteQueuesOrchEvents(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	h.reset()
	if _, err := s.Apply(ProjectRef{ID: repoID}, func(b *Backlog) ([]int, error) {
		b.Item(2).Status = StatusReview
		return []int{2}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.last(); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("changed %v", got)
	}
	b, err := Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	ev := b.Item(1).Orchestrator.Events
	if len(ev) != 1 || ev[0].Kind != EventReview || ev[0].Item != 2 || !ev[0].At.Equal(s.now()) {
		t.Fatalf("events %+v", ev)
	}
}

func TestQueueOrchStallsWorkingRun(t *testing.T) {
	b := eventsGroup(t)
	act := RunActivity{Working: true, LastActive: t0.Add(time.Minute)}
	activity := func(id string) (RunActivity, bool) { return act, true }

	if got := b.QueueOrchStalls(activity, t0.Add(30*time.Minute)); got != nil {
		t.Fatalf("stalled at 29 minutes idle: %v", got)
	}
	at := t0.Add(31 * time.Minute)
	if got := b.QueueOrchStalls(activity, at); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("groups %v", got)
	}
	if !b.Item(2).lastRun().StalledAt.Equal(at) {
		t.Fatalf("stalled at %v", b.Item(2).lastRun().StalledAt)
	}
	if !b.Item(6).lastRun().StalledAt.IsZero() {
		t.Fatal("an unbound group's run was marked")
	}
	b.Item(1).Orchestrator.Events = nil
	if got := b.QueueOrchStalls(activity, t0.Add(5*time.Hour)); got != nil {
		t.Fatalf("one idle period stalled twice: %v", got)
	}

	act.LastActive = t0.Add(time.Hour)
	if got := b.QueueOrchStalls(activity, t0.Add(80*time.Minute)); got != nil {
		t.Fatalf("new period stalled early: %v", got)
	}
	if got := b.QueueOrchStalls(activity, t0.Add(90*time.Minute)); got == nil {
		t.Fatal("a new idle period did not stall")
	}
	if got := kinds(b, 1); !reflect.DeepEqual(got, []OrchEvent{{Kind: EventStalled, Item: 2}}) {
		t.Fatalf("events %+v", got)
	}
}

func TestQueueOrchStallsStoppedSession(t *testing.T) {
	b := eventsGroup(t)
	activity := func(string) (RunActivity, bool) { return RunActivity{Stopped: true}, true }
	if got := b.QueueOrchStalls(activity, t0.Add(time.Minute)); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("groups %v", got)
	}
	if got := b.QueueOrchStalls(activity, t0.Add(time.Hour)); got != nil {
		t.Fatalf("a stopped run stalled twice: %v", got)
	}
}

func TestQueueOrchStallsSkips(t *testing.T) {
	stopped := func(string) (RunActivity, bool) { return RunActivity{Stopped: true}, true }
	cases := map[string]struct {
		edit     func(*Backlog)
		activity func(string) (RunActivity, bool)
	}{
		"unknown session": {activity: func(string) (RunActivity, bool) { return RunActivity{Stopped: true}, false }},
		"neither working nor stopped": {activity: func(string) (RunActivity, bool) {
			return RunActivity{}, true
		}},
		"open question": {activity: stopped, edit: func(b *Backlog) {
			if _, err := b.Ask(2, "s2", Question{Text: "?"}, t0); err != nil {
				t.Fatal(err)
			}
		}},
		"in review":     {activity: stopped, edit: func(b *Backlog) { b.Item(2).Status = StatusReview }},
		"run starting":  {activity: stopped, edit: func(b *Backlog) { b.Item(2).Runs[0].State = RunStarting }},
		"group unbound": {activity: stopped, edit: func(b *Backlog) { b.Item(1).Orchestrator = nil }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			b := eventsGroup(t)
			if c.edit != nil {
				c.edit(b)
			}
			if got := b.QueueOrchStalls(c.activity, t0.Add(time.Hour)); got != nil {
				t.Fatalf("queued on %v", got)
			}
		})
	}
}

func TestQueueOrchChangesReviewOnNewPR(t *testing.T) {
	b := eventsGroup(t)
	b.Item(2).Status = StatusReview
	prev := b.clone()
	b.Item(2).PR = "https://example.com/pr/1"
	b.queueOrchChanges(prev, t0)
	if got := kinds(b, 1); !reflect.DeepEqual(got, []OrchEvent{{Kind: EventReview, Item: 2}}) {
		t.Fatalf("events %+v", got)
	}

	b.Item(1).Orchestrator.Events = nil
	prev = b.clone()
	b.Item(2).PR = ""
	if got := b.queueOrchChanges(prev, t0); got != nil {
		t.Fatalf("a cleared PR queued on %v", got)
	}
}

func TestQueueOrchChangesEscalationAnsweredInSession(t *testing.T) {
	b := eventsGroup(t)
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	q.Route = RouteUser
	prev := b.clone()
	if b.AnsweredInSessionText("s2", q.ID, "", nil, t0) != 2 {
		t.Fatal("not answered in session")
	}
	b.queueOrchChanges(prev, t0)
	want := []OrchEvent{{Kind: EventEscalationAnswered, Item: 2, Message: 1}}
	if got := kinds(b, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
}
