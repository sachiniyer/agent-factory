package tmux

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// DetectAgentFromCommand is the seam every agent-conditional spawn/restore
// behavior keys off (#1116, #1131): it must identify the agent a resolved
// command will actually run — through override paths, trailing flags, and
// wrapper prefixes — and return "" for anything that runs no known agent, so
// no agent flags or readiness heuristics ever leak onto a non-agent command.
func TestDetectAgentFromCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		// Bare enums (the common no-override case).
		{"bare claude", "claude", ProgramClaude},
		{"bare codex", "codex", ProgramCodex},
		{"bare aider", "aider", ProgramAider},
		{"bare gemini", "gemini", ProgramGemini},
		{"bare amp", "amp", ProgramAmp},
		{"bare opencode", "opencode", ProgramOpencode},
		{"bare devin", "devin", ProgramDevin},

		// Override / legacy shapes: absolute paths, flags, quoting.
		{"claude abs path", "/home/foo/bin/claude", ProgramClaude},
		{"claude path with flags", "/home/foo/bin/claude --plugin-dir x", ProgramClaude},
		{"codex path with flags", "/usr/local/bin/codex --full-auto", ProgramCodex},
		{"amp path with flags", "/home/user/.amp/bin/amp --no-ide", ProgramAmp},
		{"quoted claude path", "'/opt/my tools/claude' --model opus", ProgramClaude},
		{"uppercase base matches", "/opt/bin/Claude", ProgramClaude},
		// opencode installs to ~/.opencode/bin/opencode by default, so the
		// absolute-path shape is the COMMON case for it, not an exotic one.
		{"opencode abs path", "/home/foo/.opencode/bin/opencode", ProgramOpencode},
		{"opencode path with flags", "/home/foo/.opencode/bin/opencode --model anthropic/claude-opus-4-5", ProgramOpencode},
		{"quoted opencode path", "'/opt/my tools/opencode' --continue", ProgramOpencode},
		{"uppercase opencode base matches", "/opt/bin/OpenCode", ProgramOpencode},

		// Wrapper prefixes still match (#742 precedent from resumeProgram).
		{"ionice wrapper", "ionice -c 3 claude", ProgramClaude},
		{"env wrapper", "env FOO=1 gemini --resume latest", ProgramGemini},
		{"assignment value is not an agent", "CODEX_HOME=/tmp/codex claude", ProgramClaude},
		{"env operand is not an agent", "env -C /opt/codex claude", ProgramClaude},
		{"amp env wrapper", "env AMP_URL=https://ampcode.com amp --no-notifications", ProgramAmp},
		{"opencode ionice wrapper", "ionice -c 3 opencode", ProgramOpencode},
		{"opencode env wrapper", "env FOO=1 opencode --continue", ProgramOpencode},

		// devin: bare, absolute path, the af-injected trust flag, and a wrapper.
		// The default install symlinks ~/.local/bin/devin at a versioned path, so
		// the absolute-path shape is common for it.
		{"devin abs path", "/home/foo/.local/bin/devin", ProgramDevin},
		{"devin with trust flag", "devin --respect-workspace-trust false", ProgramDevin},
		{"devin path with flags", "/home/foo/.local/bin/devin --permission-mode accept-edits --respect-workspace-trust false", ProgramDevin},
		{"quoted devin path", "'/opt/my tools/devin' --continue", ProgramDevin},
		{"devin ionice wrapper", "ionice -c 3 devin", ProgramDevin},
		{"devin env wrapper", "env DEVIN_PERMISSION_MODE=smart devin", ProgramDevin},

		// Non-agent commands: no agent behavior may attach to these.
		{"bare shell (#1131)", "bash", ""},
		{"unknown tool", "/usr/bin/some-other-tool", ""},
		{"unknown tool with flags (#1116)", "/usr/bin/some-other-tool --foo", ""},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"substring but not base", "/opt/claude-wrapper/run", ""},
		{"agent name inside quoted arg", "bash -c 'claude --help'", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectAgentFromCommand(tt.command); got != tt.want {
				t.Errorf("DetectAgentFromCommand(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestDetectAgentExecutableAgreesWithAgentNamespaceForCommand is the shared
// invariant the #4356 narrowing broke. Before #4356, AgentForCommand matched env
// by basename, the same rule DetectAgentExecutable uses at the create gate, so a
// program the gate accepted was always classifiable on restore. #4356 tightened
// AgentForCommand to the strict isTrustedEnvExecutable set while the gate kept the
// basename rule, and the two surfaces drifted: a path-qualified env wrapper such
// as /usr/local/bin/env ... codex was accepted onto a session but could no longer
// be classified by the VS Code editor scope, so a restored session's editor
// refused with ErrUnsupportedAgent. AgentNamespaceForCommand restores the basename
// rule for the namespace-only callers; this test pins the agreement the gate and
// the editor scope must keep so the same drift cannot return silently.
func TestDetectAgentExecutableAgreesWithAgentNamespaceForCommand(t *testing.T) {
	for _, command := range []string{
		"env codex",
		"/bin/env codex",
		"/usr/bin/env codex",
		"/usr/local/bin/env CODEX_HOME=/x codex",
		"/run/current-system/sw/bin/env codex",
		"./env codex",
		"/tmp/env CLAUDE_CONFIG_DIR=/y claude",
		"env -i HOME=/h gemini --resume latest",
		"codex",
		"/opt/bin/claude --permission-mode plan",
		"bash",
		"/usr/bin/some-other-tool --foo",
		"",
	} {
		gate := DetectAgentExecutable(command)
		scope := sessionenv.AgentNamespaceForCommand(command)
		if gate != scope {
			t.Errorf("create gate and editor scope disagree on %q: DetectAgentExecutable=%q AgentNamespaceForCommand=%q",
				command, gate, scope)
		}
	}
}
