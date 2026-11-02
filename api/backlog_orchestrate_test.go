package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func withOrchestratorSessions(t *testing.T, self *session.InstanceData) {
	t.Helper()
	prev := backlogTestSessions
	backlogTestSessions = map[string]backlog.SessionInfo{
		"o1": {Title: "planner", Program: "claude", State: backlog.SessionLive},
		"s1": {Title: "B-2 a", Item: 2, State: backlog.SessionLive},
	}
	t.Cleanup(func() { backlogTestSessions = prev })
	backlogSelfSession = func() (*session.InstanceData, error) { return self, nil }
}

func writeBatch(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "items.json")
	batch := `{"proposal": "effort", "items": [{"key": "g", "title": "Effort"}, {"key": "a", "parent": "g", "title": "a"}]}`
	if err := os.WriteFile(path, []byte(batch), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type addOrchOut struct {
	IDs          map[string]int                `json:"ids"`
	Orchestrator *backlog.OrchestratorResponse `json:"orchestrator"`
}

func TestBacklogAddOrchestrate(t *testing.T) {
	stubBacklogDaemon(t)
	withOrchestratorSessions(t, &session.InstanceData{ID: "o1", Title: "planner"})
	out, err := runBacklogCmd(t, "add", "--file", writeBatch(t), "--orchestrate")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeOut[addOrchOut](t, out)
	if got.IDs["g"] != 1 || got.Orchestrator == nil || got.Orchestrator.ID != 1 ||
		got.Orchestrator.Orchestrator.SessionID != "o1" || got.Orchestrator.Orchestrator.State != backlog.SessionLive {
		t.Fatal(out)
	}
	out, err = runBacklogCmd(t, "show", "B-1")
	if err != nil {
		t.Fatal(err)
	}
	if o := decodeOut[backlogEntry](t, out).Orchestrator; o == nil || o.SessionID != "o1" || o.Title != "planner" ||
		o.State != backlog.SessionLive || o.Pending != 0 {
		t.Fatal(out)
	}
	if _, err := runBacklogCmd(t, "add", "x", "--orchestrate"); err == nil || err.Error() != "--orchestrate and --replace go with --file" {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "add", "--file", writeBatch(t), "--replace"); err == nil || err.Error() != "--replace goes with --orchestrate" {
		t.Fatal(err)
	}
}

func TestBacklogOrchestrateRefusedFromItemSession(t *testing.T) {
	stubBacklogDaemon(t)
	withOrchestratorSessions(t, &session.InstanceData{ID: "s1", Title: "B-2 a", BacklogItem: 2})
	path := writeBatch(t)
	want := backlog.ErrOrchestrateFromItemSession.Error()
	// The item-session add guard (decision (3) on #6284) refuses --file before
	// --orchestrate is reached; the service refuses an item session's bind too.
	if _, err := runBacklogCmd(t, "add", "--file", path, "--orchestrate"); err == nil || !strings.Contains(err.Error(), `item sessions cannot run "af backlog add --file"`) {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "orchestrate", "B-1", "--self"); err == nil || err.Error() != want {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "orchestrate", "B-1", "--clear"); err == nil || !strings.Contains(err.Error(), "item sessions cannot run") {
		t.Fatal(err)
	}
}

func TestBacklogOrchestrate(t *testing.T) {
	methods := stubBacklogDaemon(t)
	withOrchestratorSessions(t, &session.InstanceData{ID: "o1", Title: "planner"})
	if _, err := runBacklogCmd(t, "add", "--file", writeBatch(t)); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("next: B-2"), 0o644); err != nil {
		t.Fatal(err)
	}

	*methods = nil
	out, err := runBacklogCmd(t, "orchestrate", "B-1", "--self", "--handoff-file", notes)
	if err != nil {
		t.Fatal(err)
	}
	r := decodeOut[backlog.OrchestratorResponse](t, out)
	if r.ID != 1 || r.Orchestrator == nil || r.Orchestrator.SessionID != "o1" || r.Orchestrator.Handoff != "next: B-2" {
		t.Fatal(out)
	}
	if strings.Join(*methods, ",") != "SetBacklogOrchestrator" {
		t.Fatal(*methods)
	}

	if err := os.WriteFile(notes, []byte("next: B-3"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runBacklogCmd(t, "orchestrate", "--self", "--handoff-file", notes)
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeOut[backlog.OrchestratorResponse](t, out); r.Orchestrator.Handoff != "next: B-3" {
		t.Fatal(out)
	}

	out, err = runBacklogCmd(t, "orchestrate", "B-1", "--clear")
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeOut[backlog.OrchestratorResponse](t, out); r.ID != 1 || r.Orchestrator != nil {
		t.Fatal(out)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"B-1"}, "needs one of --self or --clear"},
		{[]string{"B-1", "--self", "--clear"}, "needs one of --self or --clear"},
		{[]string{"--clear"}, "--clear needs the group's id"},
		{[]string{"B-1", "--clear", "--handoff-file", notes}, "--handoff-file and --replace go with --self"},
		{[]string{"B-1", "--clear", "--replace"}, "--handoff-file and --replace go with --self"},
		{[]string{"--self", "--replace", "--handoff-file", notes}, "--replace needs the group's id"},
		{[]string{"--self"}, "needs a group id, or --handoff-file"},
		{[]string{"B-2", "--self"}, "B-2 is not a top-level parent"},
	} {
		if _, err := runBacklogCmd(t, append([]string{"orchestrate"}, c.args...)...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

func TestBacklogOrchestrateReplace(t *testing.T) {
	stubBacklogDaemon(t)
	withOrchestratorSessions(t, &session.InstanceData{ID: "o1", Title: "planner"})
	backlogTestSessions["o3"] = backlog.SessionInfo{Title: "new", State: backlog.SessionLive}
	if _, err := runBacklogCmd(t, "add", "--file", writeBatch(t), "--orchestrate"); err != nil {
		t.Fatal(err)
	}
	backlogSelfSession = func() (*session.InstanceData, error) { return &session.InstanceData{ID: "o3", Title: "new"}, nil }
	_, err := runBacklogCmd(t, "orchestrate", "B-1", "--self")
	if err == nil || err.Error() != `B-1 is orchestrated by session "o1"; pass --replace to take it over` {
		t.Fatal(err)
	}
	out, err := runBacklogCmd(t, "orchestrate", "B-1", "--self", "--replace")
	if err != nil {
		t.Fatal(err)
	}
	if r := decodeOut[backlog.OrchestratorResponse](t, out); r.Change != backlog.OrchestratorReplaced || r.Previous != "o1" || r.Orchestrator.SessionID != "o3" {
		t.Fatal(out)
	}
}
