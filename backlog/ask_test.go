package backlog

import (
	"strings"
	"testing"
	"time"
)

func askedRun(t *testing.T) *Backlog {
	t.Helper()
	b := New()
	it, err := b.Add(NewItem{Title: "A"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	return b
}

func TestAskKeepsTheHandoffAndChecksItsSize(t *testing.T) {
	b := askedRun(t)
	m, err := b.Ask(1, "s1", Question{Text: "which db?", Handoff: "  tried sqlite; see notes.md  \n"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Handoff != "tried sqlite; see notes.md" {
		t.Fatalf("handoff %q", m.Handoff)
	}
	big := strings.Repeat("x", MaxHandoffBytes+1)
	if _, err := b.Ask(1, "s1", Question{Text: "and?", Handoff: big}, time.Now()); err == nil || !strings.Contains(err.Error(), "the limit is 8 KB") {
		t.Fatalf("an oversized handoff: %v", err)
	}
	if _, err := b.Ask(1, "s1", Question{Text: "and?", Handoff: big[1:]}, time.Now()); err != nil {
		t.Fatalf("a handoff at the limit: %v", err)
	}
	// Master keeps several open questions per item (Codex P2s on #6295); the
	// fork's one-question rule is not ported (decided on #6284).
	if got := len(b.Item(1).openQuestions()); got != 2 {
		t.Fatalf("open questions: %d", got)
	}
}

func TestInbox(t *testing.T) {
	now := time.Now()
	b := New()
	for _, title := range []string{"G", "A", "B"} {
		if _, err := b.Add(NewItem{Title: title}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Add(NewItem{Title: "C", Parent: 1}, now); err != nil {
		t.Fatal(err)
	}
	b.Item(3).appendMessage(Message{Kind: KindQuestion, Text: "b1"}, now.Add(1*time.Minute))
	b.Item(2).appendMessage(Message{Kind: KindQuestion, Text: "a1"}, now.Add(2*time.Minute))
	b.Item(3).appendMessage(Message{Kind: KindQuestion, Text: "b2"}, now.Add(3*time.Minute))
	b.Item(2).appendMessage(Message{Kind: KindQuestion, Text: "closed", Closed: ClosedAnswered}, now)
	b.Item(4).appendMessage(Message{Kind: KindQuestion, Text: "hidden"}, now)
	b.Item(1).Archived = true
	var got []string
	for _, e := range b.Inbox() {
		got = append(got, FormatID(e.ID)+" "+e.Question.Text)
	}
	if strings.Join(got, ", ") != "B-3 b1, B-2 a1, B-3 b2" {
		t.Fatal(got)
	}
	if New().Inbox() == nil {
		t.Fatal("an empty inbox is nil")
	}
}

// A handoff is a question's notes; on a note or an answer it is refused, and a
// question keeps it through the service.
func TestServiceHandoffGoesWithAQuestion(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	for _, kind := range []string{KindNote, KindAnswer} {
		if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: kind, Text: "x", Handoff: "notes"}); err == nil || err.Error() != "a handoff goes with a question" {
			t.Fatalf("%s with a handoff: %v", kind, err)
		}
	}
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "q", SessionID: "s1", Handoff: "notes"})
	if err != nil || out.Message.Handoff != "notes" {
		t.Fatalf("%+v %v", out, err)
	}
}
