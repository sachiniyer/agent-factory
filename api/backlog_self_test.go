package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// withRunningItem adds item 1 running in session s1 and makes the command run
// inside session self.
func withRunningItem(t *testing.T, self *session.InstanceData) {
	t.Helper()
	if _, err := runBacklogCmd(t, "add", "work"); err != nil {
		t.Fatal(err)
	}
	p, err := backlogProject()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backlog.Update(p.repoID, func(b *backlog.Backlog) error {
		it := b.Item(1)
		it.Status = backlog.StatusRunning
		it.Runs = []backlog.RunRef{{SessionID: "s1", Title: "B-1 work", State: backlog.RunStarted, StartedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	backlogSelfSession = func() (*session.InstanceData, error) { return self, nil }
}

func TestBacklogStart(t *testing.T) {
	methods := stubBacklogDaemon(t)
	if _, err := runBacklogCmd(t, "add", "work"); err != nil {
		t.Fatal(err)
	}
	*methods = nil
	out, err := runBacklogCmd(t, "start", "B-1")
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeOut[backlog.RunRef](t, out); r.State != backlog.RunStarting {
		t.Fatalf("%+v", r)
	}
	if strings.Join(*methods, ",") != "StartBacklogItem" {
		t.Fatal(*methods)
	}
}

func TestBacklogRun(t *testing.T) {
	methods := stubBacklogDaemon(t)
	out, err := runBacklogCmd(t, "run", "--count", "3")
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeOut[daemon.RunBacklogResponse](t, out); len(r.Started) != 1 || r.Started[0] != 3 {
		t.Fatalf("%+v", r)
	}
	if strings.Join(*methods, ",") != "RunBacklog" {
		t.Fatal(*methods)
	}
	if _, err := runBacklogCmd(t, "run", "--count", "0"); err == nil || err.Error() != "--count must be 1 or more" {
		t.Fatal(err)
	}
}

func TestBacklogDoneSelf(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})
	out, err := runBacklogCmd(t, "done", "--self", "--pr", "https://example.com/pr/1")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); it.Status != backlog.StatusReview || it.PR != "https://example.com/pr/1" {
		t.Fatalf("%+v", it)
	}
	if _, err := runBacklogCmd(t, "done", "--self", "1"); err == nil {
		t.Error("--self with an id accepted")
	}
	out, err = runBacklogCmd(t, "note", "--self", "half", "way")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeOut[backlog.Message](t, out); m.Text != "half way" || m.Author != "agent:s1" {
		t.Fatalf("%+v", m)
	}
}

func TestBacklogSelfOutsideItemSession(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "other", Title: "foo"})
	_, err := runBacklogCmd(t, "done", "--self")
	if err == nil || err.Error() != "--self needs an item session; this session (foo) was not started from the backlog" {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "done", "--pr", "x", "1"); err == nil {
		t.Error("--pr without --self accepted")
	}
	if _, err := runBacklogCmd(t, "queue", "1"); err == nil || strings.Contains(err.Error(), "item sessions") {
		t.Fatalf("a plain session is not guarded: %v", err)
	}
}

// An agent in an item session must not use the explicit-id note form. Without
// the guard, the CLI sends PostBacklogMessage with no SessionID, so the daemon
// would record the note as the user's. Agents use `note --self`, which resolves
// the session and attributes; the user-facing id form is unchanged outside an
// item session.
func TestBacklogNoteFromItemSession(t *testing.T) {
	methods := stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})
	*methods = nil
	if _, err := runBacklogCmd(t, "note", "1", "hello"); err == nil ||
		!strings.Contains(err.Error(), "item sessions cannot run") {
		t.Fatalf("note <id> from an item session: %v", err)
	}
	for _, m := range *methods {
		if m == "PostBacklogMessage" {
			t.Fatalf("a refused note reached PostBacklogMessage: %v", *methods)
		}
	}
	out, err := runBacklogCmd(t, "note", "--self", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeOut[backlog.Message](t, out); m.Text != "hello" || m.Author != "agent:s1" {
		t.Fatalf("%+v", m)
	}
	*methods = nil
	backlogSelfSession = func() (*session.InstanceData, error) { return nil, errors.New("not in an af session") }
	out, err = runBacklogCmd(t, "note", "1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeOut[backlog.Message](t, out); m.Text != "hello" || m.Author != backlog.AuthorUser {
		t.Fatalf("%+v", m)
	}
	if strings.Join(*methods, ",") != "PostBacklogMessage" {
		t.Fatal(*methods)
	}
}

func TestBacklogGuardRails(t *testing.T) {
	methods := stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})
	*methods = nil
	for _, args := range [][]string{
		{"queue", "1"}, {"draft", "1"}, {"review", "1"}, {"reopen", "1"}, {"start", "1"}, {"done", "1"}, {"drop", "1"}, {"delete", "1"},
		{"answer", "1", "yes"}, {"note", "1", "text"}, {"run"},
		// Plan editing: an agent leaves a note instead (Codex P2 on #6294). It may
		// add the work it found, as a draft with a title and --parent only
		// (decision (3) on #6284).
		{"add", "--file", "-"}, {"add", "x", "--waits-on", "1"}, {"add", "x", "--before", "1"}, {"add", "x", "--body", "b"},
		{"add", "x", "--program", "claude"}, {"edit", "1", "--title", "x"}, {"move", "1", "--top"},
	} {
		if _, err := runBacklogCmd(t, args...); err == nil || !strings.Contains(err.Error(), "item sessions cannot run") {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"runner", "on"}, {"runner", "--max-running", "2"}, {"budget", "--limit", "40"}, {"budget", "--usd-limit", "5"}} {
		if _, err := runBacklogCmd(t, args...); err == nil || err.Error() != `item sessions cannot change the runner or budget; ask the user with "af backlog ask --self"` {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, m := range *methods {
		if m != "GetBacklog" {
			t.Fatalf("a refused command reached the daemon: %v", *methods)
		}
	}
	for _, args := range [][]string{{"runner"}, {"budget"}, {"list"}, {"show", "1"}} {
		if _, err := runBacklogCmd(t, args...); err != nil {
			t.Errorf("%v refused: %v", args, err)
		}
	}
}

func TestBacklogDoneSelfFromOlderRun(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s0", Title: "B-1 work", BacklogItem: 1})
	p, err := backlogProject()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backlog.Update(p.repoID, func(b *backlog.Backlog) error {
		it := b.Item(1)
		it.Runs = append([]backlog.RunRef{{SessionID: "s0", Title: "B-1 work", State: backlog.RunStarted, StartedAt: time.Now().Add(-time.Hour)}}, it.Runs...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "done", "--self"); err == nil || !strings.Contains(err.Error(), "older run of B-1") {
		t.Fatalf("got %v", err)
	}
}

func TestBacklogAskAndAnswer(t *testing.T) {
	methods := stubBacklogDaemon(t)
	self := &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1}
	withRunningItem(t, self)
	if _, err := runBacklogCmd(t, "ask", "which", "db?"); err == nil || !strings.Contains(err.Error(), "needs --self") {
		t.Fatal(err)
	}
	out, err := runBacklogCmd(t, "ask", "--self", "which", "db?")
	if err != nil {
		t.Fatal(err)
	}
	q := decodeOut[backlog.Message](t, out)
	if q.Kind != backlog.KindQuestion || q.Text != "which db?" || q.Author != "agent:s1" {
		t.Fatalf("%+v", q)
	}
	backlogSelfSession = func() (*session.InstanceData, error) { return nil, errors.New("not in an af session") }
	*methods = nil
	out, err = runBacklogCmd(t, "answer", "B-1", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if a := decodeOut[backlog.Message](t, out); a.Kind != backlog.KindAnswer || a.Answers != q.ID || a.Text != "postgres" {
		t.Fatalf("%+v", a)
	}
	if strings.Join(*methods, ",") != "PostBacklogMessage" {
		t.Fatal(*methods)
	}
	if _, err := runBacklogCmd(t, "answer", "1", "again"); err == nil || !strings.Contains(err.Error(), "no open question") {
		t.Fatal(err)
	}
	backlogSelfSession = func() (*session.InstanceData, error) { return self, nil }
	for _, text := range []string{"first", "second"} {
		if _, err := runBacklogCmd(t, "ask", "--self", text); err != nil {
			t.Fatal(err)
		}
	}
	backlogSelfSession = func() (*session.InstanceData, error) { return nil, errors.New("not in an af session") }
	out, err = runBacklogCmd(t, "answer", "1", "--question", "4", "to the second")
	if err != nil {
		t.Fatal(err)
	}
	if a := decodeOut[backlog.Message](t, out); a.Answers != 4 {
		t.Fatalf("%+v", a)
	}
}

func TestBacklogAskOutsideItemSession(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "other", Title: "foo"})
	_, err := runBacklogCmd(t, "ask", "--self", "q")
	if err == nil || err.Error() != "--self needs an item session; this session (foo) was not started from the backlog" {
		t.Fatal(err)
	}
}

// An agent that reports before the start record exists is resolved from its own
// item id while its run is still starting (Codex P2 on #6294).
func TestBacklogSelfResolvesAStillStartingRun(t *testing.T) {
	stubBacklogDaemon(t)
	if out, err := runBacklogCmd(t, "add", "work"); err != nil {
		t.Fatal(out, err)
	}
	p, err := backlogProject()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backlog.Update(p.repoID, func(b *backlog.Backlog) error {
		it := b.Item(1)
		it.Status = backlog.StatusRunning
		it.Runs = []backlog.RunRef{{State: backlog.RunStarting, StartedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	backlogSelfSession = func() (*session.InstanceData, error) {
		return &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1}, nil
	}
	// The client resolves the item from the session's own item id; the daemon
	// links the run when the report arrives (adoptStartingRun, daemon tests).
	id, it, err := p.selfItem()
	if err != nil || id != "s1" || it.ID != 1 {
		t.Fatalf("a quick report before the start record: %q %v %v", id, it, err)
	}
	// A session with no item at all is still refused.
	backlogSelfSession = func() (*session.InstanceData, error) {
		return &session.InstanceData{ID: "s2", Title: "plain"}, nil
	}
	if _, _, err := p.selfItem(); err == nil {
		t.Fatal("a non-item session reported")
	}
}

// From an item session, add files the agent's found work as a draft through the
// daemon, naming the session so af notes the item's origin (decision (3) on
// #6284).
func TestBacklogAddFromItemSession(t *testing.T) {
	stubBacklogDaemon(t)
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})
	out, err := runBacklogCmd(t, "add", "Found work")
	if err != nil {
		t.Fatal(err)
	}
	it := decodeOut[backlog.Item](t, out)
	if it.Status != backlog.StatusDraft || len(it.Thread) != 1 || it.Thread[0].Text != "Added by the agent working on B-1." {
		t.Fatalf("%+v", it)
	}
}
