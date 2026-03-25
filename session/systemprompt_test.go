package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCodexSystemPrompt(t *testing.T) {
	prompt := buildCodexSystemPrompt("my-task")
	if !strings.Contains(prompt, "my-task") {
		t.Error("expected prompt to contain session title")
	}
	if !strings.Contains(prompt, "af api sessions list") {
		t.Error("expected prompt to contain session list command")
	}
	if !strings.Contains(prompt, "af api sessions preview") {
		t.Error("expected prompt to contain session preview command")
	}
}

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
	result := injectSystemPrompt("claude", "test-session", dir)

	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
	if !strings.Contains(result, "--append-system-prompt") {
		t.Errorf("expected --append-system-prompt flag, got %q", result)
	}
	if !strings.HasPrefix(result, "claude") {
		t.Errorf("expected result to start with 'claude', got %q", result)
	}
	if !strings.Contains(result, "test-session") {
		t.Errorf("expected result to contain session title, got %q", result)
	}
}

func TestInjectSystemPrompt_ClaudeWithArgs(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("claude --model opus", "my-session", dir)

	if !strings.HasPrefix(result, "claude --model opus") {
		t.Errorf("expected original args preserved, got %q", result)
	}
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
	if !strings.Contains(result, "--append-system-prompt") {
		t.Errorf("expected --append-system-prompt flag, got %q", result)
	}
}

func TestInjectSystemPrompt_Codex(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("codex", "test-session", dir)

	if !strings.Contains(result, "-c") {
		t.Errorf("expected -c flag for codex, got %q", result)
	}
	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions= in flag, got %q", result)
	}
	if !strings.Contains(result, "test-session") {
		t.Errorf("expected session title in flag, got %q", result)
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

func TestInjectSystemPrompt_CodexWithArgs(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("codex --full-auto", "my-session", dir)

	if !strings.HasPrefix(result, "codex --full-auto") {
		t.Errorf("expected original args preserved, got %q", result)
	}
	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions flag, got %q", result)
	}
}

func TestInjectSystemPrompt_UnknownProgram(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("amp", "test-session", dir)

	if result != "amp" {
		t.Errorf("expected program unchanged for unsupported tool, got %q", result)
	}

	// Should NOT write any files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("unexpected file written for unsupported program: %s", e.Name())
	}
}

func TestEnsurePluginDir(t *testing.T) {
	// Use a temp dir as config home
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

	commandsDir := filepath.Join(pluginDir, "commands")
	expectedFiles := []string{"af-sessions.md", "af-kill.md", "af-send.md", "af-preview.md"}
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
