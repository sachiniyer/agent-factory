package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Legacy free-form Program values (persisted by pre-#659 binaries: absolute
// paths, optionally with flags) must keep their agent-specific flags on
// restore (#677). With no program_overrides entry for them, the free-form
// value resolves to itself, so the resolved-command detection (#1116) must
// recover the agent from those shapes too.

// Regression for #677: a restored Claude session whose persisted Program is a
// legacy absolute path must still receive --plugin-dir, or /af-* slash commands
// silently vanish after upgrade.
func TestInjectSystemPrompt_LegacyClaudePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	result := injectSystemPrompt("/home/foo/bin/claude")

	if !strings.Contains(result, "--plugin-dir") {
		t.Errorf("expected --plugin-dir for legacy claude path, got %q", result)
	}
}

// Companion to the Claude case: legacy Codex paths resolve to the codex agent, so
// they now get the codex FILE seam (skills folder, #1043 retired) — the launch
// command is left UNCHANGED and no developer_instructions blob is injected.
func TestInjectSystemPrompt_LegacyCodexPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "")

	result := injectSystemPrompt("/usr/local/bin/codex")

	if result != "/usr/local/bin/codex" {
		t.Errorf("expected legacy codex path unchanged (file seam), got %q", result)
	}
	if strings.Contains(result, "developer_instructions=") {
		t.Errorf("developer_instructions must no longer be injected, got %q", result)
	}
}

// Defensive guard: an unknown program path must NOT trigger any agent flag
// injection — we must never append Claude/Codex flags to a session we don't
// recognize.
func TestInjectSystemPrompt_UnknownProgramNoInjection(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	program := "/usr/bin/some-other-tool"
	result := injectSystemPrompt(program)

	if result != program {
		t.Errorf("expected unknown program left unchanged, got %q", result)
	}
}

// Regression guard: the bare enum value (what current binaries persist) must
// keep working exactly as before the normalization seam was added.
func TestInjectSystemPrompt_BareEnumStillInjects(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	result := injectSystemPrompt("claude")
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

// saveOverrideConfig writes a global config whose program_overrides redirect
// the claude enum to the given command, so resolveProgramForInstance exercises
// the real override-resolution path (instance path outside any git repo →
// global config applies).
func saveOverrideConfig(t *testing.T, claudeOverride string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: claudeOverride}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
}

// Regression for #1116/#1131: an AutoYes instance whose claude enum is
// overridden to a NON-claude command must not get the claude-only
// --permission-mode flag — the resolved program would exit on the unknown
// option and the spawn would die as an opaque timeout.
func TestResolveProgramForInstance_OverrideToNonAgentNoAutoYesFlag(t *testing.T) {
	saveOverrideConfig(t, "bash")

	i := &Instance{
		Title:   "cheap-instance",
		Program: tmux.ProgramClaude,
		Path:    t.TempDir(),
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if result != "bash" {
		t.Errorf("expected override resolved to bare %q with no injected flags, got %q", "bash", result)
	}
}

// Companion: an override that still runs claude (custom path + flags) must
// keep the AutoYes flag — keying off the resolved command must not LOSE
// flags for claude-compatible overrides.
func TestResolveProgramForInstance_OverrideToClaudePathKeepsAutoYesFlag(t *testing.T) {
	saveOverrideConfig(t, "/opt/claude-next/bin/claude --model opus")

	i := &Instance{
		Title:   "custom-claude",
		Program: tmux.ProgramClaude,
		Path:    t.TempDir(),
		AutoYes: true,
	}
	result := resolveProgramForInstance(i)
	if !strings.HasPrefix(result, "/opt/claude-next/bin/claude --model opus") {
		t.Errorf("expected override preserved as prefix, got %q", result)
	}
	if !strings.Contains(result, "--permission-mode bypassPermissions") {
		t.Errorf("expected AutoYes flag for claude-running override, got %q", result)
	}
}
