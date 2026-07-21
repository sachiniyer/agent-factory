package tmux

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandEnvironmentFromCommand(t *testing.T) {
	workDir := filepath.Join(string(filepath.Separator), "launch")
	tests := []struct {
		name      string
		command   string
		key       string
		want      CommandEnvOverride
		wantDir   string
		wantExe   string
		wantAgent string
		launch    string
		wantErr   string
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
		{name: "inherited", command: "ionice -c 3 codex", key: "CODEX_HOME"},
		{name: "opaque wrapper", command: "/opt/bin/my-agent-wrapper --ready", key: "CODEX_HOME", wantExe: "/opt/bin/my-agent-wrapper"},
		{name: "agent behind wrapper", command: "ionice -c 3 /opt/bin/codex", key: "CODEX_HOME", wantExe: "/opt/bin/codex", wantAgent: ProgramCodex},
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
		})
	}
}

func TestCodexHomeFromCommandUsesEffectiveCwd(t *testing.T) {
	launchDir := filepath.Join(string(filepath.Separator), "launch")
	got, err := CodexHomeFromCommand("env -C /tmp CODEX_HOME=relative codex", launchDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(string(filepath.Separator), "tmp", "relative"), got)
}
