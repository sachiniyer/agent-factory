package daemon

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestLastLines(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"fewer than n", "a\nb", 5, "a\nb"},
		{"keeps the tail", "a\nb\nc\nd", 2, "c\nd"},
		{"drops trailing blank screen rows", "a\nb\n\n   \n\n", 2, "a\nb"},
		{"trims trailing spaces", "a  \nb\t", 5, "a\nb"},
		{"keeps inner blank lines", "a\n\nb", 3, "a\n\nb"},
		{"empty", "", 3, ""},
	}
	for _, tc := range cases {
		if got := lastLines(tc.in, tc.n); got != tc.want {
			t.Errorf("%s: lastLines(%q, %d) = %q, want %q", tc.name, tc.in, tc.n, got, tc.want)
		}
	}
}

func TestPlainText_DropsControlBytes(t *testing.T) {
	cases := map[string]string{
		"a\x07b\x08c\x00d\x0be":                "abcde",
		"p\u0085q\x7fr":                        "pqr",
		"\x1b[31mred\x1b[0m\r\n\tg":            "red\n\tg",
		"café → ok":                            "café → ok",
		"\x1b]8;;http://x\x07link\x1b]8;;\x07": "link",
	}
	for in, want := range cases {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"x\x9by", "x\x9b", "\x9b31mz"} {
		got := plainText(in)
		if strings.ContainsRune(got, 0x9b) || strings.Contains(got, "\x9b") || !utf8.ValidString(got) {
			t.Errorf("plainText(%q) = %q, still carries an 8-bit CSI or invalid UTF-8", in, got)
		}
	}
}

func TestPreviewSession_RejectsBadRequest(t *testing.T) {
	cs := &controlServer{}
	for _, req := range []PreviewSessionRequest{
		{ID: ""},
		{ID: "x", Lines: maxPreviewSessionLines + 1},
		{ID: "x", Lines: -1},
	} {
		var resp PreviewSessionResponse
		if err := cs.PreviewSession(req, &resp); err == nil {
			t.Errorf("PreviewSession(%+v) = nil error, want a refusal", req)
		}
	}
}

func TestPreviewSession_UnknownIDIsAnError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var resp PreviewSessionResponse
	err = (&controlServer{manager: m}).PreviewSession(PreviewSessionRequest{ID: "no-such-id", Lines: 12}, &resp)
	if err == nil || !strings.Contains(err.Error(), "no-such-id") {
		t.Fatalf("err = %v, want one that names the id", err)
	}
}

func previewSessionInstance(t *testing.T) (*controlServer, *session.Instance, string) {
	t.Helper()
	cs, title, repoID := previewTestSession(t)
	cs.manager.mu.Lock()
	inst := cs.manager.instances[daemonInstanceKey(repoID, title)]
	cs.manager.mu.Unlock()
	if inst == nil || inst.ID == "" {
		t.Fatalf("premise: want a tracked instance with a stable id, got %+v", inst)
	}
	return cs, inst, repoID
}

// The route reads the agent tab the internal Preview route already serves to
// the same callers, through plainText and lastLines and nothing else.
func TestPreviewSession_ReadsTheAgentTab(t *testing.T) {
	cs, inst, repoID := previewSessionInstance(t)
	var full PreviewResponse
	if err := cs.Preview(PreviewRequest{Title: inst.Title, RepoID: repoID, Tab: 0}, &full); err != nil {
		t.Fatalf("Preview of the agent tab: %v", err)
	}
	var resp PreviewSessionResponse
	if err := cs.PreviewSession(PreviewSessionRequest{ID: inst.ID}, &resp); err != nil {
		t.Fatalf("PreviewSession: %v", err)
	}
	if want := lastLines(plainText(full.Content), maxPreviewSessionLines); resp.Text != want {
		t.Fatalf("PreviewSession text %q, want the agent tab's capture %q", resp.Text, want)
	}
	var one PreviewSessionResponse
	if err := cs.PreviewSession(PreviewSessionRequest{ID: inst.ID, Lines: 1}, &one); err != nil {
		t.Fatalf("PreviewSession lines=1: %v", err)
	}
	if strings.Count(one.Text, "\n") > 0 {
		t.Fatalf("lines=1 returned %q", one.Text)
	}
}

func TestPreviewSession_TitleIsNotAnID(t *testing.T) {
	cs, inst, _ := previewSessionInstance(t)
	var resp PreviewSessionResponse
	if err := cs.PreviewSession(PreviewSessionRequest{ID: inst.Title}, &resp); err == nil {
		t.Fatalf("a title must not resolve; got text %q", resp.Text)
	}
}

func TestPreviewSession_NotStartedIsAnError(t *testing.T) {
	cs, inst, _ := previewSessionInstance(t)
	inst.SetStartedForTest(false)
	var resp PreviewSessionResponse
	err := cs.PreviewSession(PreviewSessionRequest{ID: inst.ID}, &resp)
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v, text = %q; a session that is not started must be an actionable error, not empty text", err, resp.Text)
	}
}

func TestPreviewSession_ArchivedIsAnError(t *testing.T) {
	cs, inst, _ := previewSessionInstance(t)
	inst.SetStatusForTest(session.Archived)
	var resp PreviewSessionResponse
	err := cs.PreviewSession(PreviewSessionRequest{ID: inst.ID}, &resp)
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("err = %v, text = %q; an archived session must say so, not return empty text", err, resp.Text)
	}
}

func TestPreviewSession_RefusesWhileAKillIsInFlight(t *testing.T) {
	cs, inst, repoID := previewSessionInstance(t)
	cs.manager.mu.Lock()
	cs.manager.killsInFlight[daemonInstanceKey(repoID, inst.Title)] = struct{}{}
	cs.manager.mu.Unlock()
	var resp PreviewSessionResponse
	err := cs.PreviewSession(PreviewSessionRequest{ID: inst.ID}, &resp)
	if err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("err = %v, text = %q; a session being deleted must not be read", err, resp.Text)
	}
}

func TestPreviewSessionErr(t *testing.T) {
	gone := previewSessionErr("s1", fmt.Errorf("capture: %w", tmux.ErrSessionGone))
	if !strings.Contains(gone.Error(), "not running") {
		t.Errorf("tmux gone mapped to %q, want a not-running error", gone)
	}
	tab := previewSessionErr("s1", fmt.Errorf("tab: %w", session.ErrTabGone))
	if !strings.Contains(tab.Error(), "agent tab closed") {
		t.Errorf("tab gone mapped to %q, want its own message", tab)
	}
	other := fmt.Errorf("boom")
	if previewSessionErr("s1", other) != other {
		t.Error("other errors must pass through")
	}
}

// PreviewSession is in the public catalogue and, like Preview, out of the
// sandbox scope: a sandbox must not read another session's screen.
func TestPreviewSession_IsPublicAndNotSandboxed(t *testing.T) {
	for _, r := range HTTPRoutes() {
		if r.Path == "/v1/PreviewSession" {
			if sandboxAllowedPath(r.Path) {
				t.Fatal("/v1/PreviewSession must not be reachable with a sandbox credential")
			}
			return
		}
	}
	t.Fatal("/v1/PreviewSession is missing from the public af api catalog")
}
