package session

import (
	"os"
	"strings"
	"testing"
)

func TestBuildSystemPrompt(t *testing.T) {
	prompt := buildSystemPrompt("my-task")
	if !strings.Contains(prompt, "my-task") {
		t.Error("expected prompt to contain session title")
	}
	if !strings.Contains(prompt, "af api sessions list") {
		t.Error("expected prompt to contain session list command")
	}
	if !strings.Contains(prompt, "af api tasks toggle") {
		t.Error("expected prompt to contain task toggle command")
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

	if !strings.Contains(result, "--append-system-prompt") {
		t.Errorf("expected --append-system-prompt flag, got %q", result)
	}
	if !strings.HasPrefix(result, "claude") {
		t.Errorf("expected result to start with 'claude', got %q", result)
	}
	if !strings.Contains(result, "test-session") {
		t.Errorf("expected result to contain session title, got %q", result)
	}

	// Should NOT write any files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("unexpected file written for claude: %s", e.Name())
	}
}

func TestInjectSystemPrompt_ClaudeWithArgs(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("claude --model opus", "my-session", dir)

	if !strings.HasPrefix(result, "claude --model opus") {
		t.Errorf("expected original args preserved, got %q", result)
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

	// Should NOT write any files
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
