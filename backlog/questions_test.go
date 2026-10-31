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
	want := PendingAnswer{ID: 1, AnswerID: out.Message.ID, SessionID: "s1", Prompt: "The user answered your question on B-1 (\"Which db?\"): Postgres"}
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

// One answer's failure cannot roll the item back over work another answer started
// (Codex P2 on #6295): two answers accepted on an item in review, the second
// delivered, then the first fails.
func TestAnswerFailedKeepsRunningForANewerAnswer(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	it.LiveAnswers = 2
	it.Thread = []Message{
		{ID: 1, Kind: KindAnswer, Answers: 0, Delivery: DeliveryPending, PrevStatus: StatusReview},
		{ID: 2, Kind: KindAnswer, Answers: 0, Delivery: DeliveryDelivered, PrevStatus: StatusRunning},
	}
	if err := b.AnswerFailed(1, 1, "pane gone", now); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusRunning {
		t.Fatalf("status = %s: the first answer's rollback overwrote the work the second started", b.Item(1).Status)
	}
	// With no newer answer, the rollback still happens.
	b.Item(1).LiveAnswers = 1
	b.Item(1).Thread = []Message{{ID: 1, Kind: KindAnswer, Delivery: DeliveryPending, PrevStatus: StatusReview}}
	if err := b.AnswerFailed(1, 1, "pane gone", now); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusReview {
		t.Fatalf("status = %s, want the replaced status back", b.Item(1).Status)
	}
}

// Two answers accepted on an item in review, both failing in order, give the item
// back to review (Codex P2 on #6295): the second answer keeps the status before
// the oldest pending answer, not the "running" the first one set.
func TestQueuedAnswersAllFailingRestoreTheOriginalStatus(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	for _, q := range []string{"one?", "two?"} {
		if _, err := b.Ask(1, "s1", q, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := b.Answer(1, 0, "a", true, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Answer(1, 0, "b", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Answer.PrevStatus != StatusReview {
		t.Fatalf("the second answer saved %q, want the status before the oldest pending answer", second.Answer.PrevStatus)
	}
	if err := b.AnswerFailed(1, first.Answer.ID, "pane gone", now); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusRunning {
		t.Fatalf("status = %s: the second answer is still pending", b.Item(1).Status)
	}
	if err := b.AnswerFailed(1, second.Answer.ID, "pane gone", now); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusReview {
		t.Fatalf("status = %s, want review once every answer failed", b.Item(1).Status)
	}
}

// The terminal binding closes the question the user SAW (its text, read from the
// session's attention before the line was forwarded), not whichever is oldest when
// the worker runs (Codex P2 on #6295).
func TestAnsweredInSessionTextClosesOnlyTheSeenQuestion(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	if _, err := b.Ask(1, "s1", "first?", now); err != nil {
		t.Fatal(err)
	}
	if got := b.AnsweredInSessionText("s1", 0, "second?", nil, now); got != 0 {
		t.Fatalf("closed %d for a question the user did not see", got)
	}
	if got := b.AnsweredInSessionText("s1", 0, "", nil, now); got != 0 {
		t.Fatal("no seen question must close nothing")
	}
	if _, err := b.Ask(1, "s1", "second?", now); err != nil {
		t.Fatal(err)
	}
	if got := b.AnsweredInSessionText("s1", 0, "second?", nil, now); got != 0 {
		t.Fatal("only the oldest open question can have been answered by a line")
	}
	if got := b.AnsweredInSessionText("s1", 0, "first?", nil, now); got != 1 {
		t.Fatalf("got %d", got)
	}
	if b.OpenQuestionText(1) != "second?" {
		t.Fatalf("the newer question must stay open, got %q", b.OpenQuestionText(1))
	}
}

// The reverse order: the first answer is delivered and the second fails. The agent
// is still working on the first, so the item stays running (Codex P2 on #6295).
func TestALaterAnswerFailingKeepsEarlierDeliveredWorkRunning(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	for _, q := range []string{"one?", "two?"} {
		if _, err := b.Ask(1, "s1", q, now); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := b.Answer(1, 0, "a", true, now)
	second, _ := b.Answer(1, 0, "b", true, now)
	if err := b.AnswerDelivered(1, first.Answer.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := b.AnswerFailed(1, second.Answer.ID, "pane gone", now); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusRunning {
		t.Fatalf("status = %s: the delivered first answer's work was rolled back over", b.Item(1).Status)
	}
	// Once the run ends the count resets, so a later answer batch rolls back normally.
	if end := b.RunEnded("s1", now); !end.Changed || b.Item(1).LiveAnswers != 0 {
		t.Fatalf("run end: %+v live answers %d", end, b.Item(1).LiveAnswers)
	}
}

// The attention mirror is capped; the terminal binding compares in that
// normalization (Codex P2 on #6295).
func TestAnsweredInSessionTextMatchesACappedQuestion(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	long := strings.Repeat("why? ", 200)
	if _, err := b.Ask(1, "s1", long, now); err != nil {
		t.Fatal(err)
	}
	capped := func(s string) string {
		if r := []rune(s); len(r) > 500 {
			return string(r[:500])
		}
		return s
	}
	if got := b.AnsweredInSessionText("s1", 0, capped(long), nil, now); got != 0 {
		t.Fatal("without the normalization the capped text cannot match")
	}
	if got := b.AnsweredInSessionText("s1", 0, capped(long), capped, now); got != 1 {
		t.Fatalf("a capped question the user saw was not closed: %d", got)
	}
}

// A pending answer whose session was killed, with a replacement run started before
// its failure is recorded, rolls nothing back (Codex P2 on #6298): the status
// belongs to the replacement now.
func TestAnswerFailedAfterAReplacementRunLeavesTheItemRunning(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"}}
	ans, err := b.Answer(1, 0, "Postgres", true, now)
	if err != nil {
		t.Fatal(err)
	}
	// The session is killed and the user starts a replacement before the worker
	// records the failure.
	if _, err := b.BeginStart(1, StartedByUser, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.FinishStart(1, now.Add(time.Second), "s2", "A-2", nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.AnswerFailed(1, ans.Answer.ID, "the session is gone", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := b.Item(1).Status; got != StatusRunning {
		t.Fatalf("status = %s: the failed answer's rollback overwrote the replacement run", got)
	}
}

// A held answer accepted on an item in review, then a continue started before the
// answer fails, keeps the continued work running (Codex P2 on #6295).
func TestAnswerFailedAfterAContinueLeavesTheItemRunning(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"}}
	ans, err := b.Answer(1, 0, "Postgres", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Continued(1, "s1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.AnswerFailed(1, ans.Answer.ID, "pane gone", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := b.Item(1).Status; got != StatusRunning {
		t.Fatalf("status = %s: the failed answer overwrote the continued run", got)
	}
}

// A pending answer belongs to the run that accepted it (its RunCount), so after a
// restart it is never retargeted at a replacement run (Codex P2 on #6297).
func TestPendingAnswerStaysWithTheRunThatAcceptedIt(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "old", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"}}
	if _, err := b.Answer(1, 0, "Postgres", true, now); err != nil {
		t.Fatal(err)
	}
	if p := b.PendingAnswers(); len(p) != 1 || p[0].SessionID != "old" || p[0].Superseded {
		t.Fatalf("before the replacement: %+v", p)
	}
	// The old session is killed and the item started again before the failure was recorded.
	it.Runs = append(it.Runs, RunRef{SessionID: "new", State: RunStarted, StartedAt: now.Add(time.Minute)})
	p := b.PendingAnswers()
	if len(p) != 1 || p[0].SessionID != "old" || !p[0].Superseded {
		t.Fatalf("after the replacement the answer must stay with the old run and be superseded: %+v", p)
	}
}

// An answer accepted under an earlier run does not lend its saved status to an
// answer in the replacement run (Codex P2 on #6295).
func TestAnswerDoesNotInheritAnEarlierRunsRollbackStatus(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"}}
	if _, err := b.Answer(1, 0, "Postgres", true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.BeginStart(1, StartedByUser, nil, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.FinishStart(1, now.Add(time.Second), "s2", "A-2", nil, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	q, _ := b.AddNote(1, AuthorAF, "ignored", now)
	_ = q
	it.Thread = append(it.Thread, Message{ID: 90, Kind: KindQuestion, Author: AuthorAF, Text: "Which cache?"})
	ans, err := b.Answer(1, 90, "Redis", true, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AnswerFailed(1, ans.Answer.ID, "pane gone", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := b.Item(1).Status; got != StatusRunning {
		t.Fatalf("status = %s: the replacement run's answer rolled back to the earlier run's status", got)
	}
}

// Two open questions that share their first 500 runes are told apart by id: a
// second submission bound to the first question does not close the second
// (Codex P2 on #6295).
func TestAnsweredInSessionBindsToTheQuestionIDNotItsText(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	long := strings.Repeat("x", 600)
	it.Thread = []Message{
		{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: long + "A"},
		{ID: 2, Kind: KindQuestion, Author: AuthorAF, Text: long + "B"},
	}
	if got := b.AnsweredInSessionText("s1", 1, "", nil, now); got != 1 {
		t.Fatalf("first submission: %d", got)
	}
	if got := b.AnsweredInSessionText("s1", 1, "", nil, now); got != 0 {
		t.Fatalf("the second submission bound to question 1 closed question 2: %d", got)
	}
	if q := b.Item(1).OpenQuestion(); q == nil || q.ID != 2 {
		t.Fatalf("question 2 must stay open: %+v", q)
	}
}

// A session replaced between the service reading the item and its transaction
// refuses the answer and leaves the question open for the retry (Codex P2 on
// #6295), rather than closing it against a stale gone-session target.
func TestServiceAnswerRefusesWhenTheSessionChangedUnderIt(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	withRun(t, 1, StatusRunning)
	ask(t, s, "Which db?")
	h.target = TargetGone // the killed session
	h.onTarget = func() { // ...replaced before the transaction
		if _, err := Update(repoID, func(b *Backlog) error {
			b.Item(1).Runs = append(b.Item(1).Runs, RunRef{SessionID: "s2", Title: "B-1 A", State: RunStarted})
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	_, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindAnswer, Text: "Postgres"})
	if err == nil || !strings.Contains(err.Error(), "changed while answering") {
		t.Fatalf("err = %v, want the retry error", err)
	}
	b, _ := Load(repoID)
	if b.Item(1).OpenQuestion() == nil {
		t.Fatal("the question was closed without an answer being delivered")
	}
}

// An answer accepted under an earlier run does not hold back the replacement
// run's completion (Codex P2 on #6299).
func TestRunEndedIgnoresAnAnswerPendingForAnEarlierRun(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"}}
	if _, err := b.Answer(1, 0, "Postgres", true, now); err != nil {
		t.Fatal(err)
	}
	it.Runs = append(it.Runs, RunRef{SessionID: "s2", State: RunStarted, StartedAt: now.Add(time.Minute)})
	it.Status = StatusRunning
	if end := b.RunEnded("s2", now.Add(2*time.Minute)); !end.Changed || b.Item(1).Status != StatusReview {
		t.Fatalf("the replacement's completion was held back by the old run's answer: %+v %s", end, b.Item(1).Status)
	}
}

// Answer(first), the user marks the item for review, Answer(second), then the
// first fails: the first's failure must not decrement the second's live count or
// put the status it replaced over the second's work (Codex P2 on #6295).
func TestAnswerFailedAfterAStatusResetLeavesALaterAnswersWorkRunning(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: now.Add(-time.Hour)}}
	it.Thread = []Message{
		{ID: 1, Kind: KindQuestion, Author: AuthorAF, Text: "Which db?"},
		{ID: 2, Kind: KindQuestion, Author: AuthorAF, Text: "Which cache?"},
	}
	first, err := b.Answer(1, 1, "Postgres", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetStatus(1, StatusReview, ActorUser, func(*Item) bool { return false }, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := b.Answer(1, 2, "Redis", true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Item(1).Thread[len(b.Item(1).Thread)-1].PrevStatus; got != StatusReview {
		t.Fatalf("second answer saved %q: it inherited the first answer's rollback status", got)
	}
	if err := b.AnswerFailed(1, first.Answer.ID, "the session is gone", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := b.Item(1).Status; got != StatusRunning {
		t.Fatalf("status = %s: the first answer's failure restored review over the second answer's work", got)
	}
	if got := b.Item(1).LiveAnswers; got != 1 {
		t.Fatalf("LiveAnswers = %d, want the second answer's 1", got)
	}
	if err := b.AnswerDelivered(1, second.Answer.ID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
}
