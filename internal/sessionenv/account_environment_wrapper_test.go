package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Unmodeled argv-passthrough wrappers (strace, perf, valgrind, gdb --args,
// xargs, ...) run a child command and pass argv through, exactly like the
// modeled nohup/nice/timeout/setsid/stdbuf/ionice/taskset do. Before the fix,
// unwrapAccountCommand's closed wrapper list left such a binary in its default
// arm, returned its argv opaque, and unwrappedAccountCommandMutates's own
// default called it safe — so wrapping the modelled `env NAME=value <agent>`
// mutation in an unmodeled wrapper hid the inner assignment from the walk.
// `strace env CODEX_HOME=/other codex` was ACCEPTED while
// `nohup env CODEX_HOME=/other codex` was refused, over the same effect.
//
// The fix fails closed when an unmodeled wrapper's literal tail words carry a
// NAME=... assignment whose NAME is one af removes for the selected account,
// lifting envCallMutatesAccountEnvironment's NAME= rule one level. Verified
// against the installed bash and strace before this test was written.
func TestValidateAccountEnvironmentCommand_RefusesUnmodeledWrapperHiddenAssignment(t *testing.T) {
	for _, command := range []string{
		// The exact shapes from the report, all previously accepted.
		"strace env CODEX_HOME=/other codex",
		"perf env CODEX_HOME=/other codex",
		"valgrind env CODEX_HOME=/other codex",
		"gdb --args env CODEX_HOME=/other codex",
		"xargs -I{} env CODEX_HOME=/other codex",
		"strace -o /dev/null env CODEX_HOME=/other codex",
		// A credential name (not just the config dir) is hidden the same way.
		"strace env OPENAI_API_KEY=sk-stolen codex",
		"strace env CODEX_API_KEY=sk-stolen codex",
		"strace env CODEX_ACCESS_TOKEN=tok-stolen codex",
		// A shell-startup name is hidden the same way: it executes before the
		// agent reaches its exec shim.
		"strace env BASH_ENV=/tmp/evil codex",
		"strace env PROMPT_COMMAND='export CODEX_HOME=/other' codex",
		"strace env ZDOTDIR=/tmp/evil codex",
		// The assignment may appear anywhere in the wrapper's literal argv tail;
		// the wrapper's own options, the wrapped `env`, and the agent all stand
		// in front of it.
		"strace -ff -o trace env CODEX_HOME=/other codex",
		"perf stat -o /dev/null env CODEX_HOME=/other codex",
		"valgrind --leak-check=full env CODEX_HOME=/other codex",
		"gdb --batch --args env CODEX_HOME=/other codex",
		"xargs --replace={} env CODEX_HOME=/other codex",
		// An absolute-path wrapper is still an unmodeled wrapper.
		"/usr/bin/strace env CODEX_HOME=/other codex",
		// Quoted forms reduce to the same NAME=value argument after quote
		// removal, so they hide the same assignment.
		`strace env "CODEX_HOME=/other" codex`,
		"strace env 'CODEX_HOME=/other' codex",
		// A literal NAME with a dynamic value is still a hidden assignment.
		"strace env CODEX_HOME=$value codex",
		// A dynamic wrapper name does not make the hidden assignment safe.
		"$WRAPPER env CODEX_HOME=/other codex",
		// Nested behind a modelled wrapper: nohup unwraps to strace, which is
		// the unmodeled wrapper carrying the hidden assignment.
		"nohup strace env CODEX_HOME=/other codex",
		// Nested behind exec and the command builtin the same way.
		"exec strace env CODEX_HOME=/other codex",
		"command strace env CODEX_HOME=/other codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q hides a denied NAME= assignment behind an unmodeled wrapper", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The hidden-assignment fix must hold for every supported agent's credential
// names, config variable, and cloud-mode selectors — not just codex.
func TestValidateAccountEnvironmentCommand_RefusesUnmodeledWrapperAcrossAgents(t *testing.T) {
	cases := []struct {
		agent   string
		command string
	}{
		{"codex", "strace env CODEX_HOME=/other codex"},
		{"codex", "strace env OPENAI_API_KEY=sk codex"},
		{"claude", "strace env CLAUDE_CONFIG_DIR=/other claude"},
		{"claude", "strace env ANTHROPIC_API_KEY=sk claude"},
		{"claude", "strace env ANTHROPIC_AUTH_TOKEN=tok claude"},
		{"claude", "strace env CLAUDE_CODE_OAUTH_TOKEN=oauth claude"},
		// A cloud-mode selector is denied because it can redirect the agent off
		// the selected account.
		{"claude", "strace env CLAUDE_CODE_USE_BEDROCK=1 claude"},
		{"claude", "strace env CLAUDE_CODE_USE_VERTEX=1 claude"},
		{"claude", "strace env CLAUDE_CODE_USE_FOUNDRY=1 claude"},
		{"gemini", "strace env GEMINI_CLI_HOME=/other gemini"},
		{"gemini", "strace env GEMINI_API_KEY=key gemini"},
		{"gemini", "strace env GOOGLE_API_KEY=key gemini"},
		{"gemini", "strace env GOOGLE_APPLICATION_CREDENTIALS=/svc.json gemini"},
		{"gemini", "strace env GOOGLE_GENAI_USE_VERTEXAI=1 gemini"},
	}
	for _, test := range cases {
		account := Account{Agent: test.agent, Name: "work", Dir: "/afhome/accounts/" + test.agent + "/work"}
		err := ValidateAccountEnvironmentCommand(test.command, account)
		require.Error(t, err, "agent %q: command %q hides a denied assignment behind a wrapper",
			test.agent, test.command)
	}
}

// The wrapper guard must stay narrow. A process tab runs an arbitrary user
// command, so the fix latches onto the literal NAME= signal specifically
// rather than the wrapper's option spelling or a keyword scan. Each of these
// carries no denied NAME= word, so it must keep working exactly as before.
func TestValidateAccountEnvironmentCommand_WrapperGuardStaysNarrow(t *testing.T) {
	for _, command := range []string{
		// Noun-uses of the mutators as operands carry no NAME= word.
		"man env",
		"make env",
		"git grep env",
		"ls env/bin",
		"pip show env",
		"python3 -c 'print(\"env\")'",
		"strace -p 1234 env",
		"strace -p 1234",
		// An unmodeled wrapper around an ordinary command sets nothing.
		"strace npm run dev",
		"perf stat npm run dev",
		"valgrind ls",
		"gdb --args npm run dev",
		"xargs ls",
		"xargs -I{} ls",
		"strace -o /dev/null npm run dev",
		// A bare identity NAME without an `=` is an argument, not an
		// assignment token, so it does not trigger the NAME= rule.
		"strace echo CODEX_HOME",
		"strace env CODEX_HOME codex",
		// An ordinary (non-identity) NAME= assignment is still allowed.
		"strace env PORT=3000 npm start",
		"strace env PORT=3000 codex",
		"xargs -I{} env PORT=3000 npm start",
		// A NAME that merely shares a prefix with a denied name is not denied:
		// the guard matches the whole assignment target.
		"strace env CODEX_HOME_OTHER=warn codex",
		"strace make CODEX_HOME_OTHER=warn",
		// An invalid assignment target (empty or leading-digit name) is not a
		// NAME=value token env would honor.
		"strace env = codex",
		"strace env 1INVALID=x codex",
		// Modeled wrappers keep unwrapping to an ordinary command.
		"strace nice -n 5 npm run dev",
		"strace ionice -c 3 npm run dev",
		// A no-op assignment with no command is not the exploit shape.
		"strace",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q carries no denied NAME= word and must stay allowed", command)
	}
}

// commandMutatesAccountEnvironment is the predicate ValidateAccountEnvironmentCommand
// delegates to. Pin its verdict directly so the boolean signal — not just the
// end-to-end error — is locked against the report's evidence table.
func TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// Baselines the guard already had: bare and modeled-wrapper forms refuse.
		{"env CODEX_HOME=/other codex", true},
		{"nohup env CODEX_HOME=/other codex", true},
		{"nice env CODEX_HOME=/other codex", true},
		// Unmodeled wrappers now refuse too (the bug).
		{"strace env CODEX_HOME=/other codex", true},
		{"perf env CODEX_HOME=/other codex", true},
		{"valgrind env CODEX_HOME=/other codex", true},
		{"gdb --args env CODEX_HOME=/other codex", true},
		{"xargs -I{} env CODEX_HOME=/other codex", true},
		{"strace -o /dev/null env CODEX_HOME=/other codex", true},
		// A credential name is hidden the same way.
		{"strace env OPENAI_API_KEY=sk codex", true},
		// A shell-startup name is hidden the same way.
		{"strace env BASH_ENV=/tmp/evil codex", true},
		// A quoted form reduces to the same token after quote removal.
		{`strace env "CODEX_HOME=/other" codex`, true},
		{"strace env 'CODEX_HOME=/other' codex", true},
		// A literal NAME with a dynamic value is still a hidden assignment.
		{"strace env CODEX_HOME=$value codex", true},
		// Non-exploiting shapes stay safe: no denied NAME= word in the tail.
		{"strace npm run dev", false},
		{"strace -p 1234 env", false},
		{"man env", false},
		{"strace env PORT=3000 npm start", false},
		{"strace echo CODEX_HOME", false},
		{"strace env CODEX_HOME codex", false},
		// Class B (mutation inside a quoted -c script string) is a fundamental
		// limitation of static argv analysis and stays out of scope: an
		// unmodeled wrapper around `sh -c '...'` does NOT trip the NAME= rule
		// (there is no literal NAME= word — the override lives inside the quoted
		// script), and `strace` is not a known shell so the -c path does not fire
		// either. This documents the intentional boundary the fix leaves, per
		// the report. The modelled `sh -c 'unset CODEX_HOME; codex'` (no
		// wrapper) IS refused via shellCommandIsUnproven — verified separately.
		{"strace sh -c 'unset CODEX_HOME; codex'", false},
		{"sh -c 'unset CODEX_HOME; codex'", true}, // control: bare shell form is caught
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, codex)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// The end-to-end ApplyAccountEnvironment path is the production gate the exec
// shim calls. Confirm a hidden assignment behind an unmodeled wrapper is refused
// before any scoped environment is produced, mirroring the modeled-wrapper tests.
func TestApplyAccountEnvironment_RefusesUnmodeledWrapperHiddenAssignment(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"strace env CODEX_HOME=/other codex",
		"strace env OPENAI_API_KEY=sk-stolen codex",
		"strace env BASH_ENV=/tmp/evil codex",
		"gdb --args env CODEX_HOME=/other codex",
		"valgrind env CODEX_HOME=/other codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
}
