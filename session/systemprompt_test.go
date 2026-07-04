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

	result := injectSystemPrompt("claude")

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
	result := injectSystemPrompt("/usr/local/bin/claude --model opus")

	if !strings.HasPrefix(result, "/usr/local/bin/claude --model opus") {
		t.Errorf("expected resolved form preserved, got %q", result)
	}
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir flag, got %q", result)
	}
}

func TestInjectSystemPrompt_Codex(t *testing.T) {
	dir := t.TempDir()
	result := injectSystemPrompt("codex")

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
	result := injectSystemPrompt("codex --full-auto")

	if !strings.HasPrefix(result, "codex --full-auto") {
		t.Errorf("expected resolved form preserved, got %q", result)
	}
	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions flag, got %q", result)
	}
}

// Regression for #820: a user who deliberately sets developer_instructions in
// their program_overrides must keep their value — codex's -c is last-wins per
// key, so appending ours would clobber it.
func TestInjectSystemPrompt_CodexExistingDeveloperInstructions(t *testing.T) {
	resolved := `codex -c 'developer_instructions=my custom prompt'`
	result := injectSystemPrompt(resolved)

	if result != resolved {
		t.Errorf("expected resolved form unchanged, got %q", result)
	}
	if got := strings.Count(result, "developer_instructions="); got != 1 {
		t.Errorf("expected exactly one developer_instructions flag, got %d in %q", got, result)
	}
}

func TestInjectSystemPrompt_Aider(t *testing.T) {
	result := injectSystemPrompt("aider")

	if result != "aider" {
		t.Errorf("expected aider unchanged (no system-prompt flag), got %q", result)
	}
}

func TestInjectSystemPrompt_Gemini(t *testing.T) {
	result := injectSystemPrompt("gemini")

	if result != "gemini" {
		t.Errorf("expected gemini unchanged (no system-prompt flag), got %q", result)
	}
}

// TestInjectSystemPrompt_ResolvedCommandMatrix pins #1116/#1131: which flags
// get injected is decided by the agent the RESOLVED command actually runs —
// through every override shape (bare name, absolute path, path+flags,
// redirect to a different agent, redirect to a non-agent binary) — never by
// the config-name key the command was resolved from. The non-agent rows are
// the class fix: injecting claude's --plugin-dir into e.g. bash makes it exit
// instantly and the spawn dies as an opaque timeout.
func TestInjectSystemPrompt_ResolvedCommandMatrix(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	tests := []struct {
		name     string
		resolved string
		want     string // "" = resolved must come back unchanged
	}{
		// name→name (no override) for all four agents.
		{"claude bare", "claude", "--plugin-dir"},
		{"codex bare", "codex", "developer_instructions="},
		{"aider bare", "aider", ""},
		{"gemini bare", "gemini", ""},

		// name→path and name→path+flags overrides.
		{"claude override path", "/opt/claude-next/bin/claude", "--plugin-dir"},
		{"claude override path with flags", "/opt/claude-next/bin/claude --model opus", "--plugin-dir"},
		{"codex override path with flags", "/usr/local/bin/codex --full-auto", "developer_instructions="},
		{"aider override path", "/usr/local/bin/aider --no-auto-commits", ""},
		{"gemini override path", "/usr/local/bin/gemini", ""},

		// name→other-agent: the RESOLVED agent's flags, not the key's.
		{"claude key resolved to codex gets codex flags", "codex --full-auto", "developer_instructions="},
		{"codex key resolved to claude gets claude flags", "/usr/bin/claude", "--plugin-dir"},

		// name→non-agent binary: no injection at all (#1116, #1131).
		{"claude key resolved to bash (#1131)", "bash", ""},
		{"claude key resolved to unknown tool (#1116)", "/usr/bin/some-other-tool --foo", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectSystemPrompt(tt.resolved)
			if tt.want == "" {
				if got != tt.resolved {
					t.Errorf("expected %q unchanged, got %q", tt.resolved, got)
				}
				return
			}
			if !strings.HasPrefix(got, tt.resolved) {
				t.Errorf("expected resolved form preserved as prefix, got %q", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected %q injected into %q, got %q", tt.want, tt.resolved, got)
			}
			// Exactly one agent's flags: claude's and codex's must never
			// both appear.
			if strings.Contains(got, "--plugin-dir") && strings.Contains(got, "developer_instructions=") {
				t.Errorf("both agents' flags injected: %q", got)
			}
		})
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
