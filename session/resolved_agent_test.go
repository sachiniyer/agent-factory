package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// ResolvedAgent is the seam WaitForReady and the trust-prompt gate key off
// (#1116, #1131): with a live tmux session it must report the agent from the
// command the pane actually runs (override-resolved), and only fall back to
// the persisted Program value when no tmux session exists yet.
func TestResolvedAgent(t *testing.T) {
	withTmuxProgram := func(program string) *Instance {
		ts := tmux.NewTmuxSessionWithDeps("resolved-agent", program, nil, nil)
		return &Instance{Program: tmux.ProgramClaude, Tabs: []*Tab{newAgentTab(ts)}}
	}

	tests := []struct {
		name string
		inst *Instance
		want string
	}{
		// tmux program is ground truth, regardless of the config-name enum.
		{"override to bash (#1131)", withTmuxProgram("bash"), ""},
		{"override to unknown tool (#1116)", withTmuxProgram("/usr/bin/some-other-tool --foo"), ""},
		{"override to codex", withTmuxProgram("/usr/local/bin/codex --full-auto"), tmux.ProgramCodex},
		{"override to amp", withTmuxProgram("/home/me/.amp/bin/amp --no-ide"), tmux.ProgramAmp},
		{"claude with injected flags", withTmuxProgram("claude --plugin-dir '/x/plugin'"), tmux.ProgramClaude},

		// No tmux session yet: fall back to the Program value, including
		// legacy free-form persisted shapes (#677).
		{"no tmux, bare enum", &Instance{Program: tmux.ProgramGemini}, tmux.ProgramGemini},
		{"no tmux, amp enum", &Instance{Program: tmux.ProgramAmp}, tmux.ProgramAmp},
		{"no tmux, legacy claude path", &Instance{Program: "/home/foo/bin/claude --plugin-dir x"}, tmux.ProgramClaude},
		{"no tmux, unknown program", &Instance{Program: "some-other-tool"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.inst.ResolvedAgent(); got != tt.want {
				t.Errorf("ResolvedAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}
