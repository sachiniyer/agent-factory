package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// bash re-evaluates the stdout of a command substitution as FRESH arithmetic
// before using it, including inside an array subscript that the parser reports
// as a plain read. The substitution can therefore print `NAME=value` and have
// bash execute it as an arithmetic assignment to a denied account-identity
// variable while the surrounding expression only appears to read it:
//
//	let 'arr[$(echo CODEX_HOME=1)]'
//
// runs `echo CODEX_HOME=1`, splices `CODEX_HOME=1` back into the expression as
// `arr[CODEX_HOME=1]`, and evaluates the subscript `CODEX_HOME=1` as an
// assignment that rewrites the just-exported selected root in the same shell
// that later execs the agent. Verified against the installed bash before this
// test was written.
//
// The pre-existing guard in account_environment_followup_test.go locks down the
// LITERAL form `let 'arr[CODEX_HOME=42]'` (subscript visible as a BinaryArithm
// assignment node), but the same mechanism hidden behind a `$(...)` command
// substitution sailed past every layer: accountSubscriptInArithmetic only finds
// a literal `name[`, and the AST walk judges the inner `echo` as inert data and
// never models bash's re-evaluation of the substitution output.

// TestProbeLetCmdSubstBypass is the regression test from the bug report: the
// literal form was already refused while the command-substitution form was
// allowed, even though bash reaches the same mutation through both.
func TestProbeLetCmdSubstBypass(t *testing.T) {
	// LITERAL form: already guarded by account_environment_followup_test.go.
	literalErr := ValidateAccountEnvironmentCommand(
		"let 'arr[CODEX_HOME=42]'; codex", scopedProcessTabAccount())
	require.Error(t, literalErr, "literal form must be refused")

	// BYPASS form: same mechanism, hidden behind a command substitution.
	bypassErr := ValidateAccountEnvironmentCommand(
		"let 'arr[$(echo CODEX_HOME=1)]'; codex", scopedProcessTabAccount())
	require.Error(t, bypassErr,
		"cmdsubst form must be refused too, since bash re-evaluates the substitution output as arithmetic")
}

// Every arithmetic entry point that treats a parsed arithmetic AST as
// authoritative must fail closed when the source contains a command
// substitution, because bash re-evaluates the substitution output as fresh
// arithmetic and can therefore assign any denied name through it. This covers
// `let`, `(( ))`, and `$(( ))` in operand, subscript, and command positions,
// across every denied name, both substitution spellings (`$(...)` and
// backticks), under a modeled wrapper (nice), and regardless of which inner
// command the substitution runs.
func TestValidateAccountEnvironmentCommand_RefusesArithmeticCommandSubstitution(t *testing.T) {
	for _, command := range []string{
		// `let` with the mutating assignment hidden in a command substitution.
		// The whole expression can be the substitution, or it can sit inside an
		// array subscript or as a bare operand.
		"let 'arr[$(echo CODEX_HOME=1)]'; codex",
		"let 'arr[$(printf CODEX_HOME=1)]'; codex",
		"let '$(echo CODEX_HOME=1)'; codex",
		"let 'x + $(echo CODEX_HOME=1)'; codex",
		// Backtick command substitution is the same hazard.
		"let 'arr[`echo CODEX_HOME=1`]'; codex",
		// The denial set is not just CODEX_HOME: any identity/shell-startup name
		// is reachable through the substitution.
		"let 'arr[$(echo OPENAI_API_KEY=1)]'; codex",
		"let 'arr[$(echo BASH_ENV=/tmp/payload)]'; codex",
		// An inner command whose own argv look inert (cat reading a file) is
		// still unprovable: its stdout is re-evaluated as arithmetic.
		"let 'arr[$(cat /tmp/x)]'; codex",
		// `$(( ))` arithmetic expansion re-evaluates substitution output too.
		"echo $(( arr[$(echo CODEX_HOME=1)] )); codex",
		"echo $(( $(echo CODEX_HOME=1) )); codex",
		"x=$(( arr[$(echo CODEX_HOME=1)] )); codex",
		"x=$(( $(echo CODEX_HOME=1) )); codex",
		"echo $(( arr[`echo CODEX_HOME=1`] )); codex",
		// `(( ))` arithmetic command — already refused by the POSIX parse, but
		// the bash entry point must fail closed independently so the verdict never
		// depends on a parse-variant coincidence.
		"(( arr[$(echo CODEX_HOME=1)] )); codex",
		"(( $(echo CODEX_HOME=1) )); codex",
		// A modeled wrapper (nice) that schedules `let` reaches the same guard.
		"nice -n 5 let 'arr[$(echo CODEX_HOME=1)]'; codex",
		// Parameter expansion with an arithmetic subscript: bash re-evaluates
		// the subscript as arithmetic, so `${arr[$(printf CODEX_HOME=1)]}` has
		// the same re-evaluation hazard as a `let` subscript. ParamExp.Index is
		// an implicit arithmetic context.
		": \"${arr[$(printf CODEX_HOME=1)]}\"; codex",
		": \"${arr[$(echo CODEX_HOME=1)]}\"; codex",
		": ${arr[`echo CODEX_HOME=1`]}; codex",
		// Indexed assignment also uses arithmetic for the subscript.
		"arr[$(echo CODEX_HOME=1)]=x; codex",
		// LITERAL forms the followup suite already guards must keep being refused.
		"let 'arr[CODEX_HOME=42]'; codex",
		"(( arr[CODEX_HOME=42] )); codex",
		"echo $(( arr[CODEX_HOME=42] )); codex",
		"let 'CODEX_HOME=1'; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q hides a denied-name assignment behind arithmetic command substitution", command)
	}
}

// The refusal above must stay narrow. A process tab is an arbitrary user
// command, so ordinary arithmetic — including `$(( ))` that contains NO
// command substitution — and command substitutions that appear OUTSIDE an
// arithmetic context (where their output is plain data, not re-evaluated
// arithmetic) must keep working.
func TestValidateAccountEnvironmentCommand_AllowsProvableArithmeticAndExternalCmdSubst(t *testing.T) {
	for _, command := range []string{
		// Command substitutions that are NOT inside an arithmetic context: their
		// stdout is data, so `echo $(echo CODEX_HOME=1)` merely prints the
		// string and mutates nothing.
		"echo $(echo CODEX_HOME=1); codex",
		"echo \"$(echo CODEX_HOME=1)\"; codex",
		"codex $(echo hi)",
		// Pure arithmetic with no command substitution is provable and stays
		// allowed, including `$(( ))` whose re-evaluation cannot assign anything.
		"echo $(( 1 + 2 )); codex",
		"echo $(( arr[i] )); codex",
		"(( x = 1 )); codex",
		"let 'x = 1'; codex",
		"let 'x = $((1+1))'; codex",
		// Parameter expansions with literal subscripts are safe: no
		// command substitution to re-evaluate as arithmetic.
		"echo ${arr[0]}; codex",
		"echo ${arr[i]}; codex",
		// The existing narrow arithmetic forms remain allowed.
		"let 'total += 1'",
		"let 'arr[i=42]'; npm run dev",
		"(( counter[index] ))",
		"(( arr[i=42] )); npm run dev",
		// Non-arithmetic commands are unaffected.
		"cd /tmp && codex",
		"npm run dev",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q is provably free of identity mutation and must stay allowed", command)
	}
}
