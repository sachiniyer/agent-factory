package tmux

import "testing"

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
		{"amp env wrapper", "env AMP_URL=https://ampcode.com amp --no-notifications", ProgramAmp},
		{"opencode ionice wrapper", "ionice -c 3 opencode", ProgramOpencode},
		{"opencode env wrapper", "env FOO=1 opencode --continue", ProgramOpencode},

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
