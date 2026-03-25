package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	result := injectSystemPrompt("claude", "test-session", dir)

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

func TestInjectSystemPrompt_ClaudeWithArgs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	result := injectSystemPrompt("claude --model opus", "my-session", dir)

	if !strings.HasPrefix(result, "claude --model opus") {
		t.Errorf("expected original args preserved, got %q", result)
	}
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
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
	if !strings.Contains(result, "af api sessions whoami") {
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
	expectedFiles := []string{"af-sessions.md", "af-kill.md", "af-send.md", "af-preview.md", "af-whoami.md"}
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

// TestInjectSystemPrompt_Claude_E2E verifies the full chain: injectSystemPrompt
// creates the plugin dir, the --plugin-dir path points to real files with valid
// Claude Code slash command format, and all expected commands are present.
func TestInjectSystemPrompt_Claude_E2E(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmpDir)

	result := injectSystemPrompt("claude", "test-session", tmpDir)

	// Extract the plugin dir path from the command string
	const flag = "--plugin-dir "
	idx := strings.Index(result, flag)
	if idx == -1 {
		t.Fatalf("--plugin-dir not found in result: %q", result)
	}
	// Path is single-quoted after the flag
	rest := result[idx+len(flag):]
	if rest[0] != '\'' {
		t.Fatalf("expected single-quoted path, got: %q", rest)
	}
	end := strings.Index(rest[1:], "'")
	if end == -1 {
		t.Fatalf("unterminated quote in: %q", rest)
	}
	pluginDir := rest[1 : end+1]

	// Verify the plugin dir exists and has a commands/ subdirectory
	commandsDir := filepath.Join(pluginDir, "commands")
	info, err := os.Stat(commandsDir)
	if err != nil {
		t.Fatalf("commands dir does not exist at %q: %v", commandsDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %q to be a directory", commandsDir)
	}

	// Verify all expected command files exist with valid frontmatter
	expectedCommands := map[string]struct {
		tool string // expected allowed-tools pattern
		desc string // expected description substring
	}{
		"af-sessions.md": {tool: "af api sessions list", desc: "List all Agent Factory sessions"},
		"af-kill.md":     {tool: "af api sessions kill", desc: "Kill an Agent Factory session"},
		"af-send.md":     {tool: "af api sessions send-prompt", desc: "Send a prompt to another"},
		"af-preview.md":  {tool: "af api sessions preview", desc: "Preview another Agent Factory"},
		"af-whoami.md":   {tool: "af api sessions whoami", desc: "Identify the current Agent Factory"},
	}

	entries, err := os.ReadDir(commandsDir)
	if err != nil {
		t.Fatalf("failed to read commands dir: %v", err)
	}

	foundFiles := make(map[string]bool)
	for _, e := range entries {
		foundFiles[e.Name()] = true
	}

	for name, expected := range expectedCommands {
		if !foundFiles[name] {
			t.Errorf("missing command file: %s", name)
			continue
		}

		content, err := os.ReadFile(filepath.Join(commandsDir, name))
		if err != nil {
			t.Errorf("failed to read %s: %v", name, err)
			continue
		}
		text := string(content)

		// Verify frontmatter structure (starts with ---, has allowed-tools, description, ends with ---)
		if !strings.HasPrefix(text, "---\n") {
			t.Errorf("%s: missing frontmatter opening '---'", name)
		}
		if !strings.Contains(text, "allowed-tools:") {
			t.Errorf("%s: missing 'allowed-tools:' in frontmatter", name)
		}
		if !strings.Contains(text, "description:") {
			t.Errorf("%s: missing 'description:' in frontmatter", name)
		}
		if !strings.Contains(text, expected.tool) {
			t.Errorf("%s: expected allowed-tools to contain %q, got:\n%s", name, expected.tool, text)
		}
		if !strings.Contains(text, expected.desc) {
			t.Errorf("%s: expected description to contain %q, got:\n%s", name, expected.desc, text)
		}
	}

	// Verify no unexpected files
	for name := range foundFiles {
		if _, ok := expectedCommands[name]; !ok {
			t.Errorf("unexpected file in commands dir: %s", name)
		}
	}
}
