package tmux

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandEnvironmentFromCommand(t *testing.T) {
	workDir := filepath.Join(string(filepath.Separator), "launch")
	tests := []struct {
		name       string
		command    string
		key        string
		want       CommandEnvOverride
		wantDir    string
		wantExe    string
		wantAgent  string
		unknownDir bool
		launch     string
		wantErr    string
	}{
		{name: "leading assignment", command: "CODEX_HOME='/tmp/codex home' codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/codex home", Present: true, Set: true, Literal: true}},
		{name: "env assignment", command: "/usr/bin/env CODEX_HOME=/tmp/one codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/one", Present: true, Set: true, Literal: true}},
		{name: "last assignment wins", command: "CODEX_HOME=/tmp/one CODEX_HOME=/tmp/two codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/two", Present: true, Set: true, Literal: true}},
		{name: "unset separate", command: "env -u CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "unset attached short", command: "env -uCODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "unset attached long", command: "env --unset=CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "clear then restore", command: "env -i HOME=/tmp/isolated codex", key: "HOME", want: CommandEnvOverride{Value: "/tmp/isolated", Present: true, Set: true, Literal: true}},
		{name: "clear inherited variable", command: "env -i HOME=/tmp/isolated codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "chdir separate", command: "env -C /tmp codex", key: "CODEX_HOME", wantDir: "/tmp"},
		{name: "chdir attached", command: "env -Crelative codex", key: "CODEX_HOME", wantDir: filepath.Join(workDir, "relative")},
		{name: "nested env cwd", command: "env -C /tmp env -C child CODEX_HOME=rel codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "rel", Present: true, Set: true, Literal: true}, wantDir: "/tmp/child"},
		{name: "codex chdir separate", command: "codex -C /tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex chdir attached", command: "codex -C/tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex long chdir separate", command: "codex --cd /tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex long chdir attached", command: "codex --cd=/tmp", key: "CODEX_HOME", unknownDir: true},
		{name: "codex option delimiter", command: "codex -- -C", key: "CODEX_HOME"},
		{name: "inherited through wrapper", command: "ionice -c 3 codex", key: "CODEX_HOME", unknownDir: true},
		{name: "shell cd is unmodeled", command: "cd /tmp && codex", key: "CODEX_HOME", wantExe: "codex", wantAgent: ProgramCodex, unknownDir: true},
		{name: "opaque wrapper", command: "/opt/bin/my-agent-wrapper --ready", key: "CODEX_HOME", wantExe: "/opt/bin/my-agent-wrapper"},
		{name: "agent behind wrapper", command: "ionice -c 3 /opt/bin/codex", key: "CODEX_HOME", wantExe: "/opt/bin/codex", wantAgent: ProgramCodex, unknownDir: true},
		{name: "dynamic value refused", command: "CODEX_HOME=$ALT_CODEX_HOME codex", key: "CODEX_HOME", wantErr: "uses shell expansion"},
		{name: "dynamic chdir refused", command: "env -C $OTHER codex", key: "CODEX_HOME", wantErr: "unsupported env invocation"},
		{name: "unknown env option refused", command: "env --future-option codex", key: "CODEX_HOME", wantErr: "unknown option"},
		{name: "split string refused", command: "env -S CODEX_HOME=/tmp codex", key: "CODEX_HOME", wantErr: "split-string"},
		{name: "terminal env option refused", command: "env --help codex", key: "CODEX_HOME", wantErr: "without running a command"},
		{name: "relative launch directory refused", command: "codex", key: "CODEX_HOME", launch: "relative", wantErr: "launch directory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			launchDir := workDir
			if tc.launch != "" {
				launchDir = tc.launch
			}
			got, err := CommandEnvironmentFromCommand(tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Override(tc.key))
			if tc.wantExe != "" {
				require.Equal(t, tc.wantExe, got.Executable)
				require.Equal(t, tc.wantAgent, got.Agent)
			}
			wantDir := tc.wantDir
			if wantDir == "" {
				wantDir = workDir
			}
			require.Equal(t, wantDir, got.WorkingDir)
			require.Equal(t, !tc.unknownDir, got.WorkingDirKnown())
		})
	}
}

func TestCodexHomeFromCommandUsesEffectiveCwd(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	got, err := CodexHomeFromCommand("env -C /tmp CODEX_HOME=relative codex", launchDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(string(filepath.Separator), "tmp", "relative"), got)
}

// ConfigRootFromCommand is the one answer to "where does this launch read its
// config" (#4501): Codex capture and af's skill placement both take it, so the
// two agents' shapes and the daemon fallback are pinned here once.
func TestConfigRootFromCommand(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	t.Setenv("HOME", "/daemon-home")
	t.Setenv("CODEX_HOME", "/daemon-codex")
	t.Setenv("GEMINI_CLI_HOME", "")
	tests := []struct {
		name, agent, command, want, wantErr string
	}{
		{name: "codex inherits the daemon variable", agent: ProgramCodex, command: "codex", want: "/daemon-codex"},
		{name: "codex command variable wins", agent: ProgramCodex, command: "CODEX_HOME=/srv/codex codex", want: "/srv/codex"},
		{name: "codex HOME fallback names the config dir", agent: ProgramCodex, command: "env -u CODEX_HOME HOME=/h codex", want: "/h/.codex"},
		{name: "gemini empty daemon variable falls back to HOME", agent: ProgramGemini, command: "gemini", want: "/daemon-home"},
		{name: "gemini command variable wins", agent: ProgramGemini, command: "GEMINI_CLI_HOME=/srv/gemini gemini", want: "/srv/gemini"},
		{name: "gemini root is HOME-like", agent: ProgramGemini, command: "HOME=/h gemini", want: "/h"},
		{name: "gemini empty command variable falls back to HOME", agent: ProgramGemini, command: "GEMINI_CLI_HOME= HOME=/h gemini", want: "/h"},
		{name: "gemini relative variable uses launch cwd", agent: ProgramGemini, command: "env -C sub GEMINI_CLI_HOME=rel gemini", want: "/launch/sub/rel"},
		{name: "gemini dynamic variable refused", agent: ProgramGemini, command: "GEMINI_CLI_HOME=$X gemini", wantErr: "uses shell expansion"},
		{name: "gemini dynamic HOME refused", agent: ProgramGemini, command: "HOME=$X gemini", wantErr: "uses shell expansion"},
		{name: "gemini parse failure names the agent", agent: ProgramGemini, command: "env --future-option gemini", wantErr: "cannot resolve Gemini environment"},
		{name: "cleared environment names the variable", agent: ProgramGemini, command: "env -i gemini", wantErr: "GEMINI_CLI_HOME is unset and the launched command has no literal HOME fallback"},
		{name: "agent without a followed root", agent: ProgramClaude, command: "claude", wantErr: "does not follow a config root"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConfigRootFromCommand(tc.agent, tc.command, launchDir)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.FromSlash(tc.want), got)
		})
	}
}
