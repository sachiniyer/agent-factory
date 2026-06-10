package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// TestMain initializes the logger so that functions under test that write
// WarningLog/ErrorLog messages do not nil-deref.
func TestMain(m *testing.M) {
	log.Initialize(false)
	defer log.Close()
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	code := m.Run()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// codexYOLOBanner is the actual codex startup pane from
// sachiniyer/agent-factory#714 — codex rendered its banner, the YOLO-mode
// header, and the "›" (U+203A) input prompt, but the claude-only
// isReadyContent never matched it, so waitForReady spun for the full 60s.
const codexYOLOBanner = "╭───────────────────────────────────────────────╮\n" +
	"│ >_ OpenAI Codex (v0.135.0)                    │\n" +
	"│ permissions: YOLO mode                        │\n" +
	"╰───────────────────────────────────────────────╯\n" +
	"› Use /skills to list available skills"

func TestIsReadyContent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		content string
		want    bool
	}{
		// claude (and the default/legacy fallback)
		{"empty", "claude", "", false},
		{"claude input prompt", "claude", "some output\n\n❯ ", true},
		{"claude trust prompt", "claude", "Do you trust the files in this folder?\n1. Yes\n2. No", true},
		{"claude mcp trust prompt", "claude", "Claude Code detected a new MCP server from `.mcp.json`.\n1. Use this MCP server", true},
		{
			name:  "claude doc trust prompt",
			agent: "claude",
			content: "Open documentation url for more info: https://docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{"claude not ready", "claude", "installing dependencies...\nready soon", false},
		// An unknown / legacy program falls through to the claude signals.
		{"unknown program uses claude signals", "/usr/bin/some-tool", "some output\n❯ ", true},
		{"unknown program not ready", "/usr/bin/some-tool", "compiling…", false},

		// codex — regression case from #714.
		{"codex YOLO banner with prompt (#714)", "codex", codexYOLOBanner, true},
		{"codex bare prompt glyph", "codex", "some output\n› ", true},
		// #729: the workspace-trust dialog must NOT be treated as ready —
		// no codex dismissal exists for it, so the prompt would be typed into
		// the dialog. Wait for the real "›" prompt. Regression from #714/#715.
		{"codex trust folder prompt is not ready (#729)", "codex", "Do you trust this folder?\n> Yes", false},
		{"codex trust dialog with later prompt is ready (#729)", "codex", "Do you trust this folder?\n› ", true},
		// Codex must NOT be considered ready on claude's "❯" alone, and the
		// banner box border ("╰") is not a codex ready signal by itself.
		{"codex not ready on claude glyph", "codex", "rendering\n❯ ", false},
		{"codex not ready on box border alone", "codex", "╭──╮\n│ x │\n╰──╯", false},

		// aider
		{"aider banner", "aider", "Aider v0.74.0\nMain model: ...", true},
		{"aider input prompt", "aider", "some output\n> ", true},
		{
			name:  "aider doc trust prompt",
			agent: "aider",
			content: "Open documentation url for more info: https://aider.chat/docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{"aider not ready", "aider", "loading model weights…", false},

		// gemini (best-guess box-border signal — see #714)
		{"gemini box frame", "gemini", "╭──╮\n│ Gemini │\n╰──╯", true},
		{
			name:    "gemini doc trust prompt",
			agent:   "gemini",
			content: "Gemini CLI\nOpen documentation url for more info.\n(D)on't ask again",
			want:    true,
		},
		{"gemini not ready", "gemini", "starting gemini-cli…", false},

		// shared doc-trust guard: both substrings required.
		{"only open documentation url without confirm", "claude", "See Open documentation url for details.", false},
		{"only dont ask again without doc url", "aider", "Some prompt asking (D)on't ask again", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content, tc.agent); got != tc.want {
				t.Errorf("isReadyContent(%q, %q) = %v, want %v", tc.content, tc.agent, got, tc.want)
			}
		})
	}
}

// setupPendingInstancesDir overrides AGENT_FACTORY_HOME to a temp directory
// for the duration of the test and returns the expected pending instances
// file path.
func setupPendingInstancesDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, hadOrig := os.LookupEnv("AGENT_FACTORY_HOME")
	if err := os.Setenv("AGENT_FACTORY_HOME", dir); err != nil {
		t.Fatalf("failed to set AGENT_FACTORY_HOME: %v", err)
	}
	t.Cleanup(func() {
		if hadOrig {
			os.Setenv("AGENT_FACTORY_HOME", orig)
		} else {
			os.Unsetenv("AGENT_FACTORY_HOME")
		}
	})
	return filepath.Join(dir, pendingInstancesFileName)
}

func TestLoadAndClearPendingInstances_Missing(t *testing.T) {
	setupPendingInstancesDir(t)

	pending, err := LoadAndClearPendingInstances()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending, got %d", len(pending))
	}
}

func TestLoadAndClearPendingInstances_Valid(t *testing.T) {
	path := setupPendingInstancesDir(t)

	data := []session.InstanceData{{Title: "one"}, {Title: "two"}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	pending, err := LoadAndClearPendingInstances()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pending) != 2 || pending[0].Title != "one" || pending[1].Title != "two" {
		t.Fatalf("unexpected pending: %+v", pending)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected pending file to be removed, stat err = %v", err)
	}
}

// TestLoadAndClearPendingInstances_Corrupted verifies that corrupted JSON is
// treated as a recoverable condition: the function returns no error, yields
// no pending instances, and removes the corrupted file so that subsequent
// calls are not stuck repeating the same failure.
func TestLoadAndClearPendingInstances_Corrupted(t *testing.T) {
	path := setupPendingInstancesDir(t)

	if err := os.WriteFile(path, []byte("this is not json{"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	pending, err := LoadAndClearPendingInstances()
	if err != nil {
		t.Fatalf("expected nil error on corrupted file, got %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending instances on corrupted file, got %d", len(pending))
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected corrupted pending file to be removed, stat err = %v", err)
	}
}

func TestNextTaskRunTitleSkipsPersistedTitles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo-title"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	preexisting := []session.InstanceData{
		{Title: "nightly"},
		{Title: "nightly-2"},
	}
	preRaw, err := json.MarshalIndent(preexisting, "", "  ")
	if err != nil {
		t.Fatalf("marshal preexisting: %v", err)
	}
	if err := os.WriteFile(instancesPath, preRaw, 0644); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}

	title, err := NextTaskRunTitle(repoID, "/tmp/repo", "nightly", "claude")
	if err != nil {
		t.Fatalf("NextTaskRunTitle: %v", err)
	}
	if title != "nightly-3" {
		t.Fatalf("expected nightly-3, got %q", title)
	}
}

// TestNextTaskRunTitleCaseInsensitive guards against #721: the persisted title
// "Foo" must block the case-variant base "foo", so the next title is "foo-2"
// rather than "foo". A case-sensitive check would hand back "foo", which the
// daemon's EqualFold validation then rejects.
func TestNextTaskRunTitleCaseInsensitive(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo-title-case"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	preexisting := []session.InstanceData{
		{Title: "Foo"},
	}
	preRaw, err := json.MarshalIndent(preexisting, "", "  ")
	if err != nil {
		t.Fatalf("marshal preexisting: %v", err)
	}
	if err := os.WriteFile(instancesPath, preRaw, 0644); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}

	title, err := NextTaskRunTitle(repoID, "/tmp/repo", "foo", "claude")
	if err != nil {
		t.Fatalf("NextTaskRunTitle: %v", err)
	}
	if title != "foo-2" {
		t.Fatalf("expected foo-2 (case-variant of persisted %q must collide), got %q", "Foo", title)
	}
}

// TestNextTaskRunTitleSanitizeCollision guards against #741 (completing #721):
// the persisted title "My Task" must block the base "my-task" because both
// sanitize to the same git branch, so the next title is "my-task-2". A check
// that only compared case-insensitively would hand back "my-task", which the
// daemon's branch-collision validation then rejects.
func TestNextTaskRunTitleSanitizeCollision(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo-title-sanitize"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	preexisting := []session.InstanceData{
		{Title: "My Task"},
	}
	preRaw, err := json.MarshalIndent(preexisting, "", "  ")
	if err != nil {
		t.Fatalf("marshal preexisting: %v", err)
	}
	if err := os.WriteFile(instancesPath, preRaw, 0644); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}

	title, err := NextTaskRunTitle(repoID, "/tmp/repo", "my-task", "claude")
	if err != nil {
		t.Fatalf("NextTaskRunTitle: %v", err)
	}
	if title != "my-task-2" {
		t.Fatalf("expected my-task-2 (sanitize-variant of persisted %q must collide), got %q", "My Task", title)
	}
}

func TestTaskRunBaseTitleFallsBackToTaskID(t *testing.T) {
	got := TaskRunBaseTitle(Task{ID: "abc123"})
	if got != "task-abc123" {
		t.Fatalf("unexpected fallback title: %q", got)
	}
}

// TestFormatWaitForReadyTimeoutError covers the UX half of
// sachiniyer/agent-factory#502: when WaitForReady gives up, the returned
// error must carry a trimmed snippet of the captured pane content so the
// user-facing TUI shows what the agent was doing — not just "timed out".
// Empty captured content collapses back to the bare timeout message so
// users don't see a dangling "last pane content:" header.
func TestFormatWaitForReadyTimeoutError(t *testing.T) {
	timeout := 60 * time.Second

	t.Run("happy case appends trimmed snippet", func(t *testing.T) {
		// >5 lines and well under 400 bytes — should keep only the last 5.
		content := "boot 1\nboot 2\nboot 3\nLoading config...\nConnecting to MCP server...\nStill waiting on handshake\nAlmost there..."
		got := formatWaitForReadyTimeoutError(timeout, content).Error()

		wantHeader := "timed out waiting for program to start (1m0s)\nlast pane content:"
		if !strings.HasPrefix(got, wantHeader) {
			t.Fatalf("missing header.\n got=%q\nwant prefix=%q", got, wantHeader)
		}
		if !strings.Contains(got, "  Almost there...") {
			t.Errorf("expected indented snippet line in error, got %q", got)
		}
		if !strings.Contains(got, "  Loading config...") {
			t.Errorf("expected last-5-lines window to include 'Loading config...', got %q", got)
		}
		if strings.Contains(got, "boot 1") || strings.Contains(got, "boot 2") {
			t.Errorf("expected oldest lines to be trimmed off, got %q", got)
		}
	})

	t.Run("empty content omits header entirely", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("empty content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("whitespace-only content treated as empty", func(t *testing.T) {
		got := formatWaitForReadyTimeoutError(timeout, "\n\n   \n\n").Error()
		want := "timed out waiting for program to start (1m0s)"
		if got != want {
			t.Fatalf("whitespace-only content error mismatch.\n got=%q\nwant=%q", got, want)
		}
	})

	t.Run("long content is byte-capped", func(t *testing.T) {
		// One huge line well over the 400-byte cap.
		long := strings.Repeat("x", 1200)
		got := formatWaitForReadyTimeoutError(timeout, long).Error()
		// Header + "\n  " + at most 400 bytes of snippet.
		header := "timed out waiting for program to start (1m0s)\nlast pane content:\n  "
		if !strings.HasPrefix(got, header) {
			t.Fatalf("missing header prefix, got %q", got)
		}
		snippet := strings.TrimPrefix(got, header)
		if len(snippet) > 400 {
			t.Errorf("snippet not capped: len=%d, want <=400", len(snippet))
		}
	})
}
