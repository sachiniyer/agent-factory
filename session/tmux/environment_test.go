package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentOverrideFromCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		key     string
		want    CommandEnvOverride
	}{
		{name: "leading assignment", command: "CODEX_HOME='/tmp/codex home' codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/codex home", Present: true, Set: true, Literal: true}},
		{name: "env assignment", command: "/usr/bin/env CODEX_HOME=/tmp/one codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/one", Present: true, Set: true, Literal: true}},
		{name: "last assignment wins", command: "CODEX_HOME=/tmp/one CODEX_HOME=/tmp/two codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "/tmp/two", Present: true, Set: true, Literal: true}},
		{name: "unset separate", command: "env -u CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "unset attached", command: "env --unset=CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "clear then restore", command: "env -i HOME=/tmp/isolated codex", key: "HOME", want: CommandEnvOverride{Value: "/tmp/isolated", Present: true, Set: true, Literal: true}},
		{name: "clear inherited variable", command: "env -i HOME=/tmp/isolated codex", key: "CODEX_HOME", want: CommandEnvOverride{Present: true, Literal: true}},
		{name: "dynamic value unknown", command: "CODEX_HOME=$ALT_CODEX_HOME codex", key: "CODEX_HOME", want: CommandEnvOverride{Value: "$ALT_CODEX_HOME", Present: true, Set: true, Literal: false}},
		{name: "inherited", command: "ionice -c 3 codex", key: "CODEX_HOME", want: CommandEnvOverride{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, EnvironmentOverrideFromCommand(tc.command, tc.key))
		})
	}
}
