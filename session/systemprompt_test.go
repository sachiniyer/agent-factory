package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello", "'hello'"},
		{"it's", "'it'\\''s'"},
		{"no quotes", "'no quotes'"},
		{"", "''"},
	}
	for _, tt := range tests {
		got := shellQuote(tt.input)
		if got != tt.expected {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestInjectSystemPrompt_Claude(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	result := injectSystemPrompt(tmux.ProgramClaude, "claude", "test-session", dir)

	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
	if !strings.HasPrefix(result, "claude") {
		t.Errorf("expected result to start with 'claude', got %q", result)
	}
	if strings.Contains(result, "--append-system-prompt") {
		t.Errorf("expected no --append-system-prompt flag, got %q", result)
	}
}

func TestInjectSystemPrompt_ClaudeWithResolvedFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	// The resolved form (from program_overrides) carries the path-and-flags;
	// injectSystemPrompt appends --plugin-dir to it.
	result := injectSystemPrompt(tmux.ProgramClaude, "/usr/local/bin/claude --model opus", "my-session", dir)

	if !strings.HasPrefix(result, "/usr/local/bin/claude --model opus") {
		t.Errorf("expected resolved form preserved, got %q", result)
	}
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
}

func TestInjectSystemPrompt_Codex(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt(tmux.ProgramCodex, "codex", "test-session", dir)

	if !strings.Contains(result, "-c") {
		t.Errorf("expected -c flag for codex, got %q", result)
	}
	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions= in flag, got %q", result)
	}
	if !strings.Contains(result, "af sessions whoami") {
		t.Errorf("expected whoami command in codex prompt, got %q", result)
	}
	if !strings.HasPrefix(result, "codex") {
		t.Errorf("expected result to start with 'codex', got %q", result)
	}

	// Should NOT write any files in the worktree dir
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("unexpected file written for codex: %s", e.Name())
	}
}

func TestInjectSystemPrompt_CodexWithResolvedFlags(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt(tmux.ProgramCodex, "codex --full-auto", "my-session", dir)

	if !strings.HasPrefix(result, "codex --full-auto") {
		t.Errorf("expected resolved form preserved, got %q", result)
	}
	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions flag, got %q", result)
	}
}

func TestInjectSystemPrompt_Aider(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt(tmux.ProgramAider, "aider", "test-session", dir)

	if result != "aider" {
		t.Errorf("expected aider unchanged (no system-prompt flag), got %q", result)
	}
}

func TestInjectSystemPrompt_Gemini(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt(tmux.ProgramGemini, "gemini", "test-session", dir)

	if result != "gemini" {
		t.Errorf("expected gemini unchanged (no system-prompt flag), got %q", result)
	}
}

func TestEnsurePluginDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	pluginDir, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("ensurePluginDir() failed: %v", err)
	}

	expectedDir := filepath.Join(tmpDir, "plugin")
	if pluginDir != expectedDir {
		t.Errorf("expected plugin dir %q, got %q", expectedDir, pluginDir)
	}

	// Verify plugin manifest exists
	manifestPath := filepath.Join(pluginDir, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		t.Error("expected .claude-plugin/plugin.json manifest to exist")
	}

	commandsDir := filepath.Join(pluginDir, "commands")
	expectedFiles := []string{"af-sessions.md", "af-kill.md", "af-send.md", "af-preview.md", "af-whoami.md", "af-create.md"}
	for _, name := range expectedFiles {
		path := filepath.Join(commandsDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected command file %s to exist", name)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("failed to read %s: %v", name, err)
			continue
		}
		if !strings.Contains(string(content), "allowed-tools") {
			t.Errorf("expected %s to contain frontmatter with allowed-tools", name)
		}
	}
}

func TestEnsurePluginDir_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	dir1, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	dir2, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if dir1 != dir2 {
		t.Errorf("expected same dir on repeated calls, got %q and %q", dir1, dir2)
	}
}

func TestEnsurePluginDir_PrunesStaleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	pluginDir, err := ensurePluginDir()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	commandsDir := filepath.Join(pluginDir, "commands")
	stale := filepath.Join(commandsDir, "af-removed.md")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatalf("failed to seed stale file: %v", err)
	}

	// Non-.md files and unrelated content must be left alone.
	keep := filepath.Join(commandsDir, "README.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0644); err != nil {
		t.Fatalf("failed to seed keep file: %v", err)
	}

	if _, err := ensurePluginDir(); err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("expected stale file %s to be pruned, got err=%v", stale, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("expected non-.md file %s to survive prune: %v", keep, err)
	}

	for name := range pluginCommands {
		if _, err := os.Stat(filepath.Join(commandsDir, name)); err != nil {
			t.Errorf("expected %s to still exist after prune: %v", name, err)
		}
	}
}
