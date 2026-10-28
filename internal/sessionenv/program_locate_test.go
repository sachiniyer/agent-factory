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
		"":                     ErrNoProgramExecutable,
		"   ":                  ErrNoProgramExecutable,
		"FOO=1 BAR=two":        ErrNoProgramExecutable,
		"claude | tee":         ErrProgramNotLiteral,
		"$AGENT --x":           ErrProgramDynamic,
		"exec $AGENT":          ErrProgramDynamic,
		"env $AGENT":           ErrProgramDynamic,
		`PATH=$X claude`:       ErrProgramDynamic,
		`env PATH="$X" claude`: ErrProgramDynamic,
		`env -u "$N" claude`:   ErrProgramDynamic,
		"env":                  ErrProgramNotLiteral,
	} {
		_, err := LocateProgramExecutable(command)
		assert.ErrorIs(t, err, want, command)
	}
}
