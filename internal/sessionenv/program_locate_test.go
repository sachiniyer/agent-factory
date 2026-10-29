package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocateProgramExecutable(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    ProgramExecutable
		raw     string // the exact text [Start:End] must cut out
	}{
		{command: "claude --x", want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: "exec codex", want: ProgramExecutable{Name: "codex"}, raw: "codex"},
		{command: "exec -- codex -c x", want: ProgramExecutable{Name: "codex"}, raw: "codex"},
		{command: "FOO=1 BAR='a b' claude", want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: "PATH=/opt/bin claude", want: ProgramExecutable{Name: "claude", PATH: "/opt/bin", HasPATH: true}, raw: "claude"},
		{command: "PATH=/a PATH=/b claude", want: ProgramExecutable{Name: "claude", PATH: "/b", HasPATH: true}, raw: "claude"},
		{command: "env CLAUDE_CONFIG_DIR=/p claude --y", want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `env "VAR=$HOME/x" claude`, want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `env 'A=b' "B=$X" claude`, want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `PATH=$HOME/bin:$PATH /abs/claude`, want: ProgramExecutable{Name: "/abs/claude", PATHDynamic: true}, raw: "/abs/claude"},
		{command: `PATH=$X claude`, want: ProgramExecutable{Name: "claude", PATHDynamic: true}, raw: "claude"},
		{command: `env PATH="$X" claude`, want: ProgramExecutable{Name: "claude", PATHDynamic: true}, raw: "claude"},
		{command: `env "PATH=$X" claude`, want: ProgramExecutable{Name: "claude", PATHDynamic: true}, raw: "claude"},
		{command: `PATH=$X PATH=/ok claude`, want: ProgramExecutable{Name: "claude", PATH: "/ok", HasPATH: true}, raw: "claude"},
		{command: "env PATH=/e/bin claude", want: ProgramExecutable{Name: "claude", PATH: "/e/bin", HasPATH: true}, raw: "claude"},
		{command: "/usr/bin/env FOO=1 claude", want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `"/opt/my agent/claude" --x`, want: ProgramExecutable{Name: "/opt/my agent/claude"}, raw: `"/opt/my agent/claude"`},
		{command: "env -i claude", want: ProgramExecutable{Name: "claude", Opaque: true}, raw: "claude"},
		{command: "env -u PATH claude", want: ProgramExecutable{Name: "claude", Opaque: true}, raw: "claude"},
		{command: `claude --add-dir "$HOME/p" --x`, want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `FOO="$HOME" claude`, want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: `env CFG="$HOME/c" claude "$(date)"`, want: ProgramExecutable{Name: "claude"}, raw: "claude"},
		{command: "env -C /tmp claude", want: ProgramExecutable{Name: "claude", Opaque: true}, raw: "claude"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got, err := LocateProgramExecutable(tc.command)
			require.NoError(t, err)
			assert.Equal(t, tc.raw, tc.command[got.Start:got.End], "the offsets cut out exactly the executable word")
			got.Start, got.End = 0, 0
			assert.Equal(t, tc.want, got)
		})
	}

	for command, want := range map[string]error{
		"":                   ErrNoProgramExecutable,
		"   ":                ErrNoProgramExecutable,
		"FOO=1 BAR=two":      ErrNoProgramExecutable,
		"claude | tee":       ErrProgramNotLiteral,
		"$AGENT --x":         ErrProgramDynamic,
		"exec $AGENT":        ErrProgramDynamic,
		"env $AGENT":         ErrProgramDynamic,
		`env -u "$N" claude`: ErrProgramDynamic,
		"env":                ErrProgramNotLiteral,
		// An unquoted expansion field-splits into extra env operands (#5872).
		`env "VAR="$X claude`:  ErrProgramDynamic,
		`env VAR=$X claude`:    ErrProgramDynamic,
		`env VAR=${X} claude`:  ErrProgramDynamic,
		`env VAR=$(id) claude`: ErrProgramDynamic,
		`env VAR=* claude`:     ErrProgramDynamic,
	} {
		_, err := LocateProgramExecutable(command)
		assert.ErrorIs(t, err, want, command)
	}
}
