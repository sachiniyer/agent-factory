package backlog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// boundGroup is group 1 with children 2 and 3, bound to session "o".
func boundGroup(t *testing.T) *Backlog {
	t.Helper()
	b := mk(t, Item{ID: 1, Title: "Effort"}, Item{ID: 2, Parent: 1, Title: "Two"}, Item{ID: 3, Parent: 2, Title: "Three"}, Item{ID: 4, Title: "Alone"})
	if err := b.BindOrchestrator(1, orch("o"), t0); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestQueueOrchEventGoesToTheTopGroup(t *testing.T) {
	b := boundGroup(t)
	if g := b.QueueOrchEvent(OrchEvent{Kind: EventQuestion, Item: 3, Message: 1}, t0); g != 1 {
		t.Fatalf("group %d", g)
	}
	if g := b.QueueOrchEvent(OrchEvent{Kind: EventQuestion, Item: 3, Message: 1}, t0.Add(time.Second)); g != 1 {
		t.Fatalf("repeat group %d", g)
	}
	b.QueueOrchEvent(OrchEvent{Kind: EventFinished, Item: 1}, t0.Add(time.Minute))
	want := []OrchEvent{{Kind: EventQuestion, Item: 3, Message: 1, At: t0}, {Kind: EventFinished, Item: 1, At: t0.Add(time.Minute)}}
	if got := b.Item(1).Orchestrator.Events; !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
	if g := b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 4}, t0); g != 0 {
		t.Fatalf("an unbound item queued on %d", g)
	}
	if g := b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 9}, t0); g != 0 {
		t.Fatalf("a missing item queued on %d", g)
	}
}

func TestOrchDeliveriesOnlyBoundGroupsWithEvents(t *testing.T) {
	b := boundGroup(t)
	if got := b.OrchDeliveries(t0); len(got) != 0 {
		t.Fatalf("no events, deliveries %+v", got)
	}
	b.QueueOrchEvent(OrchEvent{Kind: EventReview, Item: 2}, t0)
	got := b.OrchDeliveries(t0)
	if len(got) != 1 || got[0].Group != 1 || got[0].SessionID != "o" || got[0].Program != "claude" || len(got[0].Events) != 1 {
		t.Fatalf("%+v", got)
	}
	got[0].Events[0].Kind = "changed"
	if b.Item(1).Orchestrator.Events[0].Kind != EventReview {
		t.Fatal("a delivery shares the outbox")
	}
}

func TestOrchDeliveredDropsOnlyWhatWasSent(t *testing.T) {
	b := boundGroup(t)
	b.QueueOrchEvent(OrchEvent{Kind: EventReview, Item: 2}, t0)
	sent := b.OrchDeliveries(t0)[0].Events
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0.Add(time.Second))
	b.OrchDeliveryFailed(1, "o", "x", t0)

	if b.OrchDelivered(1, "other", sent, t0) {
		t.Fatal("a session no longer bound dropped events")
	}
	if !b.OrchDelivered(1, "o", sent, t0.Add(2*time.Second)) {
		t.Fatal("not delivered")
	}
	o := b.Item(1).Orchestrator
	if len(o.Events) != 1 || o.Events[0].Kind != EventStalled || o.Failures != nil {
		t.Fatalf("%+v", o)
	}
}

func TestOrchDeliveryFailedBacksOffThenEscalates(t *testing.T) {
	b := boundGroup(t)
	b.QueueOrchEvent(OrchEvent{Kind: EventReview, Item: 2}, t0)

	if b.OrchDeliveryFailed(1, "o", "boom", t0) {
		t.Fatal("first failure escalated")
	}
	if got := b.OrchDeliveries(t0.Add(OrchRetryStep - time.Second)); len(got) != 0 {
		t.Fatal("retried inside the first wait")
	}
	at := t0.Add(OrchRetryStep)
	if got := b.OrchDeliveries(at); len(got) != 1 {
		t.Fatal("no retry after the first wait")
	}
	if b.OrchDeliveryFailed(1, "o", "boom", at) {
		t.Fatal("second failure escalated")
	}
	if got := b.OrchDeliveries(at.Add(2*OrchRetryStep - time.Second)); len(got) != 0 {
		t.Fatal("retried inside the second wait")
	}
	at = at.Add(2 * OrchRetryStep)
	if !b.OrchDeliveryFailed(1, "o", "restore failed", at) {
		t.Fatal("third failure in the window did not escalate")
	}
	g := b.Item(1)
	q := g.OpenQuestion()
	if q == nil || q.Author != AuthorAF || !strings.Contains(q.Text, `"t-o"`) || !strings.Contains(q.Text, "restore failed") {
		t.Fatalf("question %+v", q)
	}
	if !b.OrchEscalated(1) || len(b.OrchDeliveries(at.Add(time.Hour))) != 0 {
		t.Fatal("delivery goes on while escalated")
	}
	if len(g.Orchestrator.Events) != 1 {
		t.Fatal("escalation dropped the events")
	}
	if inbox := b.Inbox(); len(inbox) != 1 || inbox[0].ID != 1 {
		t.Fatalf("inbox %+v", inbox)
	}
}

func TestOrchDeliveryFailedForgetsOldFailures(t *testing.T) {
	b := boundGroup(t)
	b.OrchDeliveryFailed(1, "o", "x", t0)
	b.OrchDeliveryFailed(1, "o", "x", t0.Add(time.Minute))
	if b.OrchDeliveryFailed(1, "o", "x", t0.Add(OrchFailureWindow)) {
		t.Fatal("a failure outside the window counted")
	}
	if got := len(b.Item(1).Orchestrator.Failures); got != 2 {
		t.Fatalf("failures %d", got)
	}
	if b.OrchDeliveryFailed(1, "other", "x", t0.Add(OrchFailureWindow)) {
		t.Fatal("a session no longer bound escalated")
	}
}

func TestOrchReplacedRebindsAndKeepsTheRest(t *testing.T) {
	b := boundGroup(t)
	if _, err := b.SetOrchestratorHandoff("o", "notes", t0); err != nil {
		t.Fatal(err)
	}
	b.QueueOrchEvent(OrchEvent{Kind: EventReview, Item: 2}, t0)
	sent := append([]OrchEvent(nil), b.Item(1).Orchestrator.Events...)
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0)
	b.OrchDeliveryFailed(1, "o", "x", t0)

	if err := b.OrchReplaced(1, "stale", orch("n"), sent, t0); err == nil {
		t.Fatal("replaced a session that is not bound")
	}
	if err := b.OrchReplaced(1, "o", orch("n"), sent, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	o := b.Item(1).Orchestrator
	if o.SessionID != "n" || o.Handoff != "notes" || len(o.Events) != 1 || o.Events[0].Kind != EventStalled ||
		o.Failures != nil || !o.BoundAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", o)
	}
}

func TestOrchOutboxSurvivesASave(t *testing.T) {
	b := boundGroup(t)
	b.QueueOrchEvent(OrchEvent{Kind: EventQuestion, Item: 3, Message: 2}, t0)
	b.OrchDeliveryFailed(1, "o", "x", t0)
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back Backlog
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	o := back.Item(1).Orchestrator
	if len(o.Events) != 1 || len(o.Failures) != 1 || !o.Failures[0].Equal(t0) {
		t.Fatalf("%+v", o)
	}
	if !back.OrchDelivered(1, "o", back.OrchDeliveries(t0.Add(OrchRetryStep))[0].Events, t0) || len(back.Item(1).Orchestrator.Events) != 0 {
		t.Fatal("events read back did not match the outbox")
	}
}

func TestOrchPrompts(t *testing.T) {
	b := boundGroup(t)
	b.Item(2).PR = "https://example.test/pr/1"
	if _, err := b.SetOrchestratorHandoff("o", "Merge B-2 first.", t0); err != nil {
		t.Fatal(err)
	}
	events := []OrchEvent{{Kind: EventQuestion, Item: 3}, {Kind: EventReview, Item: 2}, {Kind: EventStalled, Item: 3},
		{Kind: EventFinished, Item: 1}, {Kind: EventEscalationAnswered, Item: 3}}
	p := b.OrchEventsPrompt(1, events)
	for _, want := range []string{"Events in effort B-1:", "- B-3 Three: the agent asked a question.",
		"- B-2 Two: ready for review, https://example.test/pr/1.", "- B-3 Three: stalled.",
		"- B-1 Effort: every item is done or dropped.", "the user answered the question you escalated", `af backlog show B-1`} {
		if !strings.Contains(p, want) {
			t.Errorf("events prompt lacks %q:\n%s", want, p)
		}
	}

	p = b.OrchStartPrompt(1, "/p/effort", true, events[:1])
	for _, want := range []string{"You orchestrate backlog effort B-1: Effort", "proposal in /p/effort",
		"af started this session to replace it", "Merge B-2 first.", "--handoff-file", "the agent asked a question"} {
		if !strings.Contains(p, want) {
			t.Errorf("start prompt lacks %q:\n%s", want, p)
		}
	}
	if p = b.OrchStartPrompt(1, "", false, nil); !strings.Contains(p, "restarted without its earlier conversation") ||
		strings.Contains(p, "Events in") || strings.Contains(p, "proposal in") {
		t.Errorf("restart prompt:\n%s", p)
	}
}

func TestQueueOrchEventDuringADeliveryIsKept(t *testing.T) {
	b := boundGroup(t)
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0)
	d, ok, changed := b.BeginOrchDelivery(1, t0)
	if !ok || !changed || len(d.Events) != 1 || b.Item(1).Orchestrator.Sending != 1 {
		t.Fatalf("%+v %v %v", d, ok, changed)
	}
	if _, _, changed = b.BeginOrchDelivery(1, t0); changed {
		t.Fatal("a repeat mark reports a change")
	}
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0.Add(time.Second))
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0.Add(2*time.Second))
	if got := len(b.Item(1).Orchestrator.Events); got != 2 {
		t.Fatalf("events %d: a new event merged into the one in flight, or a waiting one was added twice", got)
	}
	b.OrchDelivered(1, "o", d.Events, t0)
	o := b.Item(1).Orchestrator
	if len(o.Events) != 1 || !o.Events[0].At.Equal(t0.Add(time.Second)) || o.Sending != 0 {
		t.Fatalf("%+v", o)
	}
	if _, ok, _ := b.BeginOrchDelivery(4, t0); ok {
		t.Fatal("an unbound item has a delivery")
	}
}

func TestOrchDeliveryFailedEndsTheInFlightMark(t *testing.T) {
	b := boundGroup(t)
	b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0)
	b.BeginOrchDelivery(1, t0)
	b.OrchDeliveryFailed(1, "o", "x", t0)
	if b.Item(1).Orchestrator.Sending != 0 {
		t.Fatal("a failed delivery stays in flight")
	}
}

func TestRebindClosesTheEscalation(t *testing.T) {
	escalated := func() *Backlog {
		b := boundGroup(t)
		b.QueueOrchEvent(OrchEvent{Kind: EventStalled, Item: 3}, t0)
		for range OrchMaxFailures {
			b.OrchDeliveryFailed(1, "o", "x", t0)
		}
		if !b.OrchEscalated(1) {
			t.Fatal("not escalated")
		}
		return b
	}

	b := escalated()
	if err := b.OrchReplaced(1, "o", orch("n"), nil, t0); err != nil {
		t.Fatal(err)
	}
	if b.OrchEscalated(1) || len(b.OrchDeliveries(t0)) != 1 {
		t.Fatal("the escalation blocks the replacement")
	}
	if q := b.Item(1).Thread[0]; q.Closed != ClosedRebound {
		t.Fatalf("question %+v", q)
	}

	b = escalated()
	if err := b.BindOrchestrator(1, orch("n"), t0); err != nil || b.OrchEscalated(1) {
		t.Fatalf("bind: %v, escalated %v", err, b.OrchEscalated(1))
	}

	b = escalated()
	if _, err := b.ClearOrchestrator(1, t0); err != nil || b.OrchEscalated(1) || len(b.Inbox()) != 0 {
		t.Fatalf("clear: %v, inbox %+v", err, b.Inbox())
	}
}
