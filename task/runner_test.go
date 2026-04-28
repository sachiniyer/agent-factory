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

// TestPickFreeTaskTitle exercises the title-allocation behavior used by
// RunTask to keep concurrent task runs from stomping on each other's
// tmux session. The taken-predicate is supplied as a set of known-busy
// titles so the test can drive the algorithm without spawning real
// tmux sessions.
func TestPickFreeTaskTitle(t *testing.T) {
	makeTaken := func(busy ...string) func(string) bool {
		set := make(map[string]struct{}, len(busy))
		for _, s := range busy {
			set[s] = struct{}{}
		}
		return func(s string) bool {
			_, ok := set[s]
			return ok
		}
	}

	cases := []struct {
		name string
		base string
		busy []string
		want string
	}{
		{
			name: "base is free",
			base: "scheduled-task",
			busy: nil,
			want: "scheduled-task",
		},
		{
			name: "base taken, falls through to -1",
			base: "scheduled-task",
			busy: []string{"scheduled-task"},
			want: "scheduled-task-1",
		},
		{
			name: "base + -1 taken, lands on -2",
			base: "scheduled-task",
			busy: []string{"scheduled-task", "scheduled-task-1"},
			want: "scheduled-task-2",
		},
		{
			name: "fills gap left by killed run",
			base: "scheduled-task",
			busy: []string{"scheduled-task", "scheduled-task-2"},
			want: "scheduled-task-1",
		},
		{
			name: "long contiguous sequence",
			base: "task",
			busy: []string{"task", "task-1", "task-2", "task-3", "task-4"},
			want: "task-5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickFreeTaskTitle(tc.base, makeTaken(tc.busy...))
			if got != tc.want {
				t.Fatalf("pickFreeTaskTitle(%q, busy=%v) = %q, want %q",
					tc.base, tc.busy, got, tc.want)
			}
		})
	}
}
