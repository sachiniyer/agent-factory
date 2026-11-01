package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func TestBacklogInbox(t *testing.T) {
	stubBacklogDaemon(t)
	if out, err := runBacklogCmd(t, "inbox"); err != nil || out != "No questions\n" {
		t.Fatalf("%q %v", out, err)
	}
	self := backlogSelfSession
	withRunningItem(t, &session.InstanceData{ID: "s1", Title: "B-1 work", BacklogItem: 1})
	if _, err := runBacklogCmd(t, "ask", "--self", "which db?"); err != nil {
		t.Fatal(err)
	}
	// --handoff-file keeps the agent's notes with the question, and the inbox
	// shows them.
	notes := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(notes, []byte("tried sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "ask", "--self", "--handoff-file", notes, "and the port?"); err != nil {
		t.Fatal(err)
	}
	backlogAskHandoffFile = ""
	backlogSelfSession = self
	out, err := runBacklogCmd(t, "inbox")
	if err != nil || !strings.Contains(out, "\n  and the port?\n  Notes from the agent:\n    tried sqlite\n") {
		t.Fatalf("handoff in the inbox: %q %v", out, err)
	}
	if err != nil || !strings.HasPrefix(out, "B-1  work  · ") || !strings.Contains(out, "\n  which db?\n") {
		t.Fatalf("%q %v", out, err)
	}
	envelopeOutput = true
	out, err = runBacklogCmd(t, "inbox")
	if err != nil || !strings.Contains(out, `"data"`) || !strings.Contains(out, `"text": "which db?"`) {
		t.Fatalf("--json: %s %v", out, err)
	}
}

func TestFormatBacklogInboxStripsControls(t *testing.T) {
	now := time.Now()
	got := formatBacklogInbox([]backlog.InboxEntry{{ID: 1, Title: "a\x1b]0;pwned\x07",
		Question: backlog.Message{Text: "q\x1b[2J\u009b?", At: now, Handoff: "x\r\ny\x1b[31m"}}}, now)
	want := "B-1  a]0;pwned  · <1m ago\n  q[2J?\n  Notes from the agent:\n    x\n    y[31m\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestFormatBacklogInbox(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	got := formatBacklogInbox([]backlog.InboxEntry{
		{ID: 3, Title: "web", Question: backlog.Message{Text: "which db?", At: now.Add(-3 * time.Hour), Handoff: "tried sqlite\nslow"}},
		{ID: 5, Title: "cli", Question: backlog.Message{Text: "ship?", At: now.Add(-90 * time.Second)}},
	}, now)
	want := "B-3  web  · 3h ago\n  which db?\n  Notes from the agent:\n    tried sqlite\n    slow\n" +
		"B-5  cli  · 1m ago\n  ship?\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
