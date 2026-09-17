package sessionenv

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strace is modeled without an option grammar: the child boundary is never
// resolved, every suffix of the argv is judged as a command line, and a small
// hazard record judges the operands of the two option shapes that mutate or
// exec outside that argv. These cases pin the contract.
func TestCommandMutatesAccountEnvironment_StraceSuffixModel(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// -o/--output operands beginning with | or ! are spawned as commands
		// by strace itself — a mutation channel the suffix scan cannot see
		// because the operand is one shell word.
		{"strace -o '|env CODEX_HOME=/other codex' true", true},
		{"strace -o'|env CODEX_HOME=/other codex' true", true},
		{"strace --output='|env CODEX_HOME=/other codex' true", true},
		{"strace --output '|env CODEX_HOME=/other codex' true", true},
		{"strace -o '!env CODEX_HOME=/other codex' true", true},
		{"strace -o '|sh -c \"touch /tmp/pwn\"' true", true},
		{"strace -fo '|env CODEX_HOME=/other codex' true", true},
		// getopt_long resolves unambiguous prefixes of --output the same way;
		// ambiguous spellings exit in strace and refuse here as well.
		{"strace --out='|env CODEX_HOME=/other codex' true", true},
		// Ordinary file operands are benign: writing a trace is not a
		// mutation of the sibling environment.
		{"strace -o /tmp/trace.out codex", false},
		{"strace -o/tmp/trace.out codex", false},
		{"strace --output=/tmp/trace.out codex", false},
		{"strace -A -o /tmp/trace.out codex", false},
		{"strace --output-append-mode -o /tmp/trace.out codex", false},
		// -E/--env injects or removes a variable in the traced child; the
		// operand is judged like the env builtin's.
		{"strace -E CODEX_HOME=/other codex", true},
		{"strace -E CODEX_HOME codex", true},
		{"strace -ECODEX_HOME=/other codex", true},
		{"strace --env=CODEX_HOME=/other codex", true},
		{"strace --env CODEX_HOME=/other codex", true},
		{"strace --e CODEX_HOME=/other codex", true},
		{"strace -E PORT=3000 codex", false},
		{"strace --env=PORT=3000 codex", false},
		// -Y is argument-free in strace 6.8. The grammar-free model needs no
		// arity table to get this right: env is judged as a suffix, so the
		// option cannot smuggle the assignment past it.
		{"strace -Y env CODEX_HOME=/other codex", true},
		{"strace -Y -f env CODEX_HOME=/other codex", true},
		// A child hidden behind any option spelling is caught by its own
		// suffix, not by knowing where the options end.
		{"strace -e trace=%file env CODEX_HOME=/other codex", true},
		{"strace -s 128 env CODEX_HOME=/other codex", true},
		{"strace -p 1 -o /tmp/t env CODEX_HOME=/other codex", true},
		{"strace env CODEX_HOME=/other codex", true},
		{"strace sh -c 'export CODEX_HOME=/x; codex'", true},
		// A nested strace argv is itself a suffix judged by the same rules.
		{"strace strace -o '|env CODEX_HOME=/other codex' true", true},
		{"strace strace -E CODEX_HOME=/x env true", true},
		// A strace inside another unrecognized wrapper's tail keeps its own
		// hazard record — the pipe-output exec is still refused.
		{"perf strace -o '|env CODEX_HOME=/other codex' true", true},
		{"perf strace -E CODEX_HOME=/x true", true},
		{"perf strace -o /tmp/t true", false},
		// A non-literal word is unreducible: its expansion is a suffix the
		// scan cannot see, so the whole invocation fails closed.
		{"strace -o $f codex", true},
		{"strace $W codex", true},
		{"strace ${W:-env} CODEX_HOME=/other codex", true},
		// Unquoted glob metacharacters are not literals either: /bin/sh -c
		// expands them into different argv before strace runs, so a filename
		// such as CODEX_HOME=x or |cmd could silently become the operand.
		{"strace -E * codex", true},
		{"strace -o * codex", true},
		{"strace -o /tmp/*.out codex", true},
		{"strace -e 'trace=*' codex", false},
		{"strace -o '/tmp/*.out' codex", false},
		// An escaped metacharacter resolves to its literal value: \* is a
		// literal asterisk operand (benign), but \| resolves to | and still
		// trips the output-command hazard the raw string would have missed.
		{"strace -E \\* codex", false},
		{"strace -o \\|env CODEX_HOME=/other codex true", true},
		// A nested strace behind an unrecognized argv-passthrough wrapper runs
		// the full suffix model, not just the hazard record: its xargs child
		// is judged as a command line exactly as at top level.
		{"valgrind strace xargs -I{} env {} codex", true},
		{"perf strace xargs -I{} env {} codex", true},
		{"valgrind strace -o /tmp/t true", false},
		// Class-B over-refusals, priced by the design: an option operand that
		// spells a denied assignment mutates nothing as a filename or filter,
		// but the same words as a command-line suffix would — so they refuse.
		{"strace -o CODEX_HOME=x codex", true},
		{"strace -e CODEX_HOME=x codex", true},
		{"strace -e trace=env,CODEX_HOME=x codex", false},
		// Ordinary invocations stay allowed.
		{"strace codex", false},
		{"strace -e trace=%file codex", false},
		{"strace -p 1234", false},
		{"strace -f -s 128 codex --flag", false},
		{"strace -o /tmp/t nice -n 5 codex", false},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, codex)
		assert.Equal(t, test.want, got, "command %q", test.command)
	}
}

// The suffix scan judges every suffix of a nested strace argv as its own
// command, so nested wrappers converge exponentially in the worst case. The
// shared evaluation budget turns an attacker-sized nesting from a hang into a
// refusal; this pins both that bound and that an ordinary nesting still
// returns a verdict.
func TestCommandMutatesAccountEnvironment_StraceBudgetRefusal(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	// ~20 nested strace words needs ~2^20 suffix walks — far over budget — so
	// the walk must refuse rather than hang.
	deep := "strace " + strings.Repeat("strace ", 20) + "codex"
	require.True(t, commandMutatesAccountEnvironment(deep, codex),
		"attacker-sized nesting must refuse inside the budget")
	// A few nested layers stay decidable.
	require.False(t, commandMutatesAccountEnvironment("strace strace strace codex", codex))
}

// The meter counts argv words JUDGED, so a flat argv pays for the tail each
// suffix judgment re-walks — quadratic charging for quadratic work. An
// ordinary long argv (a compiler-style command with a few hundred file
// arguments) still fits; twice that length is four times the walk and does
// not. One slot per suffix saw neither: every length under
// straceFlatArgvLimit was admitted and the quadratic ran below the bound.
func TestCommandMutatesAccountEnvironment_StraceLongFlatArgv(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	assert.False(t, commandMutatesAccountEnvironment(
		"strace codex "+strings.Repeat("src/file.o ", 400), codex),
		"ordinary long strace argv must not exhaust the shared budget")
	assert.True(t, commandMutatesAccountEnvironment(
		"strace codex "+strings.Repeat("src/file.o ", 800), codex),
		"four times the walk must exhaust it")
}

// straceFlatArgvLimit is the O(1) early-out in front of that arithmetic: an
// argv past it is not an ordinary invocation and refuses before the hazard
// record reads a word.
func TestCommandMutatesAccountEnvironment_StraceFlatArgvLimit(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	flat := "strace codex " + strings.Repeat("src/file.o ", straceFlatArgvLimit)
	require.True(t, commandMutatesAccountEnvironment(flat, codex),
		"flat argv past the cap must refuse rather than do unbounded quadratic work")
}

// Wrapper and builtin names compare after shell escape resolution — `s\trace`
// is `strace` to /bin/sh, and a match that only reads the raw spelling misses
// it. Shell expansion is the same class: an unquoted glob, tilde, or brace
// word can split or rewrite argv before the command sees it, so it fails
// closed wherever a fixed literal is required.
func TestCommandMutatesAccountEnvironment_EscapeAndExpansionResolved(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// Backslash escapes resolve before name matching, at the head, in a
		// wrapper tail, and inside a strace argv's suffix judgments.
		{`s\trace -E CODEX_HOME codex`, true},
		{`un\set CODEX_HOME`, true},
		{`e\nv CODEX_HOME=/other codex`, true},
		{`valgrind s\trace -E CODEX_HOME codex`, true},
		{`strace s\h -c 'unset CODEX_HOME; codex'`, true},
		// An unquoted glob or brace in command position can expand to env or
		// to a same-shell mutator such as unset (u* in a directory holding a
		// file named "unset"), so the invocation fails closed.
		{`strace* -E CODEX_HOME codex`, true},
		{`e* CODEX_HOME=/other codex`, true},
		{`u* CODEX_HOME; codex`, true},
		// Unquoted brace expansion splits one word into several argv entries
		// even under POSIX parsing; a brace word carrying -E or -o operands
		// cannot be judged as the literal it spells.
		{`strace {-E,CODEX_HOME} codex`, true},
		{`e{nv,} CODEX_HOME=/other codex`, true},
		{`strace {-o,'|env CODEX_HOME=/x true'} codex`, true},
		// An unclosed '[' does not switch the rest of the word off. Brace
		// expansion is an earlier phase than pathname expansion, so bash reads
		// the ',' and the '}' whatever the '[' is doing — measured against
		// `bash --posix` in #4579, which the first row is the control for.
		{`{unset,echo} CODEX_HOME`, true},
		{`{unset,a[b} CODEX_HOME; codex`, true},
		{`{env,CODEX_HOME=/other[x} codex`, true},
		{`strace {-E,CODEX_HOME,a[b} codex`, true},
		// The same hole from the glob side: inside an unclosed '[' the '*' is
		// no bracket member, it is a live wildcard, so the operand is not the
		// literal it spells.
		{`strace -o /tmp/t[a* codex`, true},
		{`strace -E CODEX_HOME[a?b codex`, true},
		// Quoted braces never expand, '{}'-shaped words are not brace
		// expansions (xargs -I{} is its own marker syntax), and a leading ~
		// head stays accepted: it expands to a fixed absolute path that can
		// never name a same-shell builtin. And an unclosed '[' with no live
		// brace or glob syntax left in the word is still the literal text it
		// spells: the fix reaches the syntax the bracket was hiding, not every
		// word that happens to contain a '['.
		{`strace '{-E,CODEX_HOME}' codex`, false},
		{`xargs -I{} env codex {}`, false},
		{`~/bin/tool arg`, false},
		{`strace -o /tmp/t[unfinished codex`, false},
		{`strace -o /tmp/t[a\*b codex`, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, commandMutatesAccountEnvironment(tc.command, codex),
			"command %q", tc.command)
	}
}

// Quote removal runs before pathname expansion, so a glob bracket expression
// can span quoted and unquoted fragments: `["|"]` is the bracket `[|]` to
// /bin/sh even though the member lives in a quoted part. A per-literal scan
// misses it; the bracket state must carry across part boundaries.
func TestCommandMutatesAccountEnvironment_SpanningBracketGlob(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		// The operand strace -o executes when it begins with |: in a working
		// directory holding a file named `|env CODEX_HOME=x codex`, the word
		// expands to it whether the brackets are spelled with double quotes,
		// single quotes, or escapes.
		{`strace -o ["|"]env[" "]CODEX_HOME=x[" "]codex true`, true},
		{`strace -o ['|']env[' ']CODEX_HOME=x[' ']codex true`, true},
		{`strace -o ["\|"]env[" "]CODEX_HOME=x[" "]codex true`, true},
		// The same span can hide a denied -E operand behind a quoted member.
		{`strace -E CODEX["_"]HOME=x codex`, true},
		{`strace -E CODEX['"']HOME=x codex`, true},
		// And the whole command name: ["e"]nv is env wherever "e" exists.
		{`["e"]nv CODEX_HOME=/other codex`, true},
		{`[u]nset CODEX_HOME`, true},
		// Ordinary unquoted brackets already refused; spanning closes the
		// quoted-member variant of the same check.
		{`strace -o /tmp/t[!0-9] codex`, true},
		{`strace -o /tmp/t[0-9]x codex`, true},
		// An unclosed '[' is literal, a quoted ']' never closes, and an
		// escaped ']' is a member — those words are not globs.
		{`echo file[unfinished`, false},
		{`echo ["]x"`, false},
		{`echo [a\]b`, false},
		{`strace -o '/tmp/t[0-9]' codex`, false},
		{`strace -o /tmp/t\[0-9\] codex`, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, commandMutatesAccountEnvironment(tc.command, codex),
			"command %q", tc.command)
	}
}

// An expandable word in command position can resolve to a same-shell mutator
// the tail-as-env judgment cannot model: `u*` in a directory holding a file
// named "unset" runs `unset`, and a brace head picks any word it lists. The
// invocation fails closed; only a leading ~ stays provable because tilde
// expands to a fixed absolute path, never a bare builtin name.
func TestCommandMutatesAccountEnvironment_ExpandableCommandHead(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	cases := []struct {
		command string
		want    bool
	}{
		{`u* CODEX_HOME`, true},
		{`u* CODEX_HOME; codex`, true},
		{`e* CODEX_HOME=/other codex`, true},
		{`{unset,echo} CODEX_HOME`, true},
		{`nice u* CODEX_HOME`, true},
		{`strace u* CODEX_HOME`, true},
		{`$COMMAND CODEX_HOME`, true},
		{`(( counter[index] ))`, true},
		{`(( arr[i=42] )); npm run dev`, true},
		// A bare `~` head is NOT a fixed path: the shell substitutes its
		// current HOME verbatim, so HOME=unset turns the word into the unset
		// builtin — quoting the remainder away is the same expansion (Codex
		// on #4466).
		{`HOME=unset; ~ CODEX_HOME; codex`, true},
		{`~ CODEX_HOME`, true},
		{`~"" CODEX_HOME`, true},
		// `~+`/`~-` are the same hole one variable over: bare, they expand
		// PWD or OLDPWD verbatim, so PWD=unset makes the head a builtin name
		// (Codex on #4466). A bare `~name` and every directory-stack index
		// (`~0` is PWD too, measured under bash) are no fixed path either.
		{`PWD=unset; ~+ CODEX_HOME; codex`, true},
		{`OLDPWD=unset; ~- CODEX_HOME; codex`, true},
		{`PWD=unset; ~0 CODEX_HOME; codex`, true},
		{`~+ CODEX_HOME`, true},
		{`~- CODEX_HOME`, true},
		{`~+2 CODEX_HOME`, true},
		{`~+0 CODEX_HOME`, true},
		{`~root CODEX_HOME`, true},
		// With a slash they are paths, but only while the command leaves the
		// directory alone: rebinding PWD or OLDPWD next to a tilde word is
		// refused (withTildeBindingNames), and cd only ever sets them to an
		// absolute directory.
		{`PWD=/usr; ~+/bin/env CODEX_HOME=/other codex`, true},
		{`PWD=/tmp; ~+/bin/tool arg`, true},
		{`OLDPWD=/tmp; ~-/bin/tool arg`, true},
		{`~+/bin/tool arg`, false},
		{`~-/bin/tool arg`, false},
		{`cd /srv && ~+/bin/tool arg`, false},
		// A tilde path names its executable by its literal final component,
		// so a modeled wrapper is judged as that wrapper however many `..`
		// segments lead to it (Codex on #4466), and HOME cannot be rebound
		// to aim `~/bin/env` somewhere else.
		{`~/../../usr/bin/env CODEX_HOME=/other codex`, true},
		{`~root/../../usr/bin/env CODEX_HOME=/other codex`, true},
		{`~/bin/env CODEX_HOME=/other codex`, true},
		{`~root/bin/env CODEX_HOME=/other codex`, true},
		{`HOME=/usr; ~/bin/env CODEX_HOME=/other codex`, true},
		{`~/bin/sh -c 'echo x'`, true},
		// The remaining tilde forms stay accepted: the word is a path whose
		// final component is its own literal text.
		{`~/bin/tool arg`, false},
		{`~/.local/bin/tool arg`, false},
		{`~root/bin/tool arg`, false},
		{`~/a/b/c arg`, false},
		{`npm run dev`, false},
		{`ls CODEX_HOME`, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, commandMutatesAccountEnvironment(tc.command, codex),
			"command %q", tc.command)
	}
}

// The refusal names an unprovable word only when that word is why the command
// refused — pinning it would change the verdict. A literal cause (the unset
// builtin, a denied NAME=) keeps the generic guidance instead of sending the
// user to pin an unrelated expansion.
func TestValidateAccountEnvironmentCommand_UnprovableWordBlameIsCausal(t *testing.T) {
	err := ValidateAccountEnvironmentCommand(
		`echo "$HOME"; unset CODEX_HOME`, scopedProcessTabAccount())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "$HOME",
		"literal-cause refusal must not blame an unrelated dynamic word")

	err = ValidateAccountEnvironmentCommand(
		`env "$AF_WRAPPER" codex`, scopedProcessTabAccount())
	require.Error(t, err)
	require.Contains(t, err.Error(), "$AF_WRAPPER",
		"pinning the named word must clear the refusal it is blamed for")
}

// The blame must be computed against the names the VERDICT used. A command
// carrying a ~ word is walked with HOME, PWD and OLDPWD denied too, because the
// walk reads `~/…` as a fixed path only while nothing rebinds that directory.
// Blaming against the un-extended set names a dynamic word whose pinning cannot
// clear the refusal — the tilde-bound assignment still refuses — which is the
// support burden the named-word message exists to prevent (#4466 review).
func TestValidateAccountEnvironmentCommand_BlameUsesTheVerdictNameSet(t *testing.T) {
	err := ValidateAccountEnvironmentCommand(
		`HOME=/usr; ~/bin/env "$AF_WRAPPER" codex`, scopedProcessTabAccount())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "$AF_WRAPPER",
		"pinning the word leaves HOME=/usr refusing, so the message must not send the user after it")
}

// The refusal error names the exact word af could not prove literal so the
// user can pin a literal and self-correct — without it a fail-closed change is
// a support burden, not a fixable message.
func TestValidateAccountEnvironmentCommand_NamesUnprovableWord(t *testing.T) {
	err := ValidateAccountEnvironmentCommand(
		`strace -o "$AF_TRACE_FILE" codex`, scopedProcessTabAccount())
	require.Error(t, err)
	require.Contains(t, err.Error(), `$AF_TRACE_FILE`,
		"refusal must name the word that failed proof")
	require.Contains(t, err.Error(), "replace it with a literal",
		"refusal must tell the user how to fix it")
}

// The evaluation budget is one meter for the whole command, not a fresh one
// per CallExpr — syntax.Walk runs the callback once per call, so a program of
// individually admissible strace invocations (50 calls of a ~900-word argv
// took ~8s measured) spent the full quadratic suffix cost on each while every
// call stayed under straceFlatArgvLimit. Sharing the budget across the walk
// makes the advertised bound cover multi-call programs (Codex on #4466).
func TestCommandMutatesAccountEnvironment_SharedBudgetAcrossCalls(t *testing.T) {
	codex := accountScopedNames("codex", "CODEX_HOME")
	// One call of this size is admissible on its own — that is what makes the
	// pair of assertions a test of SHARING rather than of the cap. Four of
	// them fit only if each opens a fresh meter.
	call := "strace " + strings.Repeat("f ", 300) + "; "
	require.False(t, commandMutatesAccountEnvironment(call, codex),
		"one call of this size must stay individually admissible")
	require.True(t, commandMutatesAccountEnvironment(strings.Repeat(call, 4), codex),
		"a program of individually-admissible calls must exhaust the shared budget")
}

// The advertised bound is on the VALIDATION, not on a call inside it.
// ValidateAccountEnvironmentCommand walks the same program up to three times —
// the verdict, the tilde-cause check, the diagnostic blame — and each walk used
// to open a fresh budget, so the bound covered a third of what the caller pays
// for and nothing bounded the wall clock: measured through this function, one
// 1000-word strace call took 0.54s, eight took 4.8s, and 32 took 18.9s, all
// ACCEPTED (#4466 review). The meter now counts the walk and is shared across
// the three, so the same program refuses in a fraction of a second.
func TestValidateAccountEnvironmentCommand_WholeValidationIsMetered(t *testing.T) {
	command := strings.Repeat("strace "+strings.Repeat("f ", 1000)+"; ", 32)
	start := time.Now()
	err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
	elapsed := time.Since(start)
	require.Error(t, err, "a program past the meter must refuse, not be judged slowly")
	require.Less(t, elapsed, 5*time.Second,
		"one validation must stay inside the metered bound; 18.9s unmetered")
}
