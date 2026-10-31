package backlog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func runsBacklog(t *testing.T) *Backlog {
	t.Helper()
	b := New()
	for _, title := range []string{"Parent", "Child a/b", "Other"} {
		if _, err := b.Add(NewItem{Title: title}, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Move(2, Move{Into: 1}, t0); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBeginStartNewRun(t *testing.T) {
	b := runsBacklog(t)
	plan, err := b.BeginStart(2, StartedByUser, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	it := b.Item(2)
	if plan.Continue != "" || it.Status != StatusRunning || len(it.Runs) != 1 {
		t.Fatalf("plan %+v item %+v", plan, it)
	}
	r := it.Runs[0]
	if r.State != RunStarting || r.PrevStatus != StatusDraft || r.StartedBy != StartedByUser || !r.StartedAt.Equal(t0) {
		t.Fatalf("run %+v", r)
	}
	if _, err := b.BeginStart(2, StartedByUser, nil, t0); err == nil || !strings.Contains(err.Error(), "already starting") {
		t.Fatalf("second start: %v", err)
	}
}

func TestBeginStartRefusals(t *testing.T) {
	b := runsBacklog(t)
	if _, err := b.BeginStart(1, StartedByUser, nil, t0); err == nil || !strings.Contains(err.Error(), "is a parent") {
		t.Fatalf("parent: %v", err)
	}
	b.Item(3).Status = StatusDone
	if _, err := b.BeginStart(3, StartedByUser, nil, t0); err == nil || !strings.Contains(err.Error(), "reopen") {
		t.Fatalf("done: %v", err)
	}
	if _, err := b.BeginStart(9, StartedByUser, nil, t0); err == nil {
		t.Fatal("unknown id started")
	}
}

func TestBeginStartLiveSession(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(2)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", Title: "B-2 Child", State: RunStarted}}

	active := stubFacts(map[string]SessionFacts{"s1": {Live: true, RunActive: true}})
	if _, err := b.BeginStart(2, StartedByUser, active, t0); err == nil || !strings.Contains(err.Error(), `already working in session "B-2 Child"`) {
		t.Fatalf("active: %v", err)
	}

	live := stubFacts(map[string]SessionFacts{"s1": {Live: true}})
	plan, err := b.BeginStart(2, StartedByUser, live, t0)
	if err != nil || plan.Continue != "s1" {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if it.Status != StatusReview || len(it.Runs) != 1 {
		t.Fatal("a continue changes nothing before the prompt is sent")
	}
	if err := b.Continued(2, "s1", t0); err != nil || it.Status != StatusRunning || len(it.Runs) != 1 {
		t.Fatalf("continued: %v %+v", err, it)
	}
	if err := b.Continued(2, "other", t0); err == nil {
		t.Fatal("continued with a session that is not the latest run")
	}

	gone := stubFacts(map[string]SessionFacts{"s1": {Reason: "session archived"}})
	it.Status = StatusRunning
	plan, err = b.BeginStart(2, StartedByRunner, gone, t0)
	if err != nil || plan.Continue != "" || len(it.Runs) != 2 || it.Runs[1].PrevStatus != StatusRunning || it.Runs[1].StartedBy != StartedByRunner {
		t.Fatalf("stopped item starts a new session: %+v %v %+v", plan, err, it.Runs)
	}
}

func TestFinishStart(t *testing.T) {
	b := runsBacklog(t)
	plan, _ := b.BeginStart(2, StartedByUser, nil, t0)
	if err := b.FinishStart(2, plan.Run.StartedAt, "s1", "B-2 Child a b", nil, t0); err != nil {
		t.Fatal(err)
	}
	r := b.Item(2).Runs[0]
	if r.State != RunStarted || r.SessionID != "s1" || r.Title != "B-2 Child a b" {
		t.Fatalf("run %+v", r)
	}
	if err := b.FinishStart(2, plan.Run.StartedAt, "s1", "", nil, t0); err == nil {
		t.Fatal("finished a run that is not starting")
	}
}

func TestFinishStartFailure(t *testing.T) {
	b := runsBacklog(t)
	b.Item(3).Status = StatusQueued
	plan, _ := b.BeginStart(3, StartedByRunner, nil, t0)
	if err := b.FinishStart(3, plan.Run.StartedAt, "", "", errors.New("boom"), t0); err != nil {
		t.Fatal(err)
	}
	it := b.Item(3)
	if it.Status != StatusQueued || it.Runs[0].State != RunFailed {
		t.Fatalf("item %+v", it)
	}
	if n := it.Thread[len(it.Thread)-1]; n.Author != AuthorAF || !strings.Contains(n.Text, "boom") {
		t.Fatalf("note %+v", n)
	}
}

func TestRecoverStarts(t *testing.T) {
	b := runsBacklog(t)
	b.Item(3).Status = StatusQueued
	p2, _ := b.BeginStart(2, StartedByUser, nil, t0)
	p3, _ := b.BeginStart(3, StartedByUser, nil, t0.Add(time.Minute))
	sessions := []ItemSession{
		{ID: "old", Item: 2, CreatedAt: t0.Add(-time.Hour)},
		{ID: "s2b", Item: 2, Title: "B-2 Child a b-2", CreatedAt: t0.Add(2 * time.Second)},
		{ID: "s2a", Item: 2, Title: "B-2 Child a b", CreatedAt: t0.Add(time.Second)},
		{ID: "x", Item: 9, CreatedAt: t0.Add(time.Hour)},
	}
	changed := b.RecoverStarts(sessions, t0.Add(time.Hour), t0)
	if len(changed) != 2 {
		t.Fatalf("changed %v", changed)
	}
	r2 := b.Item(2).Runs[0]
	if r2.State != RunStarted || r2.SessionID != "s2a" || !r2.StartedAt.Equal(p2.Run.StartedAt) {
		t.Fatalf("matched the earliest session at or after the start: %+v", r2)
	}
	it3 := b.Item(3)
	if it3.Runs[0].State != RunFailed || it3.Status != StatusQueued || !it3.Runs[0].StartedAt.Equal(p3.Run.StartedAt) {
		t.Fatalf("no match fails and restores the status: %+v", it3)
	}
	if b.RecoverStarts(sessions, t0.Add(time.Hour), t0) != nil {
		t.Fatal("recovery is idempotent")
	}
}

func TestRecoverStartsSkipsTakenSession(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(2)
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted, StartedAt: t0}}
	it.Status = StatusReview
	if _, err := b.BeginStart(2, StartedByUser, nil, t0); err != nil {
		t.Fatal(err)
	}
	b.RecoverStarts([]ItemSession{{ID: "s1", Item: 2, CreatedAt: t0.Add(time.Second)}}, t0.Add(time.Hour), t0)
	if it.Runs[1].State != RunFailed {
		t.Fatalf("a session another run names is not reused: %+v", it.Runs)
	}
}

func TestItemForSession(t *testing.T) {
	b := runsBacklog(t)
	b.Item(3).Runs = []RunRef{{SessionID: "s3", State: RunStarted}}
	if it := b.ItemForSession("s3"); it == nil || it.ID != 3 {
		t.Fatalf("got %+v", it)
	}
	if b.ItemForSession("") != nil || b.ItemForSession("nope") != nil {
		t.Fatal("matched a session that is no run")
	}
}

func TestRunEnded(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(3)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "old", State: RunStarted}, {SessionID: "s3", State: RunStarted}}

	if got := b.RunEnded("old", t0); got != (RunEnd{}) {
		t.Fatalf("an older run's end is ignored: %+v", got)
	}
	got := b.RunEnded("s3", t0)
	if !got.Changed || got.Archive || it.Status != StatusReview || it.Thread[0].Text != RunEndNote || it.Thread[0].Author != AuthorAF {
		t.Fatalf("silent run: %+v %+v", got, it)
	}

	b.Settings.OnDone = OnDoneArchive
	if got := b.RunEnded("s3", t0); got.Changed || got.Archive {
		t.Fatalf("review never archives: %+v", got)
	}
	it.Status = StatusDropped
	if got := b.RunEnded("s3", t0); got.Archive {
		t.Fatalf("dropped never archives: %+v", got)
	}
	it.Status = StatusDone
	if got := b.RunEnded("s3", t0); got.Changed || !got.Archive {
		t.Fatalf("done with on_done archive: %+v", got)
	}

	it.Status = StatusRunning
	it.Thread = append(it.Thread, Message{ID: 9, Kind: KindQuestion, Text: "?"})
	if got := b.RunEnded("s3", t0); got.Changed || got.Archive || it.Status != StatusRunning {
		t.Fatalf("open question keeps it running: %+v", got)
	}
}

func TestSessionTitleBase(t *testing.T) {
	if got := SessionTitleBase(&Item{ID: 12, Title: "web / view  x"}); got != "B-12 web view x" {
		t.Fatalf("got %q", got)
	}
}

func TestItemPrompt(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(2)
	it.Body = "Do it.\n````\nnested\n````"
	got := b.ItemPrompt(it, "/af/proposals/backlog")
	for _, want := range []string{
		"backlog item B-2: Child a/b", "part of B-1: Parent", "/af/proposals/backlog",
		"`````markdown\nDo it.", "af backlog done --self", "Do not create other af sessions",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(b.ItemPrompt(b.Item(3), ""), "proposal") {
		t.Error("no proposal line without a proposal")
	}
	if !strings.Contains(ContinuePrompt(it), "B-2") {
		t.Error("continue prompt names the item")
	}
}

func TestServiceGetUsesHostSlots(t *testing.T) {
	s, h := newTestService(t)
	h.slots = 2
	v, err := s.Get(GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.Used != 2 {
		t.Fatalf("used %d", v.Runner.Used)
	}
}

func TestRecoverStartsSkipsLaterStarts(t *testing.T) {
	b := runsBacklog(t)
	if _, err := b.BeginStart(2, StartedByUser, nil, t0); err != nil {
		t.Fatal(err)
	}
	if b.RecoverStarts(nil, t0, t0) != nil || b.Item(2).Runs[0].State != RunStarting {
		t.Fatal("a start at or after the cutoff belongs to this daemon")
	}
}

func TestBeginStartLimited(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(2)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	limited := stubFacts(map[string]SessionFacts{"s1": {Limited: true, Reason: "usage limit until 14:00"}})
	if _, err := b.BeginStart(2, StartedByUser, limited, t0); err == nil || err.Error() != "B-2 is at a usage limit until 14:00" {
		t.Fatalf("got %v", err)
	}
}

func TestContinueFailed(t *testing.T) {
	b := runsBacklog(t)
	it := b.Item(2)
	it.Status = StatusReview
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	if err := b.Continued(2, "s1", t0); err != nil {
		t.Fatal(err)
	}
	if err := b.ContinueFailed(2, StatusReview, errors.New("pane gone"), t0); err != nil {
		t.Fatal(err)
	}
	if it.Status != StatusReview || !strings.Contains(it.Thread[len(it.Thread)-1].Text, "pane gone") {
		t.Fatalf("item %+v", it)
	}
}

func TestLatestSessions(t *testing.T) {
	b := runsBacklog(t)
	b.Item(2).Runs = []RunRef{{SessionID: "old", State: RunStarted}, {SessionID: "new", State: RunStarted}}
	b.Item(3).Runs = []RunRef{{SessionID: "s3", State: RunStarted}, {State: RunFailed}}
	got := b.LatestSessions()
	if len(got) != 1 || got["new"] != 2 {
		t.Fatalf("got %v", got)
	}
	if r := b.LatestRun(2); r == nil || r.SessionID != "new" || b.LatestRun(9) != nil {
		t.Fatalf("latest run %+v", r)
	}
}

// A session that reports on its item before the start record is committed is
// linked from its own item id and creation time (Codex P2 on #6294), and the
// start record that arrives afterwards is a no-op for that run.
func TestAdoptStartingLinksAnUnrecordedSession(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	plan, err := b.BeginStart(it.ID, StartedByUser, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	started := plan.Run.StartedAt
	if b.AdoptStarting(it.ID, "s1", "B-1 A", started.Add(-time.Second), now) {
		t.Fatal("a session created before the run began was adopted")
	}
	if b.AdoptStarting(it.ID+1, "s1", "x", started, now) {
		t.Fatal("an unknown item adopted a session")
	}
	if !b.AdoptStarting(it.ID, "s1", "B-1 A", started.Add(time.Second), now) {
		t.Fatal("the starting run was not adopted")
	}
	if b.ItemForSession("s1") == nil || b.LatestRun(it.ID).State != RunStarted {
		t.Fatalf("not linked: %+v", b.Item(it.ID).Runs)
	}
	if b.AdoptStarting(it.ID, "s2", "x", started.Add(time.Second), now) {
		t.Fatal("a second session adopted a run that is already linked")
	}
	if err := b.FinishStart(it.ID, started, "s1", "B-1 A", nil, now); err != nil {
		t.Fatalf("the late start record must be a no-op for an adopted run: %v", err)
	}
}

// A run's end does not move the item to review while an answer is on its way
// (Codex P2 on #6295): delivery arms the next run, which ends on its own edge.
func TestRunEndedWaitsForAPendingAnswer(t *testing.T) {
	now := time.Now()
	b := New()
	it, _ := b.Add(NewItem{Title: "A"}, now)
	it.Status = StatusRunning
	it.Runs = []RunRef{{SessionID: "s1", State: RunStarted}}
	it.Thread = []Message{
		{ID: 1, Kind: KindQuestion, Text: "?", Closed: ClosedAnswered},
		{ID: 2, Kind: KindAnswer, Answers: 1, Delivery: DeliveryPending},
	}
	if end := b.RunEnded("s1", now); end.Changed || b.Item(1).Status != StatusRunning {
		t.Fatalf("the old run's end finished an item with an answer pending: %+v %s", end, b.Item(1).Status)
	}
	b.Item(1).Thread[1].Delivery = DeliveryDelivered
	if end := b.RunEnded("s1", now); !end.Changed || b.Item(1).Status != StatusReview {
		t.Fatalf("with nothing pending the end moves to review: %+v %s", end, b.Item(1).Status)
	}
}
