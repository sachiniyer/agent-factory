package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// TestMain initializes the logger so that functions under test that write
// WarningLog/ErrorLog messages do not nil-deref.
func TestMain(m *testing.M) {
	log.Initialize(false)
	defer log.Close()
	os.Exit(m.Run())
}

func TestIsReadyContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			name:    "claude input prompt",
			content: "some output\n\n❯ ",
			want:    true,
		},
		{
			name:    "claude trust prompt",
			content: "Do you trust the files in this folder?\n1. Yes\n2. No",
			want:    true,
		},
		{
			name:    "claude mcp trust prompt",
			content: "Claude Code detected a new MCP server from `.mcp.json`.\n1. Use this and all future MCP servers in this project\n2. Use this MCP server\n3. Continue without using this MCP server",
			want:    true,
		},
		{
			name: "aider trust prompt",
			content: "Aider v0.1\nOpen documentation url for more info: https://aider.chat/docs/\n" +
				"(Y)es/(N)o/(D)on't ask again [Yes]:",
			want: true,
		},
		{
			name: "gemini trust prompt",
			content: "Gemini CLI\nOpen documentation url for more info.\n" +
				"(D)on't ask again",
			want: true,
		},
		{
			name:    "only open documentation url without confirm",
			content: "See Open documentation url for details about this command.",
			want:    false,
		},
		{
			name:    "only dont ask again without doc url",
			content: "Some prompt asking (D)on't ask again without the documentation prefix",
			want:    false,
		},
		{
			name:    "unrelated output",
			content: "installing dependencies...\nready soon",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadyContent(tc.content); got != tc.want {
				t.Errorf("isReadyContent(%q) = %v, want %v", tc.content, got, tc.want)
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

// runnerAppendInstance mirrors the inline UpdateRepoInstances callback in
// RunTask. Keeping it identical here lets us exercise the per-repo write
// path without spinning up a full session/tmux/git-worktree fixture.
func runnerAppendInstance(repoID string, data session.InstanceData) error {
	return config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	})
}

// TestRunTask_WritesToPerRepoStorage is the regression test for issue #334.
// The bug: RunTask wrote scheduled instances only to pending_instances.json
// and launched the daemon, which loads from per-repo instances.json. The
// daemon therefore never saw the scheduled instance and AutoYes runs hung.
// This test asserts that the per-repo storage write that RunTask now performs
// produces a file the daemon path (config.LoadRepoInstances) can read back
// with the new instance present.
func TestRunTask_WritesToPerRepoStorage(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo-334"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	data := session.InstanceData{Title: "task-instance", AutoYes: true}
	if err := runnerAppendInstance(repoID, data); err != nil {
		t.Fatalf("runnerAppendInstance: %v", err)
	}

	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 instance in per-repo storage, got %d: %+v", len(got), got)
	}
	if got[0].Title != "task-instance" {
		t.Fatalf("unexpected instance title: %q", got[0].Title)
	}
	if !got[0].AutoYes {
		t.Fatalf("expected AutoYes=true on persisted instance so the daemon picks it up")
	}
}

// TestRunTask_AppendsToExistingPerRepoStorage verifies that the runner's
// append callback preserves existing instances written by other paths
// (e.g. the API or TUI), so concurrent writers don't clobber each other.
func TestRunTask_AppendsToExistingPerRepoStorage(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo-334-merge"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Pre-populate with an existing instance, simulating one created via
	// the API or TUI.
	preexisting := []session.InstanceData{{Title: "existing-from-api"}}
	preRaw, err := json.MarshalIndent(preexisting, "", "  ")
	if err != nil {
		t.Fatalf("marshal preexisting: %v", err)
	}
	if err := os.WriteFile(instancesPath, preRaw, 0644); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}

	if err := runnerAppendInstance(repoID, session.InstanceData{Title: "task-instance"}); err != nil {
		t.Fatalf("runnerAppendInstance: %v", err)
	}

	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 instances after append, got %d: %+v", len(got), got)
	}
	if got[0].Title != "existing-from-api" || got[1].Title != "task-instance" {
		t.Fatalf("unexpected merged instances: %+v", got)
	}
}
