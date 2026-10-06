package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CommandEndsOptionsTerminator and CommandHasControlOperator are the two shapes
// that misroute the flags injectSystemPrompt appends to the END of the resolved
// program string. The first (a trailing lone --) is the SILENT failure surface
// the bug is about; the second (a shell control operator) is a loud misrouting.
// These tables pin them against the same mvdan.cc/sh parser the account boundary
// uses, so the warning and the refusal can never come to different conclusions
// about the same string (the same invariant CommandUsesExecSeparator keeps).

func TestCommandEndsOptionsTerminator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"bare agent has no terminator", "claude", false},
		{"agent with flags", "claude --model opus", false},
		{"trailing terminator", "claude --", true},
		{"trailing terminator after flags", "claude --model opus --", true},
		{"terminator in the middle is not trailing", "claude -- --resume", false},
		{"absolute path trailing terminator", "/usr/local/bin/claude --", true},
		{"exec prefix stripped, no trailing terminator", "exec -- claude", false},
		{"exec prefix stripped, trailing terminator remains", "exec -- claude --", true},
		{"double-quoted terminator is literal", `claude "--"`, true},
		{"single-quoted terminator is literal", "claude '--'", true},
		{"non-literal trailing word is not a terminator", "claude --$FOO", false},
		{"redirect is ignored, trailing terminator seen", "claude > /tmp/log --", true},
		{"stderr redirect ignored, trailing terminator seen", "claude 2>/dev/null --", true},
		{"pipe is not a single call so not a trailing terminator", "claude -- | tee", false},
		{"env-prefixed agent with trailing terminator", "CLAUDE_CODE_USE_BEDROCK=1 claude --", true},
		{"only terminator word", "--", true},
		{"empty string", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandEndsOptionsTerminator(tc.command)
			require.Equalf(t, tc.want, got, "CommandEndsOptionsTerminator(%q)", tc.command)
		})
	}
}

func TestCommandHasControlOperator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		// Single simple calls — no control operator.
		{"bare agent", "claude", false},
		{"agent with flags", "claude --model opus", false},
		{"absolute path", "/usr/local/bin/claude", false},
		{"env-prefixed agent", "CLAUDE_CODE_USE_BEDROCK=1 claude", false},
		{"exec prefix", "exec claude", false},
		{"exec separator", "exec -- claude", false},

		// Redirections do NOT misroute the flag (the agent still receives it),
		// so a single call with redirects is not a control operator.
		{"stdout redirect", "claude > /tmp/log", false},
		{"stderr redirect", "claude 2>/dev/null", false},
		{"stdin redirect", "claude < /tmp/in", false},
		{"redirect then trailing terminator", "claude > /tmp/log --", false},

		// Negation is not a control operator in the shell sense and does not
		// misroute the flag.
		{"negation", "! claude", false},

		// Actual control operators / compound constructs — misroute the flag.
		{"pipe", "claude | tee /tmp/log", true},
		{"and", "claude foo && claude bar", true},
		{"or", "claude foo || claude bar", true},
		{"semicolon", "claude; echo hi", true},
		{"background", "claude &", true},
		{"background with second command", "claude & echo hi", true},
		{"subshell", "(claude)", true},
		{"pipe with trailing terminator", "claude -- | tee", true},
		{"exec separator then pipe", "exec -- claude | tee", true},

		// Parse errors are not flagged: such a command fails loudly at launch for
		// a different reason.
		{"trailing pipe is a parse error", "claude |", false},
		{"empty string", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandHasControlOperator(tc.command)
			require.Equalf(t, tc.want, got, "CommandHasControlOperator(%q)", tc.command)
		})
	}
}

// The two predicates are mutually exclusive on a single command: a trailing
// terminator requires a single simple call, while a control operator requires
// NOT a single simple call. The one exception is the exec-prefix family, where
// the exec builtin is a call even with the separator — and that is already
// covered by CommandUsesExecSeparator. This pins the invariant so a future edit
// cannot accidentally make both fire on the same plain value, which would
// produce two confusing warnings for one problem.
func TestEndsOptionsTerminatorAndControlOperatorAreMutuallyExclusive(t *testing.T) {
	for _, command := range []string{
		"claude",
		"claude --model opus",
		"claude --",
		"claude --model opus --",
		"/usr/local/bin/claude --",
		"claude | tee /tmp/log",
		"claude foo && claude bar",
		"claude; echo hi",
		"claude &",
	} {
		trailing := CommandEndsOptionsTerminator(command)
		control := CommandHasControlOperator(command)
		require.Falsef(t, trailing && control,
			"%q matched both predicates; they are meant to be mutually exclusive", command)
	}
}
