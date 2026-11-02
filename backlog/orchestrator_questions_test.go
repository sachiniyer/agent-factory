package backlog

import (
	"reflect"
	"strings"
	"testing"
)

func TestAskRoutesToOrchestrator(t *testing.T) {
	b := eventsGroup(t)
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil || q.Route != RouteOrchestrator {
		t.Fatalf("%+v %v", q, err)
	}
	if q, err = b.Ask(6, "s6", Question{Text: "unbound"}, t0); err != nil || q.Route != "" {
		t.Fatalf("unbound group routed: %+v %v", q, err)
	}
	inbox := b.Inbox()
	if len(inbox) != 1 || inbox[0].ID != 6 {
		t.Fatalf("inbox %+v", inbox)
	}
	if got := userText(b, 2); got != "" {
		t.Fatalf("attention text for a routed question %q", got)
	}
	states := b.DeriveAll(nil)
	if s := states[2]; s.State != StateStopped || s.Reason != WithEffortSessionReason {
		t.Fatalf("routed item %+v", s)
	}
	if s := states[1]; s.State == StateNeedsYou {
		t.Fatalf("group needs you for a routed question: %+v", s)
	}
	if s := states[6]; s.State != StateNeedsYou {
		t.Fatalf("unbound item %+v", s)
	}
}

func TestEscalate(t *testing.T) {
	b := eventsGroup(t)
	if _, err := b.Escalate(2, "", "o", t0); err == nil || !strings.Contains(err.Error(), "B-2 has no open question to escalate") {
		t.Fatalf("no question: %v", err)
	}
	if _, err := b.Ask(2, "s2", Question{Text: "which?"}, t0); err != nil {
		t.Fatal(err)
	}
	q, err := b.Escalate(2, " you pick ", "o", t0)
	if err != nil || q.Route != RouteUser || q.ID != 1 {
		t.Fatalf("%+v %v", q, err)
	}
	th := b.Item(2).Thread
	if len(th) != 2 || th[1].Kind != KindNote || th[1].Author != "agent:o" || th[1].Text != "you pick" {
		t.Fatalf("thread %+v", th)
	}
	if inbox := b.Inbox(); len(inbox) != 1 || inbox[0].ID != 2 {
		t.Fatalf("inbox %+v", inbox)
	}
	if userText(b, 2) != "which?" || b.Derive(2, nil).State != StateNeedsYou {
		t.Fatal("escalated question is not with the user")
	}
	if _, err := b.Escalate(2, "", "o", t0); err == nil || !strings.Contains(err.Error(), "already with the user") {
		t.Fatalf("twice: %v", err)
	}
	if _, err := b.Ask(6, "s6", Question{Text: "unbound"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Escalate(6, "", "", t0); err == nil || !strings.Contains(err.Error(), "already with the user") {
		t.Fatalf("unbound group: %v", err)
	}
}

func TestEscalateStranded(t *testing.T) {
	b := eventsGroup(t)
	if _, err := b.Ask(2, "s2", Question{Text: "which?"}, t0); err != nil {
		t.Fatal(err)
	}
	if got := b.escalateStranded(b.clone(), t0); got != nil {
		t.Fatalf("bound group escalated %v", got)
	}
	if _, err := b.ClearOrchestrator(1, t0); err != nil {
		t.Fatal(err)
	}
	if got := b.Inbox(); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("an unbound group's question is not in the inbox: %+v", got)
	}
	if got := b.escalateStranded(nil, t0); len(got) != 1 || got[0] != 2 {
		t.Fatalf("escalated %v", got)
	}
	if r := b.Item(2).Thread[0].Route; r != RouteUser {
		t.Fatalf("route %q", r)
	}
	if got := b.escalateStranded(nil, t0); got != nil {
		t.Fatalf("twice %v", got)
	}
}

func TestServiceRoutedQuestionAttention(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(2)
		it.Status = StatusRunning
		it.Runs = []RunRef{{SessionID: "s2", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: "which?", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.attention["s2"]; ok {
		t.Fatalf("routed question set attention %q", h.attention["s2"])
	}
	out, err := s.Escalate(EscalateRequest{RepoID: repoID, ID: 2, Note: "over to you"})
	if err != nil || out.Question.Route != RouteUser {
		t.Fatalf("%+v %v", out, err)
	}
	if h.attention["s2"] != "which?" {
		t.Fatalf("attention %q", h.attention["s2"])
	}
	if got := h.last(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("changed %v", got)
	}
}

func TestServiceClearEscalatesRoutedQuestions(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(2)
		it.Status = StatusRunning
		it.Runs = []RunRef{{SessionID: "s2", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: "which?", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, Clear: true}); err != nil {
		t.Fatal(err)
	}
	if h.attention["s2"] != "which?" {
		t.Fatalf("attention %q", h.attention["s2"])
	}
	b, _ := Load(repoID)
	if r := b.Item(2).Thread[0].Route; r != RouteUser {
		t.Fatalf("route %q", r)
	}
	if got := h.last(); len(got) != 2 {
		t.Fatalf("changed %v", got)
	}
}

func TestServiceOrchestratorDoneNote(t *testing.T) {
	s, _ := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}},
		ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		b.Item(2).Status, b.Item(2).PR = StatusReview, "https://x/pull/1"
		b.Item(3).Status = StatusReview
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 3, Status: StatusDone, CallerSessionID: "o2"}); err != nil {
		t.Fatal(err)
	}
	out, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 2, Status: StatusDone, CallerSessionID: "o1"})
	if err != nil {
		t.Fatal(err)
	}
	th := out.Item.Thread
	if len(th) != 1 || !th[0].Inbox || th[0].Kind != KindNote || th[0].Author != "agent:o1" ||
		th[0].Text != OrchestratorDoneNote("https://x/pull/1") {
		t.Fatalf("thread %+v", th)
	}
	b, _ := Load(repoID)
	if notes := b.InboxNotes(); len(notes) != 1 || notes[0].ID != 2 {
		t.Fatalf("inbox notes %+v", notes)
	}
	if b.Item(3).Thread != nil {
		t.Fatalf("a session that does not orchestrate added a note: %+v", b.Item(3).Thread)
	}
}

func TestEscalateNoteAuthor(t *testing.T) {
	b := eventsGroup(t)
	if _, err := b.Ask(2, "s2", Question{Text: "which?"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Escalate(2, "from the user", "", t0); err != nil {
		t.Fatal(err)
	}
	if th := b.Item(2).Thread; th[1].Author != AuthorUser {
		t.Fatalf("note author %q", th[1].Author)
	}
	b = eventsGroup(t)
	if _, err := b.Ask(2, "s2", Question{Text: "which?"}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Escalate(2, "from another session", "x", t0); err != nil {
		t.Fatal(err)
	}
	if th := b.Item(2).Thread; th[1].Author != AuthorUser {
		t.Fatalf("note author %q", th[1].Author)
	}
}

// movedBetweenGroups binds group 5 to "p", asks on 2 in group 1, sets the
// question's route, and moves 2 into group 5.
func movedBetweenGroups(t *testing.T, route string) (*Backlog, *Backlog) {
	t.Helper()
	b := eventsGroup(t)
	if err := b.BindOrchestrator(5, orch("p"), t0); err != nil {
		t.Fatal(err)
	}
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	q.Route = route
	prev := b.clone()
	b.Item(2).Parent = 5
	return b, prev
}

func TestEscalateStrandedMovedBetweenGroups(t *testing.T) {
	for _, route := range []string{RouteOrchestrator, RouteUser} {
		b, prev := movedBetweenGroups(t, route)
		if got := b.escalateStranded(prev, t0); len(got) != 1 || got[0] != 2 {
			t.Fatalf("%s: escalated %v", route, got)
		}
		if r := b.Item(2).Thread[0].Route; r != "" {
			t.Fatalf("%s: route %q", route, r)
		}
		if got := b.Inbox(); len(got) != 1 || got[0].ID != 2 {
			t.Fatalf("%s: inbox %+v", route, got)
		}
		if userText(b, 2) != "which?" {
			t.Fatalf("%s: no attention text", route)
		}
		prev = b.clone()
		if _, err := b.Answer(2, 0, "this one", t0); err != nil {
			t.Fatal(err)
		}
		if got := b.queueOrchChanges(prev, t0); got != nil {
			t.Fatalf("%s: the new group heard of a question it never had: %v %+v", route, got, kinds(b, 5))
		}
	}
}

func TestServiceMoveBetweenGroupsSendsQuestionToUser(t *testing.T) {
	s, h := orchService(t)
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}},
		ItemInput{Key: "h", Title: "H"}, ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "h"}},
		ItemInput{Key: "c", Title: "C", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(2)
		it.Status = StatusRunning
		it.Runs = []RunRef{{SessionID: "s2", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for id, sid := range map[int]string{1: "o1", 3: "o2"} {
		if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: id, SessionID: sid}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: "which?", SessionID: "s2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(MoveRequest{RepoID: repoID, ID: 2, Into: 3}); err != nil {
		t.Fatal(err)
	}
	if h.attention["s2"] != "which?" {
		t.Fatalf("attention %q", h.attention["s2"])
	}
	b, _ := Load(repoID)
	if got := b.Inbox(); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("inbox %+v", got)
	}
	if ev := b.Item(3).Orchestrator.Events; len(ev) != 0 {
		t.Fatalf("events for the new group %+v", ev)
	}
}

// userText is the text of item id's question with the user, the one its
// session's attention shows, or "".
func userText(b *Backlog, id int) string {
	_, text := b.AttentionRef(id)
	return text
}

// A question routed to the orchestrator was never on the session's attention,
// so a line typed in the item session cannot answer it; once escalated it can.
func TestRoutedQuestionIsNotAnsweredInSession(t *testing.T) {
	b := eventsGroup(t)
	q, err := b.Ask(2, "s2", Question{Text: "which?"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if id := b.AnsweredInSessionText("s2", q.ID, "", nil, t0); id != 0 {
		t.Fatalf("closed by id on %d", id)
	}
	if id := b.AnsweredInSessionText("s2", 0, "which?", nil, t0); id != 0 {
		t.Fatalf("closed by text on %d", id)
	}
	if !b.Item(2).questionOpen(q.ID) {
		t.Fatal("the routed question closed")
	}
	if _, err := b.Escalate(2, "", "o", t0); err != nil {
		t.Fatal(err)
	}
	b.Item(1).Orchestrator.Events = nil
	prev := b.clone()
	if id := b.AnsweredInSessionText("s2", q.ID, "", nil, t0); id != 2 {
		t.Fatalf("escalated question not answered in session: %d", id)
	}
	b.queueOrchChanges(prev, t0)
	want := []OrchEvent{{Kind: EventEscalationAnswered, Item: 2, Message: q.ID}}
	if got := kinds(b, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// Attention says 0 only when no question is left (Codex P2 on #6475: the host
// keeps a session's archived-for-answer mark while any question is open).
// Answering the escalated question while a routed one is still open clears
// the text but keeps that question's id.
func TestAnswerKeepsTheWaitWhileARoutedQuestionIsOpen(t *testing.T) {
	s, h := orchService(t)
	h.target = TargetLive
	mustAdd(t, s, ItemInput{Key: "g", Title: "G"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "g"}})
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(2)
		it.Status = StatusRunning
		it.Runs = []RunRef{{SessionID: "s2", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetOrchestrator(SetOrchestratorRequest{RepoID: repoID, ID: 1, SessionID: "o1"}); err != nil {
		t.Fatal(err)
	}
	ask := func(text string) int {
		t.Helper()
		out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: text, SessionID: "s2"})
		if err != nil {
			t.Fatal(err)
		}
		return out.Message.ID
	}
	first := ask("which db?")
	if _, err := s.Escalate(EscalateRequest{RepoID: repoID, ID: 2}); err != nil {
		t.Fatal(err)
	}
	second := ask("and the port?")
	if h.attention["s2"] != "which db?" || h.attentionID["s2"] != first {
		t.Fatalf("attention %q #%d", h.attention["s2"], h.attentionID["s2"])
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindAnswer, Text: "postgres", QuestionID: first}); err != nil {
		t.Fatal(err)
	}
	if h.attention["s2"] != "" || h.attentionID["s2"] != second {
		t.Fatalf("after the answer: attention %q #%d; want no text and #%d, the routed question still open", h.attention["s2"], h.attentionID["s2"], second)
	}
}
