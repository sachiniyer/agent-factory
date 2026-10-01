package sessionenv

import (
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
		// Attached -E NAME[=value] and --env=NAME[=value] forms whose value
		// is a shell expansion: literalShellWord fails on the ParamExp, so
		// the strace -E/--env handler above was previously unreachable for
		// these. The literal NAME sits in the word's Lit prefix and names
		// the variable strace sets (or, when `$V` is unset, sets to "" —
		// which af's resolvers read as the ambient home), so the override
		// occurs for any expansion. The attached non-literal form must
		// fail closed the same way the separate-word form already does.
		{"strace -ECODEX_HOME=$V codex", true},
		{"strace --env=CODEX_HOME=$V codex", true},
		{`strace -ECODEX_HOME="$V" codex`, true},
		{"strace -EOPENAI_API_KEY=$V codex", true},
		{"strace -EBASH_ENV=$V codex", true},
		// A short-option cluster whose first value-taking flag is E
		// (-fECODEX_HOME=$V = -f -E CODEX_HOME=$V) and the unambiguous
		// abbreviation --en= of --env= are the same env option the
		// repository's strace model already recognizes, and override the
		// protected variable too, so they fail closed the same way.
		{"strace -fECODEX_HOME=$V codex", true},
		{`strace -fECODEX_HOME="$V" codex`, true},
		{"strace -fE$V codex", true},
		{"strace --en=CODEX_HOME=$V codex", true},
		// A NAME that is only a literal prefix continues into the
		// non-literal tail (no '=' ends it), so the expansion can complete
		// it into any denied name (e.g. V=HOME) — fail closed.
		{"strace -ECODEX_$V=/other codex", true},
		{"strace -ECODEX_HOME$V codex", true},
		// Fully-dynamic value: the whole operand after -E is a shell
		// expansion that can name or assign a protected variable.
		{"strace -E$V codex", true},
		// Reachable through xargs: unwrapXargs peels to the inner strace
		// invocation, whose attached non-literal option must still refuse.
		{"xargs strace -ECODEX_HOME=$V codex", true},
		{"xargs strace -E$V codex", true},
		// A harmless literal NAME with an UNQUOTED dynamic value still
		// refuses: the unquoted expansion is subject to word splitting and
		// can produce a second -E option the scan never sees as a separate
		// shell word (e.g. V="x -ECODEX_HOME=/other").
		{"strace -EFOO=$V codex", true},
		{"strace --env=FOO=$V codex", true},
		// The same harmless NAME with a QUOTED dynamic value stays one
		// option value (no word splitting), so it sets only the harmless
		// NAME and stays allowed, matching the attached literal -EFOO=1.
		{`strace -EFOO="$V" codex`, false},
		{`strace --env=FOO="$V" codex`, false},
		// The separate-word `-E <value>` form evaluates the value as one
		// word; a non-literal value is unprovable, so it still refuses —
		// pinning that this fix does not loosen the stricter separate form.
		{"strace -E FOO=$V codex", true},
		// strace stops parsing options at its traced command (or "--"), so
		// a -E/--env-shaped word after that command is the command's
		// argument, not a strace environment option, and stays allowed.
		{`strace echo -ECODEX_HOME=$V codex`, false},
		{`strace echo -ECODEX_HOME="$V" codex`, false},
		{`strace -- echo -ECODEX_HOME=$V codex`, false},
		// The option region does NOT end at a value word a value-taking
		// strace option consumes: -o consumes file as its output name, so
		// the -E CODEX_HOME that follows is still a strace option and
		// still refuses — pinning that the boundary fix keeps the override
		// the previous scan caught.
		{"strace -o file -E CODEX_HOME=/other codex", true},
		// Non-strace -E keeps its meaning: extended-regexp to grep.
		{"grep -ECODEX_HOME=$V /etc/environment", false},
		// A bare "-" is a non-option argv element (strace's traced
		// command, per straceInputRegion's own handling), so a
		// -E/--env-shaped word after it is the traced program's
		// argument, not a strace environment option, and stays allowed.
		{`strace - -ECODEX_HOME=$V codex`, false},
		{`strace - --env=CODEX_HOME=/other codex`, false},
		{`strace - -E CODEX_HOME=/other codex`, false},
		// The "-" boundary still lets a real env option before it refuse.
		{"strace -E CODEX_HOME=/other - codex", true},
		// A short-option cluster whose first value-taking flag is E at
		// the cluster's end (-fE … = -f -E …) and a long --env
		// abbreviation with no attached =value (--en …) take var[=val]
		// as the NEXT argv word, so a denied or non-literal operand there
		// overrides the traced child's protected variable the same way
		// the exact -E/--env form does.
		{"strace -fE CODEX_HOME=/other codex", true},
		{"strace -fE CODEX_HOME codex", true},
		{"strace -fE CODEX_$V=/other codex", true},
		{"strace --en CODEX_HOME=/other codex", true},
		{"strace --en CODEX_HOME codex", true},
		{"strace -fE FOO=1 codex", false},
		{"strace -fE FOO=$V codex", true},
		// The separate-word form refuses any non-literal value word,
		// matching the exact -E/--env form: `strace -fE FOO="$V"` is the
		// same as `strace -E FOO="$V"`, not the attached `-EFOO="$V"`.
		{`strace -fE FOO="$V" codex`, true},
		// A non-literal strace option word whose value is an UNQUOTED
		// expansion can word-split into a further strace option the scan
		// never sees as a separate word (strace -o$V with
		// V='out -ECODEX_HOME=/other' splits into -oout and
		// -ECODEX_HOME=/other), so any such option fails closed. A QUOTED
		// expansion stays one option value, so it keeps the tail judgment.
		{"strace -o$V codex", true},
		{"strace -p$V codex", true},
		{`strace -o"$V" codex`, false},
		// A "-" option prefix followed by an expansion can complete the
		// option spelling into -E/--env and name or assign a protected
		// variable (strace -"$V" codex with V=ECODEX_HOME=/other becomes
		// the single argv word -ECODEX_HOME=/other). The QUOTED form stays
		// one word but still completes the option; the UNQUOTED form can
		// also word-split. Both fail closed, unlike the bare "-" boundary.
		{`strace -"$V" codex`, true},
		{"strace -$V codex", true},
		// A value-taking strace option whose operand is the NEXT argv word
		// still fails closed when that value is a non-literal UNQUOTED
		// expansion: it can word-split into a further strace option the
		// scan never sees as a separate word (strace -o $V codex with
		// V='trace -ECODEX_HOME=/other' is passed as -o, trace,
		// -ECODEX_HOME=/other, codex). A QUOTED separate value stays one
		// option value, so it keeps the tail judgment.
		{"strace -o $V codex", true},
		{`strace -o "$V" codex`, false},
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

// The attached strace -E NAME[=value] and --env=NAME[=value] forms whose value
// is a shell expansion pass the production gate only by way of the predicate
// the previous test pins. Confirm the end-to-end guard refuses representative
// shapes (and the xargs-fronted variant that peels to the same inner strace
// invocation), whereas a non-denied NAME with a quoted dynamic value and a
// -E/--env-shaped word past strace's traced command stay accepted.
func TestValidateAccountEnvironmentCommand_RefusesStraceAttachedEnvExpansion(t *testing.T) {
	account := scopedProcessTabAccount()
	for _, command := range []string{
		"strace -ECODEX_HOME=$V codex",
		"strace --env=CODEX_HOME=$V codex",
		"strace -E$V codex",
		"strace -EBASH_ENV=$V codex",
		// Short-option clusters and unambiguous abbreviations are the
		// same strace env option and override the protected variable too.
		"strace -fECODEX_HOME=$V codex",
		"strace --en=CODEX_HOME=$V codex",
		// A partial literal NAME continued by an expansion can complete
		// into any denied name, so the attached form fails closed.
		"strace -ECODEX_$V=/other codex",
		// A harmless literal NAME with an UNQUOTED dynamic value still refuses:
		// the unquoted expansion can word-split into a second -E option.
		"strace -EFOO=$V codex",
		"strace --env=FOO=$V codex",
		// A "-" option prefix an expansion can complete into -E/--env
		// refuses (quoted or unquoted), unlike the bare "-" boundary.
		`strace -"$V" codex`,
		"strace -$V codex",
		// A value-taking option whose separate operand is an UNQUOTED
		// expansion refuses: the value can word-split into a further
		// strace env option.
		"strace -o $V codex",
		"xargs strace -ECODEX_HOME=$V codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, account)
		require.Error(t, err, "command %q sets a protected variable via a strace -E/--env expansion and must be refused", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
	for _, command := range []string{
		// A harmless literal NAME with a QUOTED dynamic value stays one
		// option value (no word splitting) and sets only that NAME, so the
		// attached non-literal form stays accepted.
		`strace -EFOO="$V" codex`,
		`strace --env=FOO="$V" codex`,
		// A -E/--env-shaped word after strace's traced command is the
		// command's argument, not a strace environment option.
		`strace echo -ECODEX_HOME=$V codex`,
		// A value-taking option whose separate operand is a QUOTED
		// expansion stays one option value, so it sets no protected
		// variable and stays allowed.
		`strace -o "$V" codex`,
		// grep's -E is extended-regexp and stays accepted.
		"grep -ECODEX_HOME=$V /etc/environment",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, account),
			"command %q sets no protected variable and must stay allowed", command)
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
