package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

type committedSetConversationError struct{ message string }

func (e committedSetConversationError) Error() string         { return e.message }
func (committedSetConversationError) MutationCommitted() bool { return true }

// TestSessionsSetConversation_UsesSharedDaemonPath pins the daemon-owned
// contract (#6769): the CLI hands title+repo scope+id to the daemon, which
// validates the id against the session's own store — the CLI must not grow a
// second validator that could drift from the daemon's strict correlation.
func TestSessionsSetConversation_UsesSharedDaemonPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.SetConversationRequest
	prev := setConversationViaDaemon
	setConversationViaDaemon = func(req daemon.SetConversationRequest) error {
		gotReq = req
		return nil
	}
	t.Cleanup(func() { setConversationViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsSetConversationCmd, []string{"worker", "019f386f-7206-7fc2-803b-f7045e07a299"})
	if err != nil {
		t.Fatalf("set-conversation returned error: %v", err)
	}
	if gotReq.Title != "worker" || gotReq.RepoID != repoID ||
		gotReq.ConversationID != "019f386f-7206-7fc2-803b-f7045e07a299" || gotReq.ID != "" {
		t.Fatalf("SetConversation request = %+v, want title worker scoped to repo %q carrying the typed id", gotReq, repoID)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["title"] != "worker" {
		t.Fatalf("JSON output = %v, want ok=true and title=worker", payload)
	}
}

// TestSessionsSetConversation_SurfacesDaemonRejection keeps the CLI from
// claiming a write when the daemon's strict correlation refused the id.
func TestSessionsSetConversation_SurfacesDaemonRejection(t *testing.T) {
	setupRepoForCmd(t)

	prev := setConversationViaDaemon
	setConversationViaDaemon = func(daemon.SetConversationRequest) error {
		return errors.New("no rollout named 019f386f-7206-7fc2-803b-f7045e07a299 exists in the session's Codex store")
	}
	t.Cleanup(func() { setConversationViaDaemon = prev })

	_, err := runCmdCaptureStdout(t, sessionsSetConversationCmd, []string{"worker", "019f386f-7206-7fc2-803b-f7045e07a299"})
	if err == nil {
		t.Fatal("set-conversation must surface the daemon rejection")
	}
	if !strings.Contains(err.Error(), "no rollout named") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}

func TestSessionsSetConversation_PrintsCommittedWarningAsSuccess(t *testing.T) {
	setupRepoForCmd(t)

	prev := setConversationViaDaemon
	setConversationViaDaemon = func(daemon.SetConversationRequest) error {
		return committedSetConversationError{message: "conversation recorded; event publish failed"}
	}
	t.Cleanup(func() { setConversationViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsSetConversationCmd, []string{"worker", "019f386f-7206-7fc2-803b-f7045e07a299"})
	if err != nil {
		t.Fatalf("committed set-conversation returned error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("output is not JSON (%q): %v", out, err)
	}
	if payload["ok"] != true || payload["warning"] != "conversation recorded; event publish failed" {
		t.Fatalf("JSON output = %v, want ok=true with committed warning", payload)
	}
}
