package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unmodeled argv-passthrough wrappers (strace, perf, valgrind, gdb --args,
// ...) run a child command and pass argv through, exactly like the modeled
// nohup/nice/timeout/setsid/stdbuf/ionice/taskset/xargs do. Before the fix,
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
		// A non-literal word in the wrapper's tail can expand to `env` (or to
		// a multiword env invocation after word splitting); the denied NAME=
		// that follows it is then a real override.
		"strace $W env CODEX_HOME=/other codex",
		"strace $W CODEX_HOME=/other codex",
		// An unrecognized wrapper's option can itself mutate the child's
		// environment — the --opt=DENIED shape mirrors xargs's
		// --process-slot-var=NAME and strace's -E var=val.
		"strace --setenv=CODEX_HOME codex",
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
		// Non-wrapper commands whose arguments resemble a NAME=value token are
		// not refused: the argument is never executed as an environment mutation.
		"echo CODEX_HOME=/tmp",
		"rg OPENAI_API_KEY= .",
		"grep CODEX_HOME= /etc/environment",
		"cat CODEX_HOME=/other",
		// A nested env with no command word is env's print mode: it mutates
		// only its own process's environment and execs nothing, so the tail
		// assignment overrides no running agent.
		"strace env CODEX_HOME=/other",
		// A dynamic tail word could expand to `env`, but with no command word
		// after the assignment the result is still print mode.
		"strace $W CODEX_HOME=/other",
		// xargs with a literal command whose argv cannot reach env: items and
		// substitutions land in the command's own arguments.
		"xargs",
		"xargs -0p echo",
		"xargs -n 2 env codex",
		"xargs -I{} env codex {}",
		"xargs --process-slot-var=PORT env codex",
		"xargs -I{} env PORT={} codex",
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
		// A shell word in an unmodeled wrapper's tail is judged by the same
		// shellCommandIsUnproven rule the modeled path applies at command
		// position, so a literal -c script is refused even inside the quotes —
		// `strace sh -c '...'` execs exactly what `nice sh -c '...'` does.
		{"strace sh -c 'unset CODEX_HOME; codex'", true},
		{"strace sh -c 'export CODEX_HOME=/x; codex'", true},
		{"strace bash -c 'CODEX_HOME=/x codex'", true},
		{"sh -c 'unset CODEX_HOME; codex'", true}, // control: bare shell form is caught
		// A trailing shell name with no argv after it has nothing to prove
		// against, so noun-uses stay allowed.
		{"strace sh", false},
		{"echo sh", false},
		{"man sh", false},
		{"strace -p 1234 sh", false},
		// A tail that parses as a real unproven shell invocation refuses even
		// under a non-wrapper head — the same trade `echo env X=y cmd` takes.
		{"echo sh -c 'unset CODEX_HOME'", true},
		// strace's own -E/--env option injects or removes the variable in the
		// traced child's environment — the mutation without an env word. The
		// separate-word, attached, and long forms all refuse; -E is matched
		// only under strace because it means extended-regexp elsewhere.
		{"strace -E CODEX_HOME=/other codex", true},
		{"strace -E CODEX_HOME codex", true},
		{"strace -ECODEX_HOME=/other codex", true},
		{"strace --env=CODEX_HOME=/other codex", true},
		{"strace --env CODEX_HOME=/other codex", true},
		{"grep -E 'CODEX_HOME=' /etc/environment", false},
		{"strace -E FOO=1 codex", false},
		{"strace --env=PORT=3000 codex", false},
		// An option word whose value is a denied NAME= assignment mutates the
		// child's environment even when the option is not strace's.
		{"unrecognized --setenv=CODEX_HOME=/other codex", true},
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

// xargs feeds content the static walk cannot see into the child's argv: input
// items append after the initial arguments, and -I/-i/--replace substitutes
// each marker occurrence with an input line. The modeled unwrap therefore
// refuses an env invocation whose operand region either mechanism can reach —
// env re-parses the supplied word, so an item spelling NAME=value is an
// override even when the marker occupied env's command slot.
func TestCommandMutatesAccountEnvironment_XargsModel(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	for _, test := range []struct {
		command string
		want    bool
	}{
		// A non-literal command word is unprovable: it can name env at
		// runtime and the tail is then env's argv.
		{"xargs $TOOL CODEX_HOME=/other codex", true},
		// Without substitution, input items append after the initial
		// arguments; env argv naming no command hands them env's operand
		// region.
		{"xargs env CODEX_HOME=/other", true},
		{"xargs env -uCODEX_HOME", true},
		// A literal denied assignment under the modeled env arm still refuses.
		{"xargs env CODEX_HOME=/other codex", true},
		{"xargs -n 2 env CODEX_HOME=/other codex", true},
		// -I/-i/--replace substitute each marker occurrence with an input
		// line, so a marker in env's operand region — including its command
		// slot — can expand to a NAME=value override.
		{"xargs -I{} env {} codex", true},
		{"xargs -I{} env {}=x codex", true},
		{"xargs -i env {} codex", true},
		{"xargs --replace={} env -u{} codex", true},
		{"xargs -I{} cmd env {} z", true},
		// A marker spelled by a non-literal option argument is unprovable.
		{`xargs -I "$M" env x codex`, true},
		// --process-slot-var sets a variable on every exec'd command.
		{"xargs --process-slot-var=CODEX_HOME env codex", true},
		{"xargs --process-slot-var CODEX_HOME env codex", true},
		{"xargs --process-slot-var=$VAR env codex", true},
		// An env nested inside the command's own argv is checked the same
		// way: appended items land at the end of the whole argv, which is
		// env's operand region when env names no command.
		{"xargs strace env", true},
		// Safe shapes: a literal command whose argv cannot feed env an
		// operand.
		{"xargs", false},
		{"xargs echo hi", false},
		{"xargs env codex", false},
		{"xargs -I{} env codex {}", false},
		{"xargs -I{} env PORT={} codex", false},
		{"xargs --process-slot-var=PORT env codex", false},
		{"xargs --version", false},
		// A terminal option no longer drops the tail: --help/--version now
		// inspect the words after them exactly as the default branch does.
		{"xargs --help env CODEX_HOME=/other codex", true},
		{"xargs --version env CODEX_HOME=/other codex", true},
		{"xargs --version sh -c 'unset CODEX_HOME; codex'", true},
		// Clean and childless tails after a terminal option stay admitted.
		{"xargs --version echo hi", false},
		{"xargs --help env codex", false},
	} {
		require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, codex),
			"command %q", test.command)
	}
}

// String-executor wrappers — watch, tmux new-session/new (and split-window),
// GNU screen, nix-shell --run/--command — exec a SINGLE quoted operand as
// `sh -c <script>` (or its tmux/screen/nix-shell analog). The whole script
// reaches the wrapper as ONE argv word, so the argv-passthrough tail scan
// above saw only that word and never the identity-variable mutation the
// inner shell ran one level deeper. `watch 'unset CODEX_HOME; codex'` was
// therefore ACCEPTED while the semantically identical
// `bash -c 'unset CODEX_HOME; codex'`, `watch bash -c 'unset CODEX_HOME; codex'`,
// and `strace sh -c 'unset CODEX_HOME; codex'` were REFUSED — over the same
// effect on the sibling tab's applied account environment. The same hole
// underlies ionice/taskset's design rule (account_environment_followup_test.go:
// "a scoped command must not treat them as opaque leaf programs whose
// arguments are inert"): a wrapper that "runs an arbitrary command" must not
// have its operand scored inert.
//
// The fix treats a literal tail word that carries a shell metacharacter
// (`;`, `|`, `&`, `$`, “ ` “, `'`, `"`) as an unproven shell script and
// fails closed, but only under a known string-executor wrapper. Each
// identity-mutating script below was ACCEPTED before the fix and is now
// refused by ValidateAccountEnvironmentCommand — confirmed against the
// installed `watch`/`tmux`/`screen`/`nix-shell` semantics (each documented
// to run its operand via the shell) before this test was written.
func TestValidateAccountEnvironmentCommand_RefusesStringExecutorWrapperScriptOperand(t *testing.T) {
	for _, command := range []string{
		// The exact exploitation shapes from the report's evidence table.
		"watch 'unset CODEX_HOME; codex'",
		"watch 'export CODEX_HOME=/other; codex'",
		"watch 'unset OPENAI_API_KEY; codex'",
		"watch 'unset BASH_ENV; codex'",
		"watch 'unset ZDOTDIR; codex'",
		"watch 'unset PROMPT_COMMAND; codex'",
		"tmux new-session 'unset CODEX_HOME; codex'",
		"tmux new-session -d 'unset CODEX_HOME; codex'",
		"tmux new 'unset CODEX_HOME; codex'",
		"tmux split-window 'unset CODEX_HOME; codex'",
		"screen 'unset CODEX_HOME; codex'",
		"screen -dmS sess 'unset CODEX_HOME; codex'",
		"nix-shell --run 'unset CODEX_HOME; codex'",
		"nix-shell --pure --run 'unset CODEX_HOME; codex'",
		"nix-shell --command 'unset CODEX_HOME; codex'",
		// watch flag variants: the metacharacter fires on the script word,
		// so the wrapper's own flags before it do not change the verdict.
		"watch -g 'unset CODEX_HOME; codex'",
		"watch -n 5 'unset CODEX_HOME; codex'",
		"watch -b 'unset CODEX_HOME; codex'",
		// Each shell metacharacter the rule fires on, paired with a mutation.
		"watch 'unset CODEX_HOME && codex'",
		"watch 'unset CODEX_HOME || codex'",
		"watch 'unset CODEX_HOME | codex'",
		"watch 'codex; export OPENAI_API_KEY=stolen'",
		"watch 'codex && export CODEX_HOME=/other'",
		"watch 'unset CODEX_HOME; codex'",
		// Quoted forms under the wrapper reduce to the same script after
		// outer-quote stripping, so the metacharacter scan sees them too.
		`watch "unset CODEX_HOME; codex"`,
		`tmux new-session "export CODEX_HOME=/other; codex"`,
		`screen "unset OPENAI_API_KEY; codex"`,
		`nix-shell --run "unset CODEX_HOME; codex"`,
		// Path-qualified wrapper names: isAccountCommandName matches by
		// basename, matching peelAccountWrapper's trust boundary.
		"/usr/bin/watch 'unset CODEX_HOME; codex'",
		"/usr/bin/tmux new-session 'unset CODEX_HOME; codex'",
		"./tmux new 'unset CODEX_HOME; codex'",
		"/usr/bin/screen 'unset CODEX_HOME; codex'",
		"./nix-shell --run 'unset CODEX_HOME; codex'",
		// Nested under a modeled wrapper: nohup/nice/timeout unwrap, leaving
		// watch at the head; watch is the unmodeled string-executor wrapper
		// and the tail scan refuses its metacharacter-bearing script operand.
		"nohup watch 'unset CODEX_HOME; codex'",
		"nice -n 5 watch 'unset CODEX_HOME; codex'",
		"timeout 10 watch 'unset CODEX_HOME; codex'",
		"exec watch 'unset CODEX_HOME; codex'",
		"command watch 'unset CODEX_HOME; codex'",
		// A shell-expansion metacharacter ($, backtick) inside the script is
		// unprovable exactly like a `;`-separated multi-statement.
		"watch 'codex; echo $CODEX_HOME'",
		"watch 'codex `echo hi`'",
		// A NAME= in front of a metacharacter-bearing shell script is still
		// the same exploit, not the leaf-noun shape — the wrapper execs the
		// whole script, not just `NAME=`.
		"watch 'X=1; unset CODEX_HOME; codex'",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q hides an identity-mutating shell script behind a string-executor wrapper", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// commandMutatesAccountEnvironment is the predicate ValidateAccountEnvironmentCommand
// delegates to. Pin its verdict directly so the boolean signal — not just the
// end-to-end error — is locked against the report's evidence table, including
// the narrow-safe controls that must keep passing and the unchanged behavior of
// non-string-executor wrappers.
func TestCommandMutatesAccountEnvironment_StringExecutorScriptOperand(t *testing.T) {
	names := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// Control baselines the guard already had: bare and modeled-wrapper
		// sh -c '...' forms refuse.
		{"bash -c 'unset CODEX_HOME; codex'", true},
		{"sh -c 'unset CODEX_HOME; codex'", true},
		{"strace sh -c 'unset CODEX_HOME; codex'", true},
		{"watch bash -c 'unset CODEX_HOME; codex'", true},
		{"sh 'unset CODEX_HOME; codex'", true},
		// String-executor wrappers now refuse the same script one level deep.
		{"watch 'unset CODEX_HOME; codex'", true},
		{"watch 'export CODEX_HOME=/other; codex'", true},
		{"watch 'unset OPENAI_API_KEY; codex'", true},
		{"watch 'unset BASH_ENV; codex'", true},
		{"watch 'unset ZDOTDIR; codex'", true},
		{"tmux new-session 'unset CODEX_HOME; codex'", true},
		{"tmux new-session -d 'unset CODEX_HOME; codex'", true},
		{"tmux new 'unset CODEX_HOME; codex'", true},
		{"tmux split-window 'unset CODEX_HOME; codex'", true},
		{"screen 'unset CODEX_HOME; codex'", true},
		{"screen -dmS sess 'unset CODEX_HOME; codex'", true},
		{"nix-shell --run 'unset CODEX_HOME; codex'", true},
		{"nix-shell --command 'unset CODEX_HOME; codex'", true},
		// Each metacharacter fires the rule.
		{"watch 'unset CODEX_HOME && codex'", true},
		{"watch 'unset CODEX_HOME || codex'", true},
		{"watch 'unset CODEX_HOME | codex'", true},
		{"watch 'codex; export OPENAI_API_KEY=stolen'", true},
		{"watch 'codex `unset CODEX_HOME`'", true},
		// Quoted forms reduce to the same script after quote removal.
		{`watch "unset CODEX_HOME; codex"`, true},
		{`tmux new-session "export CODEX_HOME=/other; codex"`, true},
		// Path-qualified wrapper-name spellings match by basename.
		{"/usr/bin/watch 'unset CODEX_HOME; codex'", true},
		{"./tmux new 'unset CODEX_HOME; codex'", true},
		// Nested behind a modeled wrapper: unwrap leaves watch at the head.
		{"nohup watch 'unset CODEX_HOME; codex'", true},
		{"nice -n 5 watch 'unset CODEX_HOME; codex'", true},
		{"timeout 10 watch 'unset CODEX_HOME; codex'", true},
		// Nested behind exec/command the same way.
		{"exec watch 'unset CODEX_HOME; codex'", true},
		{"command watch 'unset CODEX_HOME; codex'", true},
		// Narrow-safe controls that must keep passing. Each carries no
		// shell metacharacter, so the wrapper execs one bare command whose
		// identity neutrality the leaf-operand scan already proves, and a
		// NAME= without an inner env invocation is not a mutation.
		{"watch 'codex'", false},
		{"watch 'npm run dev'", false},
		{"watch 'echo CODEX_HOME=/tmp'", false},
		{"watch 'echo CODEX_HOME'", false},
		{"watch 'rg OPENAI_API_KEY= .'", false},
		{"watch 'tail -f /var/log/system.log'", false},
		{"watch -g codex", false},
		{"watch -n 5 'npm run dev'", false},
		{"watch -g 'npm run dev'", false},
		{"tmux new-session 'npm run dev'", false},
		{"tmux new 'npm run dev'", false},
		{"tmux split-window 'npm run dev'", false},
		{"tmux new-session -s myname 'npm run dev'", false},
		{"screen 'npm run dev'", false},
		{"nix-shell --run 'npm run dev'", false},
		{"nix-shell --run codex", false},
		{"nix-shell --pure --run codex", false},
		// Non-script tmux/screen operands without metacharacters stay
		// admitted: the wrapper passes nothing to `sh -c` and the literal
		// is judged as a leaf operand.
		{"tmux ls", false},
		{"tmux kill-session -t sess", false},
		// Unchanged behavior for non-string-executor wrappers: strace/perf/
		// nice/etc. are argv-passthrough, so a single quoted 'X; Y' word is a
		// binary name strace execve()s (no such file → no mutation). The
		// existing scan already accepted these and the new string-executor
		// branch does not touch them.
		{"strace 'unset CODEX_HOME; codex'", false},
		{"strace 'env CODEX_HOME=1 codex'", false},
		{"perf 'unset CODEX_HOME; codex'", false},
		{"nice -n 5 'unset CODEX_HOME; codex'", false},
		{"echo 'unset CODEX_HOME; codex'", false},
		// The existing wrapper-tail controls are unaffected: noun-uses,
		// modeled-wrapper env invocations, strace -E/--env, xargs markers,
		// and all the shapes in TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment.
		{"strace env CODEX_HOME=/other codex", true},
		{"strace -E CODEX_HOME=/other codex", true},
		{"strace --env=CODEX_HOME=/other codex", true},
		{"xargs -I{} env CODEX_HOME=/other codex", true},
		{"unrecognized --setenv=CODEX_HOME=/other codex", true},
		{"strace npm run dev", false},
		{"strace -p 1234 env", false},
		{"man env", false},
		{"strace env PORT=3000 npm start", false},
		{"strace echo CODEX_HOME", false},
		{"strace env CODEX_HOME codex", false},
		{"strace env CODEX_HOME=/other", false},
		{"strace $W CODEX_HOME=/other", false},
		{"xargs --process-slot-var=PORT env codex", false},
		{"strace --env=PORT=3000 codex", false},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, names)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// The end-to-end ApplyAccountEnvironment path is the production gate the exec
// shim calls. Confirm a string-executor-wrapped identity mutation is refused
// before any scoped environment is produced, mirroring the modeled-wrapper
// ApplyAccountEnvironment tests.
func TestApplyAccountEnvironment_RefusesStringExecutorWrapperScriptOperand(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"watch 'unset CODEX_HOME; codex'",
		"watch 'export CODEX_HOME=/other; codex'",
		"watch 'unset OPENAI_API_KEY; codex'",
		"watch 'unset BASH_ENV; codex'",
		"tmux new-session 'unset CODEX_HOME; codex'",
		"tmux new 'unset CODEX_HOME; codex'",
		"screen 'unset CODEX_HOME; codex'",
		"nix-shell --run 'unset CODEX_HOME; codex'",
		"nohup watch 'unset CODEX_HOME; codex'",
		"nice -n 5 watch 'unset CODEX_HOME; codex'",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
	for _, command := range []string{
		"watch 'codex'",
		"watch 'npm run dev'",
		"watch 'echo CODEX_HOME=/tmp'",
		"tmux new-session 'npm run dev'",
		"screen 'npm run dev'",
		"nix-shell --run 'npm run dev'",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.NoError(t, err, "command %q carries no identity mutation and must stay allowed", command)
	}
}

// The wrapper guard must hold for every supported agent's config variable,
// credential names, and cloud-mode selectors — not just codex. Each agent's
// account is constructed through the same ValidateAccountEnvironmentCommand
// entry point that feeds the production names map, so a string-executor
// wrapper hides their identity mutation the same way `strace env X=y <agent>`
// did under TestValidateAccountEnvironmentCommand_RefusesUnmodeledWrapperAcrossAgents.
func TestValidateAccountEnvironmentCommand_StringExecutorWrappersAcrossAgents(t *testing.T) {
	cases := []struct {
		agent   string
		command string
	}{
		{"codex", "watch 'unset CODEX_HOME; codex'"},
		{"codex", "watch 'unset OPENAI_API_KEY; codex'"},
		{"codex", "watch 'unset CODEX_API_KEY; codex'"},
		{"codex", "watch 'unset CODEX_ACCESS_TOKEN; codex'"},
		{"codex", "tmux new-session 'unset CODEX_HOME; codex'"},
		{"codex", "nix-shell --run 'unset OPENAI_API_KEY; codex'"},
		{"claude", "watch 'unset CLAUDE_CONFIG_DIR; claude'"},
		{"claude", "watch 'unset ANTHROPIC_API_KEY; claude'"},
		{"claude", "watch 'unset ANTHROPIC_AUTH_TOKEN; claude'"},
		{"claude", "watch 'unset CLAUDE_CODE_OAUTH_TOKEN; claude'"},
		{"claude", "tmux new-session 'unset CLAUDE_CONFIG_DIR; claude'"},
		{"claude", "nix-shell --run 'unset ANTHROPIC_API_KEY; claude'"},
		{"gemini", "watch 'unset GEMINI_CLI_HOME; gemini'"},
		{"gemini", "watch 'unset GEMINI_API_KEY; gemini'"},
		{"gemini", "watch 'unset GOOGLE_API_KEY; gemini'"},
		{"gemini", "tmux new-session 'unset GEMINI_CLI_HOME; gemini'"},
		{"gemini", "nix-shell --run 'unset GEMINI_API_KEY; gemini'"},
	}
	for _, test := range cases {
		account := Account{Agent: test.agent, Name: "work", Dir: "/afhome/accounts/" + test.agent + "/work"}
		err := ValidateAccountEnvironmentCommand(test.command, account)
		require.Error(t, err, "agent %q: command %q hides an identity mutation behind a string-executor wrapper",
			test.agent, test.command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"agent %q: command %q must be refused by the account-environment guard",
			test.agent, test.command)
	}
}

// A cloud-mode selector carries an identity: it can redirect the agent off the
// selected account (CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY,
// GOOGLE_GENAI_USE_VERTEXAI). A string-executor wrapper hides a `;`-separated
// mutation of a selector exactly the same way it hides a config-root
// mutation, so the fix must cover them too.
func TestValidateAccountEnvironmentCommand_StringExecutorWrappersCoverCloudSelectors(t *testing.T) {
	for _, command := range []string{
		"watch 'unset CLAUDE_CODE_USE_BEDROCK; codex'",
		"watch 'unset CLAUDE_CODE_USE_VERTEX; codex'",
		"tmux new-session 'export CLAUDE_CODE_USE_BEDROCK=1; claude'",
		"screen 'unset GOOGLE_GENAI_USE_VERTEXAI; gemini'",
	} {
		account := Account{
			Agent: accountAgentForCommand(command), Name: "work",
			Dir: "/afhome/accounts/" + accountAgentForCommand(command) + "/work",
		}
		err := ValidateAccountEnvironmentCommand(command, account)
		require.Error(t, err, "command %q hides a cloud-selector mutation behind a string-executor wrapper", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
}

// accountAgentForCommand picks the agent whose names the named wrapper
// attempts to mutate, so the cloud-selector test above runs each shape
// through its own agent's account (rather than a single codex bucket).
func accountAgentForCommand(command string) string {
	switch {
	case strings.Contains(command, "GOOGLE_") || strings.Contains(command, "GEMINI"):
		return "gemini"
	case strings.Contains(command, "ANTHROPIC_") || strings.Contains(command, "CLAUDE_"):
		return "claude"
	default:
		return "codex"
	}
}
