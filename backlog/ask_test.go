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

func TestAskKeepsThenAndRefusesAnUnknownOne(t *testing.T) {
	b := askedRun(t)
	m, err := b.Ask(1, "s1", Question{Text: "which db?", Then: ThenArchive}, time.Now())
	if err != nil || m.Then != ThenArchive {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := b.Ask(1, "s1", Question{Text: "and?", Then: "later"}, time.Now()); err == nil || err.Error() != `then "later" is not wait or archive` {
		t.Fatalf("an unknown then: %v", err)
	}
}

// The archive after asking is owed while the session is the item's latest run,
// the item is open, every open question asked for it, and no answer is on its
// way; any one of those failing keeps the session live.
func TestArchiveOwedForAsk(t *testing.T) {
	now := time.Now()
	ask := func(b *Backlog, then string) *Message {
		m, err := b.Ask(1, "s1", Question{Text: "q?", Then: then}, now)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	cases := []struct {
		name string
		edit func(b *Backlog)
		want bool
	}{
		{"no question", func(b *Backlog) {}, false},
		{"asked with then archive", func(b *Backlog) { ask(b, ThenArchive) }, true},
		{"asked with then wait", func(b *Backlog) { ask(b, ThenWait) }, false},
		{"asked before then existed", func(b *Backlog) { ask(b, "") }, false},
		{"one of two asked with then wait", func(b *Backlog) { ask(b, ThenArchive); ask(b, ThenWait) }, false},
		{"both asked with then archive", func(b *Backlog) { ask(b, ThenArchive); ask(b, ThenArchive) }, true},
		{"answered", func(b *Backlog) {
			q := ask(b, ThenArchive)
			b.Item(1).appendMessage(Message{Kind: KindAnswer, Answers: q.ID, Text: "pg", Delivery: DeliveryPending, RunCount: 1}, now)
		}, false},
		{"an answer to an earlier question on its way", func(b *Backlog) {
			q := ask(b, ThenWait)
			b.Item(1).appendMessage(Message{Kind: KindAnswer, Answers: q.ID, Text: "pg", Delivery: DeliveryPending, RunCount: 1}, now)
			ask(b, ThenArchive)
		}, false},
		{"done", func(b *Backlog) { ask(b, ThenArchive); b.Item(1).Status = StatusDone }, false},
		{"started again in another session", func(b *Backlog) {
			ask(b, ThenArchive)
			b.Item(1).Runs = append(b.Item(1).Runs, RunRef{SessionID: "s2", State: RunStarted})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := askedRun(t)
			tc.edit(b)
			if got := b.ArchiveOwed("s1", ArchiveAsk); got != tc.want {
				t.Fatalf("ArchiveOwed = %v, want %v", got, tc.want)
			}
			if b.ArchiveOwed("s1", ArchiveDone) {
				t.Fatal("an open item owes no on_done archive")
			}
		})
	}
}

// RunEnded leaves the item running on an open question, as before, and asks
// for the archive only when the questions did.
func TestRunEndedAsksForTheArchiveTheQuestionsAskedFor(t *testing.T) {
	for _, then := range []string{ThenArchive, ThenWait} {
		b := askedRun(t)
		if _, err := b.Ask(1, "s1", Question{Text: "q?", Then: then}, time.Now()); err != nil {
			t.Fatal(err)
		}
		end := b.RunEnded("s1", time.Now())
		if want := then == ThenArchive; end.Archive != want || end.Changed || b.Item(1).Status != StatusRunning {
			t.Fatalf("then %s: %+v, status %s", then, end, b.Item(1).Status)
		}
		if end.Archive && end.Reason != ArchiveAsk {
			t.Fatalf("reason %q", end.Reason)
		}
	}
}

// A refused archive leaves a note and turns the questions to then wait, so
// nothing tries again; once the archive is no longer owed it changes nothing.
func TestKeptLive(t *testing.T) {
	b := askedRun(t)
	if _, err := b.Ask(1, "s1", Question{Text: "q?", Then: ThenArchive}, time.Now()); err != nil {
		t.Fatal(err)
	}
	id, err := b.KeptLive("s1", ArchiveAsk, "the session changed after its run ended", time.Now())
	if err != nil || id != 1 {
		t.Fatalf("KeptLive = %d, %v", id, err)
	}
	it := b.Item(1)
	if last := it.Thread[len(it.Thread)-1]; last.Kind != KindNote || last.Text != "Kept the session live: the session changed after its run ended." {
		t.Fatalf("note %+v", last)
	}
	if q := it.OpenQuestion(); q.Then != ThenWait {
		t.Fatalf("then %q", q.Then)
	}
	n := len(it.Thread)
	if id, err := b.KeptLive("s1", ArchiveAsk, "again", time.Now()); err != nil || id != 0 || len(b.Item(1).Thread) != n {
		t.Fatalf("a second KeptLive: %d %v, thread %d→%d", id, err, n, len(b.Item(1).Thread))
	}
}

func TestServiceThenGoesWithAQuestion(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	for _, kind := range []string{KindNote, KindAnswer} {
		if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: kind, Text: "x", Then: ThenArchive}); err == nil || err.Error() != "then goes with a question" {
			t.Fatalf("%s with then: %v", kind, err)
		}
	}
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "q", SessionID: "s1", Then: ThenArchive})
	if err != nil || out.Message.Then != ThenArchive {
		t.Fatalf("%+v %v", out, err)
	}
}
