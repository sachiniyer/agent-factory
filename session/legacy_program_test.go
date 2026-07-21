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

// A legacy free-form program is returned unchanged. Approval flags are now the
// user's program_overrides choice; af does not append one based on agent type.
func TestResolveProgramForInstance_LegacyProgramUnchanged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	i := &Instance{
		Program: "/home/foo/bin/claude",
	}
	result := resolveProgramForInstance(i)
	if result != i.Program {
		t.Errorf("expected legacy program unchanged, got %q", result)
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

func TestResolveProgramForInstance_OverrideToNonAgent(t *testing.T) {
	saveOverrideConfig(t, "bash")

	i := &Instance{
		Title:   "cheap-instance",
		Program: tmux.ProgramClaude,
		Path:    t.TempDir(),
	}
	result := resolveProgramForInstance(i)
	if result != "bash" {
		t.Errorf("expected override resolved to bare %q with no injected flags, got %q", "bash", result)
	}
}

// A custom Claude command is also returned verbatim. This is where users put
// any approval mode they deliberately chose.
func TestResolveProgramForInstance_OverrideToClaudePathUnchanged(t *testing.T) {
	saveOverrideConfig(t, "/opt/claude-next/bin/claude --model opus")

	i := &Instance{
		Title:   "custom-claude",
		Program: tmux.ProgramClaude,
		Path:    t.TempDir(),
	}
	result := resolveProgramForInstance(i)
	if result != "/opt/claude-next/bin/claude --model opus" {
		t.Errorf("expected override unchanged, got %q", result)
	}
}
