package session

import (
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// A remote session has no local tmux binding, so resolvedAgentLocked's tmux
// arm can never fire — the override-resolved command reaches it only through
// the runtime evidence the remote launch boundary records (#5108). Before the
// fix, a remote session whose program_overrides entry pointed "claude" at a
// non-agent command detected "claude" off the enum, and waitForReady then spun
// the full 60s waiting for a prompt glyph a non-agent never prints (the
// isReadyContent "" arm that accepts any non-blank pane is pinned at the task
// level by TestWaitForReadyNonAgentBecomesReadyOnAnyOutput, #1131).
func TestResolvedAgent_RemoteRuntimeEvidence(t *testing.T) {
	tests := []struct {
		name string
		inst *Instance
		want string
	}{
		// The recorded launch command decides — including the "no known agent"
		// answer — exactly as a local pane's program does.
		{"remote override to non-agent (#5108)", &Instance{Program: tmux.ProgramClaude, runtimeProgram: "./bin/sidekick"}, ""},
		{"remote override to codex", &Instance{Program: tmux.ProgramClaude, runtimeProgram: "/opt/bin/codex --full-auto"}, tmux.ProgramCodex},
		{"remote no override", &Instance{Program: tmux.ProgramClaude, runtimeProgram: "claude"}, tmux.ProgramClaude},
		// Nothing recorded: fall back to the enum, as before.
		{"remote nothing resolved falls back to enum", &Instance{Program: tmux.ProgramClaude}, tmux.ProgramClaude},
		{"remote nothing resolved, gemini enum", &Instance{Program: tmux.ProgramGemini}, tmux.ProgramGemini},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.inst.ResolvedAgent(); got != tt.want {
				t.Errorf("ResolvedAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The remote twin of LocalBackend's setRuntimeLaunch: a remote runtime carries
// the override-resolved command it bound its agent-server to (the --program
// value, #5067's --program-resolved contract), and a successful launch RPC
// records it as the session's runtime evidence. Detection and identity then
// read the command the session actually runs, identically to a local session.
func TestRemoteLaunchRecordsResolvedProgram(t *testing.T) {
	tests := []struct {
		name        string
		program     string
		resolved    string
		wantAgent   string
		wantRuntime string
		// CurrentAgentName is an identity question, not a detection one: like a
		// local wrapper script, a recorded command that names no known agent
		// leaves the configured enum standing (handoff.go's precedence doc).
		wantIdentity string
	}{
		{"non-agent override (#5108)", tmux.ProgramClaude, "./bin/sidekick", "", "./bin/sidekick", tmux.ProgramClaude},
		{"real-agent override", tmux.ProgramClaude, "/opt/bin/codex --full-auto", tmux.ProgramCodex, "/opt/bin/codex --full-auto", tmux.ProgramCodex},
		{"no override", tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude, tmux.ProgramClaude},
		// A backend that bound no command (an out-of-contract hook) records
		// nothing, and detection falls back to the enum as before.
		{"nothing resolved", tmux.ProgramGemini, "", tmux.ProgramGemini, "", tmux.ProgramGemini},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := newRemoteLaunchInstance(tc.program, tc.resolved, &stubAgentServer{})

			require.NoError(t, inst.Start(true))

			assert.Equal(t, tc.wantRuntime, inst.RuntimeProgram())
			assert.Equal(t, tc.wantAgent, inst.ResolvedAgent())
			assert.Equal(t, tc.wantIdentity, inst.CurrentAgentName())
		})
	}
}

// A launch RPC that fails must leave no command claim behind: a runtime that
// never started has no "command the session runs" to detect from.
func TestRemoteLaunchFailureRecordsNoProgram(t *testing.T) {
	inst := newRemoteLaunchInstance(tmux.ProgramClaude, "./bin/sidekick",
		&failLaunchServer{err: errors.New("launch refused")})

	require.Error(t, inst.Start(true))
	assert.Equal(t, "", inst.RuntimeProgram())
	assert.Equal(t, tmux.ProgramClaude, inst.ResolvedAgent(),
		"nothing launched, so detection stays on the enum")
}

// newRemoteLaunchInstance builds the daemon-side shape NewInstance leaves
// behind for a remote session: a remote backend carrying the command its
// provisioner resolved, a remote client marker, and a stubbed agent-server so
// the launch boundary runs without dialing anything (agentSrv is the seam
// AgentServer() honors — agentserver_local.go's fast path returns a non-nil
// cache as-is).
func newRemoteLaunchInstance(program, resolved string, srv AgentServer) *Instance {
	return &Instance{
		Title:        "s",
		Program:      program,
		backend:      &dockerBackend{remoteAgentBackend: remoteAgentBackend{resolvedProgram: resolved}},
		remoteClient: &remoteAgentClient{title: "s"},
		agentSrv:     srv,
	}
}

// failLaunchServer is a stubAgentServer whose launch RPC fails.
type failLaunchServer struct {
	stubAgentServer
	err error
}

func (s *failLaunchServer) Launch(bool) error { return s.err }
