package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// DetectAgentFromProgram is the normalization seam that lets restored sessions
// with legacy free-form Program values keep their agent-specific flags (#677).
// It must recover the canonical agent for paths and path-plus-flags, leave the
// bare enum untouched, and — critically — return unknown programs unchanged so
// no agent flags leak into a non-agent session.
func TestDetectAgentFromProgram(t *testing.T) {
	tests := []struct {
		name    string
		program string
		want    string
	}{
		{"bare claude enum", "claude", tmux.ProgramClaude},
		{"bare codex enum", "codex", tmux.ProgramCodex},
		{"bare aider enum", "aider", tmux.ProgramAider},
		{"bare gemini enum", "gemini", tmux.ProgramGemini},
		{"legacy claude path", "/home/foo/bin/claude", tmux.ProgramClaude},
		{"legacy claude path with flags", "/home/foo/bin/claude --plugin-dir x", tmux.ProgramClaude},
		{"legacy codex path", "/usr/local/bin/codex", tmux.ProgramCodex},
		{"unknown tool path unchanged", "/usr/bin/some-other-tool", "/usr/bin/some-other-tool"},
		{"unknown tool with flags unchanged", "/usr/bin/some-other-tool --foo", "/usr/bin/some-other-tool --foo"},
		{"empty unchanged", "", ""},
		{"substring-but-not-base unchanged", "/opt/claude-wrapper/run", "/opt/claude-wrapper/run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectAgentFromProgram(tt.program); got != tt.want {
				t.Errorf("DetectAgentFromProgram(%q) = %q, want %q", tt.program, got, tt.want)
			}
		})
	}
}

// Regression for #677: a restored Claude session whose persisted Program is a
// legacy absolute path must still receive --plugin-dir, or /af-* slash commands
// silently vanish after upgrade.
func TestInjectSystemPrompt_LegacyClaudePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	legacyPath := "/home/foo/bin/claude"
	result := injectSystemPrompt(legacyPath, legacyPath, "legacy-session", dir)

	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir for legacy claude path, got %q", result)
	}
}

// Companion to the Claude case: legacy Codex paths must still receive the
// developer_instructions flag.
func TestInjectSystemPrompt_LegacyCodexPath(t *testing.T) {
	dir := t.TempDir()
	legacyPath := "/usr/local/bin/codex"
	result := injectSystemPrompt(legacyPath, legacyPath, "legacy-codex", dir)

	if !strings.Contains(result, "developer_instructions=") {
		t.Errorf("expected developer_instructions for legacy codex path, got %q", result)
	}
}

// Defensive guard: an unknown program path must NOT trigger any agent flag
// injection — we must never append Claude/Codex flags to a session we don't
// recognize.
func TestInjectSystemPrompt_UnknownProgramNoInjection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	program := "/usr/bin/some-other-tool"
	result := injectSystemPrompt(program, program, "mystery-session", dir)

	if result != program {
		t.Errorf("expected unknown program left unchanged, got %q", result)
	}
}

// Regression guard: the bare enum value (what current binaries persist) must
// keep working exactly as before the normalization seam was added.
func TestInjectSystemPrompt_BareEnumStillInjects(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)

	result := injectSystemPrompt(tmux.ProgramClaude, "claude", "enum-session", dir)
	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir for bare claude enum, got %q", result)
	}
}

// Regression for #677: a restored AutoYes Claude session with a legacy path
// must still get --permission-mode bypassPermissions, or unattended operation
// silently breaks after upgrade.
func TestResolveProgramForInstance_LegacyAutoYes(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	i := &Instance{
		Program: "/home/foo/bin/claude",
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if !strings.Contains(result, "--permission-mode bypassPermissions") {
		t.Errorf("expected --permission-mode bypassPermissions, got %q", result)
	}
}

// Regression for #818: pre-#659 binaries appended --permission-mode
// bypassPermissions at create-time and persisted the full string into
// Instance.Program, so a restored legacy AutoYes session already carries the
// flag — the restore-time append must not duplicate it.
func TestResolveProgramForInstance_LegacyAutoYesFlagAlreadyPresent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	i := &Instance{
		Program: "/home/foo/bin/claude --permission-mode bypassPermissions",
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if got := strings.Count(result, "--permission-mode"); got != 1 {
		t.Errorf("expected exactly one --permission-mode flag, got %d in %q", got, result)
	}
}

// Companion to the #818 case: the =-attached spelling must also suppress the
// append.
func TestResolveProgramForInstance_LegacyAutoYesEqualsFormAlreadyPresent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	i := &Instance{
		Program: "/home/foo/bin/claude --permission-mode=bypassPermissions",
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if got := strings.Count(result, "--permission-mode"); got != 1 {
		t.Errorf("expected exactly one --permission-mode flag, got %d in %q", got, result)
	}
}

// Defensive guard for the AutoYes branch: an unknown AutoYes program must NOT
// get the Claude-only bypassPermissions flag.
func TestResolveProgramForInstance_UnknownAutoYesNoFlag(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	i := &Instance{
		Program: "/usr/bin/some-other-tool",
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if strings.Contains(result, "bypassPermissions") {
		t.Errorf("expected no bypassPermissions for unknown program, got %q", result)
	}
}
