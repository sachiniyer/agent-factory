package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// bash's `NAME+=value` is an append-assignment (set-from-empty when NAME has no
// prior value) with the same mutating effect on a protected environment name
// as `NAME=value` once the receiver is a declaration builtin
// (export/declare/typeset/readonly/local). The validator refused the BARE
// `export CODEX_HOME+=/other; codex` form via the parser's `*syntax.Assign`
// (Append) node, but a `+=` argument word sitting BEHIND a wrapper (command,
// builtin, nice, exec, nohup, ...) is re-judged as a plain word list. The shared
// operand predicate cut it at the first `=` and produced `NAME+` — a name that
// does not match the denied set — so an explicitly refused mutation silently
// slipped through. The fix strips a trailing `+` after the cut in
// `accountEnvironmentOperandDenied`; `shellWordAssignmentName` makes the same
// correction so the assignment branch of `declarationMutatesAccountEnvironment`
// stays accurate.
//
// These tests pin the wrapped `+=` shapes the report enumerated: every
// declaration-builtin head behind every peeled wrapper, the quoted-argument
// form, the lone form with no agent follow-up, the credential and shell-startup
// name variants, and the strace `-E`/`--env` option-value spellings that route
// through the same predicate. Bare-form controls confirm the parser-level
// refusal still holds; narrowness controls confirm an allowed-name `+=` append
// and the `env` wrapper (which does not honor `+=`) stay admitted.

// scopedProcessTabAppendNames is the denied-name set the production walk uses
// for a codex sibling process tab: the agent's credential names plus the config
// dir, with the shell-startup names overlaid just as
// TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment prepares it.
func scopedProcessTabAppendNames() map[string]struct{} {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	return codex
}

// TestValidateAccountEnvironmentCommand_RefusesWrappedAppendAssignment pins the
// exact command shapes the report's evidence table listed as bypassed. Each was
// ADMITTED before the fix and is now REFUSED — every declaration-builtin head
// behind every peeled wrapper, plus the quoted-argument form and the lone form
// with no agent follow-up. The refusal message is the standard account-
// environment guard message so a reader greps the same surface as the existing
// `=` form refusals.
func TestValidateAccountEnvironmentCommand_RefusesWrappedAppendAssignment(t *testing.T) {
	for _, command := range []string{
		// Baselines that already refused before the fix (control): the bare
		// form is caught by the parser's `*syntax.Assign` (Append) node, and the
		// `=` form behind `command` was already refused. Pinning them keeps a
		// future refactor from regressing the parser-level path while fixing the
		// wrapper word-list path.
		"export CODEX_HOME=/other; codex",
		"export CODEX_HOME+=/other; codex",
		"command export CODEX_HOME=/other; codex",
		// The bypass shapes the report enumerated — all now refused.
		"command export CODEX_HOME+=/other; codex",
		"builtin export CODEX_HOME+=/other; codex",
		"command readonly CODEX_HOME+=/other; codex",
		"command declare CODEX_HOME+=/other; codex",
		"command typeset CODEX_HOME+=/other; codex",
		"command local CODEX_HOME+=/other; codex",
		// The quoted-argument form reduces to the same `+=` operand after
		// quote removal, so the predicate sees the same `NAME+=value` word.
		"command export 'CODEX_HOME+=/other'; codex",
		`command export "CODEX_HOME+=/other"; codex`,
		// The lone form with no agent follow-up still mutates the sibling
		// environment before any later statement runs, so it refuses too.
		"command export CODEX_HOME+=/other",
		// Every modelled wrapper that unwraps to the declaration builtin.
		"nice -n 5 export CODEX_HOME+=/other; codex",
		"nice export CODEX_HOME+=/other; codex",
		"exec export CODEX_HOME+=/other; codex",
		"nohup export CODEX_HOME+=/other; codex",
		// `command`/`builtin` peel even when stacked under another wrapper.
		"nohup command export CODEX_HOME+=/other; codex",
		"exec builtin export CODEX_HOME+=/other; codex",
		// A credential name (agent auth selector) is hidden the same way; append
		// from empty sets it to an attacker-chosen value.
		"command export OPENAI_API_KEY+=sk; codex",
		"command export CODEX_API_KEY+=sk; codex",
		"command export CODEX_ACCESS_TOKEN+=tok; codex",
		// A shell-startup name runs before the agent reaches its exec shim.
		"command export BASH_ENV+=/tmp/evil; codex",
		"command export ZDOTDIR+=/tmp/evil; codex",
		// strace's -E/--env passthrough refuses the `=` form already; the `+=`
		// form routes through the same operand predicate and refuses for the
		// same reason, with the same refusal message — fail-closed for a
		// spelling the static walk cannot prove safe.
		"strace -E CODEX_HOME+=/other codex",
		"strace --env=CODEX_HOME+=/other codex",
		"strace -ECODEX_HOME+=/other codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q must refuse the append-assignment mutation", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q: refusal must come from the account-environment guard", command)
	}
}

// TestCommandMutatesAccountEnvironment_AppendAssignment pins the boolean
// predicate result directly so the signal — not only the end-to-end error — is
// locked against future drift. The table mirrors the e2e test plus narrowness
// cases that must stay ADMITTED: an allowed (non-identity) name's `+=` append
// is no threat, a name that merely shares a prefix with a denied name does not
// match, and the `env` wrapper does not honor `+=` (it sets the literal
// variable named `NAME+`, not `NAME`), so the append form on env is correctly
// admitted. The plain-operand controls (`unset`/`printf -v`/`read`/`getopts`)
// confirm those paths still refuse a real `=` mutation to a denied name.
func TestCommandMutatesAccountEnvironment_AppendAssignment(t *testing.T) {
	names := scopedProcessTabAppendNames()
	cases := []struct {
		command string
		want    bool
	}{
		// Baselines: `=` form refused under wrapper; bare `+=` refused via the
		// parser's `*syntax.Assign` (Append) node.
		{"export CODEX_HOME=/other; codex", true},
		{"command export CODEX_HOME=/other; codex", true},
		{"export CODEX_HOME+=/other; codex", true},
		// Bypass shapes the report enumerated, all now refused.
		{"command export CODEX_HOME+=/other; codex", true},
		{"builtin export CODEX_HOME+=/other; codex", true},
		{"command readonly CODEX_HOME+=/other; codex", true},
		{"command declare CODEX_HOME+=/other; codex", true},
		{"command typeset CODEX_HOME+=/other; codex", true},
		{"command local CODEX_HOME+=/other; codex", true},
		{`command export 'CODEX_HOME+=/other'; codex`, true},
		{`command export "CODEX_HOME+=/other"; codex`, true},
		{"command export CODEX_HOME+=/other", true},
		{"nice -n 5 export CODEX_HOME+=/other; codex", true},
		{"nice export CODEX_HOME+=/other; codex", true},
		{"exec export CODEX_HOME+=/other; codex", true},
		{"nohup export CODEX_HOME+=/other; codex", true},
		{"nohup command export CODEX_HOME+=/other; codex", true},
		{"exec builtin export CODEX_HOME+=/other; codex", true},
		// Credential names and shell-startup names behind wrappers.
		{"command export OPENAI_API_KEY+=sk; codex", true},
		{"command export CODEX_API_KEY+=sk; codex", true},
		{"command export CODEX_ACCESS_TOKEN+=tok; codex", true},
		{"command export BASH_ENV+=/tmp/evil; codex", true},
		{"command export ZDOTDIR+=/tmp/evil; codex", true},
		// strace -E/--env `+=` form: fail-closed for the spelling, matching the
		// existing `=` form refusal that routes through the same predicate.
		{"strace -E CODEX_HOME+=/other codex", true},
		{"strace --env=CODEX_HOME+=/other codex", true},
		{"strace -ECODEX_HOME+=/other codex", true},
		// Narrowness: an allowed name's `+=` append sets no protected variable.
		{"command export PORT+=3000; codex", false},
		{"command export FOO+=1; codex", false},
		{"command export ALLOWED_NAME+=x; codex", false},
		// A name that merely shares a prefix with a denied name does not match.
		{"command export CODEX_HOME_OTHER+=x; codex", false},
		{"command export CODEX_HOME2+=x; codex", false},
		// The `env` wrapper does NOT honor `+=`: the operand `CODEX_HOME+=/other`
		// is split by env at the first `=` into the literal variable `CODEX_HOME+`
		// and value `/other`, so `CODEX_HOME` is untouched and the command must
		// stay admitted. The `=` form on env IS a real mutation and stays
		// refused — the contrast demonstrates the fix closes exactly the shell
		// operator bash honors on declaration builtins.
		{"env CODEX_HOME+=/other codex", false},
		{"env CODEX_HOME=/other codex", true},
		// A bare NAME without an `=` is an argument, not an assignment token, nor
		// is it a `+=` append — this lives outside the bug's surface, and is
		// here as a control: the validator's existing `export NAME` stance (the
		// operand predicate refuses a denied name even without an `=`) is
		// unchanged by the `+=` fix.
		{"command export CODEX_HOME", true},
		{"echo CODEX_HOME+=/other", false},
		// Plain operands (no `=`) routing through the same predicate stay
		// refused for the real mutation they perform on the denied name.
		{"command unset CODEX_HOME", true},
		{"command printf -v CODEX_HOME hello; codex", true},
		{"command read CODEX_HOME; codex", true},
		{"command getopts x CODEX_HOME; codex", true},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, names)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// TestApplyAccountEnvironment_RefusesWrappedAppendAssignment confirms the
// production exec-shim gate refuses the bypass before any scoped environment
// is produced, mirroring the existing
// TestApplyAccountEnvironment_RefusesUnmodeledWrapperHiddenAssignment that
// exercises the same entry point for the wrapper `=` shape.
func TestApplyAccountEnvironment_RefusesWrappedAppendAssignment(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"command export CODEX_HOME+=/other; codex",
		"builtin export CODEX_HOME+=/other; codex",
		"command export OPENAI_API_KEY+=sk; codex",
		"command export BASH_ENV+=/tmp/evil; codex",
		"exec export CODEX_HOME+=/other; codex",
		"nice -n 5 export CODEX_HOME+=/other; codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
}

// TestShellWordAssignmentName_RecognizesAppendForm pins the helper directly.
// `shellWordAssignmentName` feeds the assignment branch of
// `declarationMutatesAccountEnvironment` and the post-`exec` assignment scan in
// `callOverridesName`. Before the fix, `NAME+=value` cut to `NAME+` and failed
// `validName`, so the word was never classified as an assignment and the
// declaration-builtin walk fell through to the literal-word branch. After the
// fix, the append form returns the clean name and `assignment=true`.
func TestShellWordAssignmentName_RecognizesAppendForm(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    string
		argIndex   int
		wantName   string
		wantAssign bool
	}{
		// Plain set form: a literal `NAME=value`, name is the part before `=`.
		{"set", "command export CODEX_HOME=/other", 2, "CODEX_HOME", true},
		// Append form: literal `NAME+=value`, recognized as an assignment to NAME.
		{"append", "command export CODEX_HOME+=/other", 2, "CODEX_HOME", true},
		// Allowed-name append form: also recognized as an assignment (to PORT).
		{"allowed-name-append", "command export PORT+=3000", 2, "PORT", true},
		// Bare NAME without an `=` is an argument, not an assignment token.
		{"bare-name", "command export CODEX_HOME", 2, "CODEX_HOME", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, ok := singleCallIgnoringRedirections(tc.command)
			require.True(t, ok, "command %q must parse as a single call", tc.command)
			words, _ := stripExecPrefix(call.Args)
			require.Less(t, tc.argIndex, len(words),
				"command %q: arg-index %d out of range", tc.command, tc.argIndex)
			name, assignment := shellWordAssignmentName(words[tc.argIndex])
			require.Equal(t, tc.wantName, name, "command %q: assignment name", tc.command)
			require.Equal(t, tc.wantAssign, assignment, "command %q: assignment flag", tc.command)
		})
	}
}
