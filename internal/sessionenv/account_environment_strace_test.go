package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAccountEnvironmentCommand_RefusesUnprovableStraceChildEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"dynamic assignment word", `override=CODEX_HOME=/other; strace env "$override" codex`},
		{"short split-string", "strace env -SCODEX_HOME=/other codex"},
		{"long split-string", "strace env --split-string=CODEX_HOME=/other codex"},
		{"ANSI-C quoted assignment", "strace env $'CODEX_HOME=/other' codex"},
		{"ordinary nested env", "strace -f -o /tmp/trace env CODEX_HOME=/other codex"},
		{"strace separate env option", "strace -E CODEX_HOME=/other codex"},
		{"strace attached env option", "strace -ECODEX_HOME=/other codex"},
		{"strace long env option", "strace --env=CODEX_HOME=/other codex"},
		{"strace unsets protected env", "strace -E CODEX_HOME codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"command %q can replace the selected account environment", test.command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceUnknownSyntaxFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
	}{
		{"unknown option", "strace --future-option npm run dev"},
		{"dynamic option operand", `strace -o "$AF_TRACE_FILE" npm run dev`},
		{"dynamic executable", `strace "$AF_TRACE_PROGRAM"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(test.command, scopedProcessTabAccount()),
				"unrecognised strace input %q must not default to safe", test.command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_RefusesExecutableStraceOutputTarget(t *testing.T) {
	for _, command := range []string{
		`strace --output='|env CODEX_HOME=/other codex' true`,
		`strace --output '!env CODEX_HOME=/other codex' true`,
		`strace -o'|env CODEX_HOME=/other codex' true`,
		`strace -o '!env CODEX_HOME=/other codex' true`,
	} {
		t.Run(command, func(t *testing.T) {
			require.Error(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
				"executable strace output target %q inherits the selected account environment", command)
		})
	}
}

func TestValidateAccountEnvironmentCommand_StraceShortOptionArity(t *testing.T) {
	t.Run("Y takes no value", func(t *testing.T) {
		require.Error(t,
			ValidateAccountEnvironmentCommand("strace -Y env CODEX_HOME=/other codex", scopedProcessTabAccount()),
			"argument-free -Y must not consume the env executable")
	})
	t.Run("s takes a value", func(t *testing.T) {
		require.NoError(t,
			ValidateAccountEnvironmentCommand("strace -s 256 npm run dev", scopedProcessTabAccount()),
			"value-taking -s must consume its string-limit operand")
	})
}

func TestValidateAccountEnvironmentCommand_FailClosedBoundaryStaysNarrow(t *testing.T) {
	for _, command := range []string{
		"echo CODEX_HOME=/tmp",
		"rg 'OPENAI_API_KEY=x'",
		"set +e - -k; npm run dev",
		"echo env CODEX_HOME=/tmp",
		"strace npm run dev",
		"strace echo CODEX_HOME=/tmp",
		"strace rg OPENAI_API_KEY=x",
		"strace -f -o /tmp/trace npm run dev",
		"strace --follow-forks --output=/tmp/trace npm run dev",
		"strace -ff -etrace=execve npm run dev",
		"strace -E PORT=3000 npm run dev",
		"strace -- env PORT=3000 npm run dev",
		"strace -p 1234",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q uses assignment-shaped text as data, not a child-environment mutation", command)
	}
}
