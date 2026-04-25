package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
