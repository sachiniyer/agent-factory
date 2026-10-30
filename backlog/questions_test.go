package backlog

import (
	"strings"
	"testing"
	"time"
)

// withRun sets item id running in session s1.
func withRun(t *testing.T, id int, status Status) {
	t.Helper()
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(id)
		it.Status = status
		it.Runs = []RunRef{{SessionID: "s1", Title: "B-1 A", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func ask(t *testing.T, s *Service, text string) Message {
	t.Helper()
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: text, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	return out.Message
}

func TestServiceAsk(t *testing.T) {
	s, h := newTestService(t)
	ids := mustAdd(t, s, ItemInput{Key: "p", Title: "P"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "p"}})
	withRun(t, ids["a"], StatusRunning)
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: "q", SessionID: "s2"}); err == nil {
		t.Error("a question from another session accepted")
	}
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindQuestion, Text: " Which db? ", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Message; m.Kind != KindQuestion || m.Author != "agent:s1" || m.Text != "Which db?" {
		t.Fatalf("%+v", m)
	}
	if h.attention["s1"] != "Which db?" {
		t.Fatalf("attention %q", h.attention["s1"])
	}
	v, err := s.Get(GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.States[2].State != StateNeedsYou || v.States[1].State != StateNeedsYou {
		t.Fatalf("item %v, parent %v", v.States[2], v.States[1])
	}
}

func TestServiceAskRefusesClosedItem(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusDone)
	_, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "q", SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "B-1 is done") {
		t.Fatal(err)
	}
}

func TestServiceAnswerDelivers(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	q := ask(t, s, "Which db?")
	h.target = TargetLive
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "Postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.Message; m.Kind != KindAnswer || m.Author != AuthorUser || m.Answers != q.ID {
		t.Fatalf("%+v", m)
	}
	if h.attention["s1"] != "" {
		t.Fatalf("attention not cleared: %q", h.attention["s1"])
	}
	want := PendingAnswer{ID: 1, AnswerID: out.Message.ID, SessionID: "s1", Prompt: "The user answered your question on B-1: Postgres"}
	if len(h.delivered) != 1 || h.delivered[0] != want {
		t.Fatalf("%+v", h.delivered)
	}
	b, _ := Load(repoID)
	it := b.Item(1)
	if it.Status != StatusRunning || it.OpenQuestion() != nil || it.Thread[0].Closed != ClosedAnswered {
		t.Fatalf("%+v", it)
	}
	if a := it.Thread[1]; a.Delivery != DeliveryPending || a.PrevStatus != StatusRunning {
		t.Fatalf("%+v", a)
	}
	if p := b.PendingAnswers(); len(p) != 1 || p[0] != want {
		t.Fatalf("pending %+v", p)
	}
	_, err = s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "again"})
	if err == nil || err.Error() != `B-1 has no open question — use "af backlog note" to add a note` {
		t.Fatal(err)
	}
}

func TestServiceAnswerResumesReviewAndRestores(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusReview)
	ask(t, s, "Merge it?")
	h.target = TargetRestore
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if len(h.delivered) != 1 {
		t.Fatalf("%+v", h.delivered)
	}
	b, _ := Load(repoID)
	if st := b.Item(1).Status; st != StatusRunning {
		t.Fatal(st)
	}
	if err := b.AnswerFailed(1, h.delivered[0].AnswerID, "no worktree", time.Now()); err != nil {
		t.Fatal(err)
	}
	it := b.Item(1)
	if it.Status != StatusReview || it.Thread[1].Delivery != DeliveryFailed || len(b.PendingAnswers()) != 0 {
		t.Fatalf("%+v", it)
	}
	if note := it.Thread[len(it.Thread)-1]; note.Author != AuthorAF || !strings.Contains(note.Text, "no worktree") {
		t.Fatalf("%+v", note)
	}
}

func TestServiceAnswerGoneKeepsAnswer(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	ask(t, s, "q")
	h.target = TargetGone
	_, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "a"})
	want := `the session for B-1 is gone; start it again with "af backlog start 1" — your answer is kept in the thread`
	if err == nil || err.Error() != want {
		t.Fatal(err)
	}
	if len(h.delivered) != 0 {
		t.Fatalf("%+v", h.delivered)
	}
	b, _ := Load(repoID)
	it := b.Item(1)
	if len(it.Thread) != 2 || it.Thread[1].Kind != KindAnswer || it.OpenQuestion() != nil {
		t.Fatalf("%+v", it.Thread)
	}
	if it.Status != StatusRunning {
		t.Fatal(it.Status)
	}
}

func TestServiceAnswerNamedQuestion(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	first := ask(t, s, "one")
	second := ask(t, s, "two")
	h.target = TargetLive
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "x", QuestionID: 99}); err == nil {
		t.Error("unknown question accepted")
	}
	out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "x", QuestionID: second.ID})
	if err != nil || out.Message.Answers != second.ID {
		t.Fatal(out, err)
	}
	if h.attention["s1"] != "one" {
		t.Fatalf("attention %q, want the question still open", h.attention["s1"])
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "x", QuestionID: second.ID}); err == nil {
		t.Error("an answered question accepted a second answer")
	}
	out, err = s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "y"})
	if err != nil || out.Message.Answers != first.ID || h.attention["s1"] != "" {
		t.Fatal(out, err, h.attention)
	}
}

func TestServiceMessageRefusals(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	for _, req := range []PostMessageRequest{
		{RepoID: repoID, ID: 1, Kind: "comment", Text: "x"},
		{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "x"},
		{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: " ", SessionID: "s1"},
		{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "x", SessionID: "s1", QuestionID: 1},
		{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "x", SessionID: "s1"},
		{RepoID: repoID, ID: 9, Kind: KindAnswer, Text: "x"},
	} {
		if _, err := s.PostMessage(req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
}

func TestServiceDoneClosesQuestion(t *testing.T) {
	for _, to := range []Status{StatusDone, StatusDropped} {
		t.Run(string(to), func(t *testing.T) {
			s, h := newTestService(t)
			mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
			withRun(t, 1, StatusRunning)
			ask(t, s, "q")
			if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: to}); err != nil {
				t.Fatal(err)
			}
			if a, ok := h.attention["s1"]; !ok || a != "" {
				t.Fatalf("attention %q %v", a, ok)
			}
			b, _ := Load(repoID)
			if c := b.Item(1).Thread[0].Closed; c != string(to) {
				t.Fatal(c)
			}
		})
	}
}

func TestAnsweredInSession(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "old", State: RunStarted}, {SessionID: "s1", State: RunStarted}}
	if got := b.AnsweredInSession("s1", now); got != 0 {
		t.Fatalf("closed %d with nothing open", got)
	}
	if _, err := b.Ask(1, "old", "q", now); err == nil {
		t.Fatal("an old run asked")
	}
	for _, q := range []string{"q", "r"} {
		if _, err := b.Ask(1, "s1", q, now); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.AnsweredInSession("old", now); got != 0 {
		t.Fatal("an old run closed the question")
	}
	if got := b.AnsweredInSession("s1", now); got != 1 {
		t.Fatal(got)
	}
	th := b.Item(1).Thread
	if th[0].Closed != ClosedAnsweredInSession || th[1].Closed != "" || b.OpenQuestionText(1) != "r" {
		t.Fatalf("only the oldest closes: %+v", th)
	}
}

func TestItemPromptSaysAsk(t *testing.T) {
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, time.Now())
	if p := b.ItemPrompt(it, ""); !strings.Contains(p, `af backlog ask --self`) {
		t.Fatal(p)
	}
}

func TestServiceAnswerWhileStarting(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	ask(t, s, "q")
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(1)
		it.Runs = append(it.Runs, RunRef{State: RunStarting})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.target = TargetLive
	_, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "a"})
	if err == nil || !strings.Contains(err.Error(), "starting a new session") {
		t.Fatal(err)
	}
	b, _ := Load(repoID)
	if len(b.Item(1).Thread) != 1 {
		t.Fatal("a refused answer was written")
	}
}

func TestAnswerDelivered(t *testing.T) {
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, time.Now())
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	if _, err := b.Ask(1, "s1", "q", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, err := b.Answer(1, 0, "a", true, time.Now())
	if err != nil || b.Item(1).Status != StatusRunning || out.Answer.PrevStatus != StatusReview {
		t.Fatal(out, err)
	}
	if err := b.AnswerDelivered(1, out.Answer.ID, time.Now()); err != nil || len(b.PendingAnswers()) != 0 {
		t.Fatal(err, b.PendingAnswers())
	}
}
