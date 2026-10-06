package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// CommandEndsOptionsTerminator and CommandHasControlOperator are the two shapes
// that misroute the flags injectSystemPrompt appends to the END of the resolved
// program string. The first (a `--` end-of-options terminator, anywhere after an
// optional `exec` prefix) is the SILENT failure surface the bug is about; the
// second (a shell control operator, including a statement terminator) is a loud
// misrouting. These tables pin them against the same mvdan.cc/sh parser the
// account boundary uses, so the warning and the refusal can never come to
// different conclusions about the same string (the same invariant
// CommandUsesExecSeparator keeps).

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
		// A `--` ANYWHERE after the program demotes the appended flag to a
		// positional, not only the last word: `claude -- --resume` appends
		// `--plugin-dir` after `--resume`, but the `--` already made `--resume`
		// positional, so the appended flag is positional too.
		{"terminator in the middle demotes the appended flag", "claude -- --resume", true},
		{"terminator before a flag", "claude -- --resume --model opus", true},
		{"terminator as the only flag", "claude --model opus --resume", false},
		{"absolute path trailing terminator", "/usr/local/bin/claude --", true},
		{"exec prefix stripped, no terminator", "exec -- claude", false},
		{"exec prefix stripped, terminator remains", "exec -- claude --", true},
		{"exec prefix stripped, terminator in the middle", "exec -- claude -- --resume", true},
		{"double-quoted terminator is literal", `claude "--"`, true},
		{"single-quoted terminator is literal", "claude '--'", true},
		{"non-literal word is not a terminator", "claude --$FOO", false},
		{"non-literal flag value is not a terminator", "claude --model=--", false},
		{"redirect is ignored, terminator seen", "claude > /tmp/log --", true},
		{"stderr redirect ignored, terminator seen", "claude 2>/dev/null --", true},
		{"pipe is not a single call so not a terminator", "claude -- | tee", false},
		{"env-prefixed agent with terminator", "CLAUDE_CODE_USE_BEDROCK=1 claude --", true},
		{"env-prefixed agent with terminator in the middle", "CLAUDE_CODE_USE_BEDROCK=1 claude -- --resume", true},
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

		// A single simple call terminated by a statement separator (`;` or a
		// trailing newline) still misroutes an appended flag: `claude;` becomes
		// `claude; --plugin-dir …` and the flag runs as its own command. A `;` is
		// exposed on stmt.Semicolon (a `&`/`|&` is already caught above as
		// Background/Coprocess); a trailing newline is the other statement
		// terminator the parser folds into a single Stmt without a Semicolon, so
		// it is detected on the raw value.
		{"trailing semicolon", "claude;", true},
		{"trailing semicolon after flags", "claude --model opus;", true},
		{"trailing newline", "claude\n", true},
		{"trailing newline with flags", "claude --model opus\n", true},
		{"trailing whitespace then newline", "claude \t\n ", true},

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

// The two predicates are mutually exclusive on the shapes the warning pairs them
// on: a terminator requires a single simple call (a `--` word the parser would
// otherwise route into a compound), while a control operator requires NOT a
// single simple call (a `|`/`&&`/subshell) or a single call terminated by a `;`
// or newline (which has no `--` word). The one exception is the exec-prefix
// family, where the exec builtin is a call even with the separator — and that is
// already covered by CommandUsesExecSeparator. warnLaunchFlagMismap's switch
// takes the first matching kind, so even a value that trips both (a single call
// with a `--` AND a `;`, like `claude --;`) emits one warning, not two. This
// pins the invariant on the shapes the two predicates are paired on, so a future
// edit cannot accidentally make both fire on the same plain value.
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
