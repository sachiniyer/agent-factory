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

// TestValidateAccountEnvironmentCommand_RefusesBinaryTestNumericCmdSubst verifies
// that `[[ ]]` numeric comparison operators (-eq, -ne, -lt, -gt, -le, -ge)
// fail closed when either operand contains a command substitution.
//
// On a bash-as-/bin/sh host, bash evaluates the operands of numeric [[ ]]
// operators as arithmetic. A command substitution in either operand is
// re-evaluated as fresh arithmetic by bash, so `[[ 0 -eq $(printf CODEX_HOME=1) ]]`
// assigns CODEX_HOME=1 in the current shell before the agent is launched.
func TestValidateAccountEnvironmentCommand_RefusesBinaryTestNumericCmdSubst(t *testing.T) {
	for _, command := range []string{
		// All six numeric operators with a command substitution in the right operand.
		"[[ 0 -eq $(printf CODEX_HOME=1) ]]; codex",
		"[[ 0 -ne $(printf CODEX_HOME=1) ]]; codex",
		"[[ 0 -lt $(printf CODEX_HOME=1) ]]; codex",
		"[[ 0 -gt $(printf CODEX_HOME=1) ]]; codex",
		"[[ 0 -le $(printf CODEX_HOME=1) ]]; codex",
		"[[ 0 -ge $(printf CODEX_HOME=1) ]]; codex",
		// Command substitution in the left operand.
		"[[ $(printf CODEX_HOME=1) -eq 0 ]]; codex",
		// Backtick form of command substitution.
		"[[ 0 -eq `printf CODEX_HOME=1` ]]; codex",
		// Other denied names are reachable too.
		"[[ 0 -eq $(printf OPENAI_API_KEY=x) ]]; codex",
		"[[ 0 -eq $(printf BASH_ENV=/tmp/p) ]]; codex",
		// An inner command whose argv look inert is still unprovable.
		"[[ 0 -eq $(cat /tmp/x) ]]; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q hides a denied-name assignment behind a [[ ]] numeric operator", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesIndirectArithmeticTaint verifies
// that a variable assigned from a command substitution and later used in an
// arithmetic context is refused. bash re-evaluates the variable's value as
// fresh arithmetic at the point of use, so the substitution's stdout becomes
// a deferred arithmetic mutation — the same bypass as an inline substitution,
// just split across two statements.
//
//	x=$(printf CODEX_HOME=1)   # x is now "CODEX_HOME=1"
//	: $((x))                   # bash re-evaluates x as arithmetic → CODEX_HOME=1
//	codex                      # runs with the replaced root
func TestValidateAccountEnvironmentCommand_RefusesIndirectArithmeticTaint(t *testing.T) {
	for _, command := range []string{
		// Variable assigned from cmd substitution, then used in $(( )).
		"x=$(printf CODEX_HOME=1); : $((x)); codex",
		"x=$(printf CODEX_HOME=1); : $((x+0)); codex",
		// Used in (( )) arithmetic command.
		"x=$(printf CODEX_HOME=1); (( x )); codex",
		// Used in let.
		"x=$(printf CODEX_HOME=1); let x; codex",
		// Used in a [[ ]] numeric comparison operand.
		"x=$(printf CODEX_HOME=1); [[ 0 -eq $x ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ $x -eq 0 ]]; codex",
		// Backtick assignment also taints.
		"x=`printf CODEX_HOME=1`; : $((x)); codex",
		// Other denied names are reachable the same way.
		"x=$(printf OPENAI_API_KEY=1); (( x )); codex",
		// Array subscript using a tainted variable.
		": ${arr[$(printf CODEX_HOME=1)]}; codex",
		"x=$(printf CODEX_HOME=1); : ${arr[x]}; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q routes a command-substitution through a variable into arithmetic and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesTaintPropagation verifies that
// taint propagates transitively through parameter-expansion copies. When `y=$x`
// and `x` is tainted by a command substitution, bash stores x's value in y, so
// `: $((y))` re-evaluates y as arithmetic — the same bypass as `: $((x))`.
func TestValidateAccountEnvironmentCommand_RefusesTaintPropagation(t *testing.T) {
	for _, command := range []string{
		// One-hop copy: y=$x; y is tainted because x is.
		"x=$(printf CODEX_HOME=1); y=$x; : $((y)); codex",
		// Two-hop chain: z=$y; z is also tainted.
		"x=$(printf CODEX_HOME=1); y=$x; z=$y; (( z )); codex",
		// Copy used in a [[ ]] numeric comparison.
		"x=$(printf CODEX_HOME=1); y=$x; [[ y -eq 0 ]]; codex",
		// Copy used in let.
		"x=$(printf CODEX_HOME=1); y=$x; let y; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q propagates a tainted variable through a copy and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesBareArithmeticVarInTest verifies
// that a bare (un-prefixed) variable name used as a numeric [[ ]] operand is
// caught when that variable is tainted. In bash arithmetic, `x` and `$x` are
// equivalent; the guard must refuse both spellings.
func TestValidateAccountEnvironmentCommand_RefusesBareArithmeticVarInTest(t *testing.T) {
	for _, command := range []string{
		// Bare variable name without $ in numeric [[ ]] operand.
		"x=$(printf CODEX_HOME=1); [[ x -eq 0 ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ 0 -eq x ]]; codex",
		// Other numeric operators.
		"x=$(printf CODEX_HOME=1); [[ x -ne 0 ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ x -lt 0 ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ x -gt 0 ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ x -le 0 ]]; codex",
		"x=$(printf CODEX_HOME=1); [[ x -ge 0 ]]; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q uses a bare tainted variable in a numeric [[ ]] and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesTaintedVarInSubscript verifies
// that a tainted variable used as an array subscript is caught. In
// `$((arr[x]))`, bash evaluates the subscript `x` as arithmetic, so if x is
// tainted, its contents are re-evaluated — the same hazard as `: $((x))`.
func TestValidateAccountEnvironmentCommand_RefusesTaintedVarInSubscript(t *testing.T) {
	for _, command := range []string{
		// Tainted variable as subscript in $(( )) arithmetic expansion.
		"x=$(printf CODEX_HOME=1); : $((arr[x])); codex",
		// Tainted variable as subscript in (( )) arithmetic command.
		"x=$(printf CODEX_HOME=1); (( arr[x] )); codex",
		// Tainted variable as subscript in let.
		"x=$(printf CODEX_HOME=1); let 'arr[x]'; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q uses a tainted variable as an array subscript and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesExpandedTaintedVarInArithm verifies
// that `$x` (a ParamExp) inside arithmetic is caught when x is tainted. The
// `$(( $x ))` form expands x and re-evaluates the result as arithmetic.
func TestValidateAccountEnvironmentCommand_RefusesExpandedTaintedVarInArithm(t *testing.T) {
	for _, command := range []string{
		// $x (ParamExp) inside $(( )).
		"x=$(printf CODEX_HOME=1); : $(( $x )); codex",
		// $x inside (( )).
		"x=$(printf CODEX_HOME=1); (( $x )); codex",
		// $x as subscript.
		"x=$(printf CODEX_HOME=1); : $(( arr[$x] )); codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q uses $tainted in arithmetic and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesParamExpAssignmentTaint verifies
// that a parameter expansion with an assignment operator (${x:=...} or ${x=...})
// taints the target variable when the default value is a command substitution.
// bash evaluates the default value and assigns it to x when x is unset, after
// which `: $((x))` re-evaluates x's contents as arithmetic — the same bypass as
// an inline substitution.
//
//	x=; : "${x:=$(printf CODEX_HOME=1)}"; : $((x)); codex
//
// The ParamExp assigns the substitution output to x; the later arithmetic
// re-evaluates it and changes CODEX_HOME.
func TestValidateAccountEnvironmentCommand_RefusesParamExpAssignmentTaint(t *testing.T) {
	for _, command := range []string{
		// := operator (assign-if-unset-or-null) with command substitution.
		`x=; : "${x:=$(printf CODEX_HOME=1)}"; : $((x)); codex`,
		// = operator (assign-if-unset) with command substitution.
		`x=; : "${x=$(printf CODEX_HOME=1)}"; : $((x)); codex`,
		// Tainted variable used in (( )) arithmetic command.
		`x=; : "${x:=$(printf CODEX_HOME=1)}"; (( x )); codex`,
		// Tainted variable used in let.
		`x=; : "${x:=$(printf CODEX_HOME=1)}"; let x; codex`,
		// Tainted variable used in numeric [[ ]] operand.
		`x=; : "${x:=$(printf CODEX_HOME=1)}"; [[ 0 -eq $x ]]; codex`,
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q taints a variable through a ParamExp assignment and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesCStyleLoopArithmetic verifies
// that C-style for loops (`for (( init; cond; post ))`) fail closed when any
// clause contains a command substitution or references a tainted variable.
// All three clauses are arithmetic contexts: bash re-evaluates the substitution
// output or variable value as arithmetic in each one.
func TestValidateAccountEnvironmentCommand_RefusesCStyleLoopArithmetic(t *testing.T) {
	for _, command := range []string{
		// Command substitution in the init clause.
		"for (( i = $(printf CODEX_HOME=1), n=0; n < 1; n++ )); do :; done; codex",
		// Command substitution in the condition clause.
		"for (( i = 0; i < $(printf CODEX_HOME=1); i++ )); do :; done; codex",
		// Command substitution in the post clause.
		"for (( i = 0; i < 1; i += $(printf CODEX_HOME=1) )); do :; done; codex",
		// Tainted variable in the init clause.
		"x=$(printf CODEX_HOME=1); for (( i = x, n=0; n < 1; n++ )); do :; done; codex",
		// Tainted variable in the condition clause.
		"x=$(printf CODEX_HOME=1); for (( i = 0; i < x; i++ )); do :; done; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q uses a command substitution or tainted variable in a C-style for loop and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesDeclarationBuiltinTaint verifies
// that a variable assigned from a command substitution via a declaration builtin
// (declare, typeset, local, export) is refused when later used in an arithmetic
// context. In bash, `declare x=$(printf CODEX_HOME=1)` is equivalent to
// `x=$(printf CODEX_HOME=1)` for taint purposes: bash re-evaluates x's value
// as fresh arithmetic when x appears in `$((x))`, `(( ))`, or `let`.
//
// The bash parser represents DeclClause arguments as `*syntax.Assign` nodes,
// so the taint accumulator's Assign arm already catches them without any
// special-case handling. This test locks down that behavior.
func TestValidateAccountEnvironmentCommand_RefusesDeclarationBuiltinTaint(t *testing.T) {
	for _, command := range []string{
		// declare builtin: x gets the substitution output; : $((x)) re-evaluates it.
		"declare x=$(printf CODEX_HOME=1); : $((x)); codex",
		// typeset is an alias for declare in bash.
		"typeset x=$(printf CODEX_HOME=1); : $((x)); codex",
		// export with a command substitution also taints the variable.
		"export x=$(printf CODEX_HOME=1); : $((x)); codex",
		// Taint propagates through a copy: declare → variable copy → arithmetic.
		"declare x=$(printf CODEX_HOME=1); y=$x; : $((y)); codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q routes a declaration-builtin substitution through a variable into arithmetic and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RespectsStatementOrderForTaint verifies
// that a command-substitution assignment that appears AFTER an arithmetic
// expression does not cause that earlier expression to be refused. Only
// assignments that precede an arithmetic use contribute taint to it.
//
// Example: `x=0; : $((x)); x=$(printf CODEX_HOME=1); codex`
//   - At the point of `$((x))`, x holds the literal 0; the substitution that
//     would taint x has not yet run, so the arithmetic is safe.
//   - The guard must not refuse this command because of a later assignment.
func TestValidateAccountEnvironmentCommand_RespectsStatementOrderForTaint(t *testing.T) {
	for _, command := range []string{
		// Arithmetic uses x before x is assigned from a command substitution.
		"x=0; : $((x)); x=$(printf CODEX_HOME=1); codex",
		// (( )) form.
		"x=0; (( x )); x=$(printf CODEX_HOME=1); codex",
		// let form.
		"x=0; let x; x=$(printf CODEX_HOME=1); codex",
		// Taint-propagation chain where the copy and arithmetic both precede
		// the tainted assignment: y=$x is not tainted at that point.
		"x=0; y=$x; : $((y)); x=$(printf CODEX_HOME=1); codex",
		// Numeric [[ ]] operand before tainted assignment.
		"x=0; [[ x -eq 0 ]]; x=$(printf CODEX_HOME=1); codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q uses arithmetic before the tainted assignment and must stay allowed", command)
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
		// The existing narrow arithmetic forms remain allowed.
		"let 'total += 1'",
		"let 'arr[i=42]'; npm run dev",
		"(( counter[index] ))",
		"(( arr[i=42] )); npm run dev",
		// Non-arithmetic commands are unaffected.
		"cd /tmp && codex",
		"npm run dev",
		// [[ ]] string-comparison operators do NOT evaluate operands as
		// arithmetic, so a command substitution in them is plain data.
		"[[ $(echo CODEX_HOME=1) == \"CODEX_HOME=1\" ]]; codex",
		"[[ $(echo hi) != \"bye\" ]]; codex",
		// [[ ]] numeric operators with no command substitution are fine.
		"[[ 0 -eq 0 ]]; codex",
		"[[ 1 -lt 2 ]]; codex",
		// A variable assigned from a command substitution but used OUTSIDE
		// arithmetic is still just data — the stdout is a string, not re-evaluated.
		"x=$(echo CODEX_HOME=1); echo $x; codex",
		"x=$(echo hi); echo $x; codex",
		// A variable assigned from a literal value (not a command substitution)
		// and used in arithmetic is provable.
		"x=42; : $((x)); codex",
		"x=1; (( x )); codex",
		// Non-tainted variables in subscripts and bare [[ ]] forms are fine.
		"x=42; : $((arr[x])); codex",
		"x=42; [[ x -eq 0 ]]; codex",
		// A variable copy from a non-tainted source is not tainted.
		"x=42; y=$x; : $((y)); codex",
		// A ParamExp assignment from a literal (not a command substitution)
		// does not taint the variable.
		`x=; : "${x:=42}"; : $((x)); codex`,
		// $[ ] (deprecated arithmetic) is handled as ArithmExp — pure
		// arithmetic with no substitution is provable.
		"echo $[1+2]; codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q is provably free of identity mutation and must stay allowed", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesCompoundStatementTaint verifies
// that taint propagates through sequential children inside compound statements
// such as blocks (`{ }`) and AND/OR lists (`&&`, `||`). When an assignment from
// a command substitution appears before an arithmetic expression inside the same
// compound statement, the later expression must see the taint.
//
//	{ x=$(printf CODEX_HOME=1); : $((x)); }; codex
//
// Both children of the block execute in the current shell, so x is tainted at
// the point where $((x)) is evaluated.
func TestValidateAccountEnvironmentCommand_RefusesCompoundStatementTaint(t *testing.T) {
	for _, command := range []string{
		// Block: assignment before arithmetic, both inside { }.
		"{ x=$(printf CODEX_HOME=1); : $((x)); }; codex",
		// AND list: assignment on the left, arithmetic on the right.
		"x=$(printf CODEX_HOME=1) && : $((x)); codex",
		// OR list: both sides may execute; taint from X is visible to Y.
		"x=$(printf CODEX_HOME=1) || : $((x)); codex",
		// Nested block.
		"{ { x=$(printf CODEX_HOME=1); }; : $((x)); }; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q has a tainted variable inside a compound statement and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesStatementOrderForTaint_Compound
// verifies that the allowed-before-tainted rule from
// TestValidateAccountEnvironmentCommand_RespectsStatementOrderForTaint also
// applies inside compound statements: arithmetic that precedes the tainted
// assignment stays allowed.
func TestValidateAccountEnvironmentCommand_AllowsCompoundSafeTaintOrder(t *testing.T) {
	for _, command := range []string{
		// Arithmetic precedes the tainted assignment inside the same block.
		"{ : $((x)); x=$(printf CODEX_HOME=1); }; codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q uses arithmetic before the tainted assignment inside a block and must stay allowed", command)
	}
}

// TestValidateAccountEnvironmentCommand_RefusesLiteralArithAssignment verifies
// that a variable assigned a literal string that is itself an arithmetic
// assignment expression to a denied name is refused when later used in
// arithmetic. bash evaluates the variable's contents as fresh arithmetic in
// `$(( ))`, `(( ))`, and `let`, so `x='CODEX_HOME=1'; : $((x))` changes
// CODEX_HOME even though x was assigned from a literal, not a command
// substitution.
func TestValidateAccountEnvironmentCommand_RefusesLiteralArithAssignment(t *testing.T) {
	for _, command := range []string{
		// Literal value is DENIED_NAME=value; arithmetic re-evaluates it.
		"x='CODEX_HOME=1'; : $((x)); codex",
		"x='OPENAI_API_KEY=secret'; : $((x)); codex",
		// Other arithmetic entry points.
		"x='CODEX_HOME=1'; (( x )); codex",
		"x='CODEX_HOME=1'; let x; codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q stores a denied-name assignment in a variable and uses it in arithmetic and must be refused", command)
	}
}

// TestValidateAccountEnvironmentCommand_AllowsLiteralArithNonDenied verifies
// that the literal-arithmetic-assignment check is narrow: a variable whose
// literal value assigns a non-denied name, or is a plain number, must stay
// allowed.
func TestValidateAccountEnvironmentCommand_AllowsLiteralArithNonDenied(t *testing.T) {
	for _, command := range []string{
		// Literal is a plain number — does not assign any name.
		"x='42'; : $((x)); codex",
		"x='0'; (( x )); codex",
		// Literal assigns a non-denied name.
		"x='a=1'; : $((x)); codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q stores a non-hazardous literal and must stay allowed", command)
	}
}

// TestValidateAccountEnvironmentCommand_AllowsDefiniteReassignment verifies
// that a definite unconditional reassignment to a provably clean value removes
// a variable from the taint set. `x=$(cmd); x=0; : $((x))` is safe because the
// literal `x=0` overwrites the tainted value before the arithmetic expression
// is evaluated.
func TestValidateAccountEnvironmentCommand_AllowsDefiniteReassignment(t *testing.T) {
	for _, command := range []string{
		// Direct reassignment to a literal clears taint.
		"x=$(printf CODEX_HOME=1); x=0; : $((x)); codex",
		"x=$(printf CODEX_HOME=1); x=42; (( x )); codex",
		"x=$(printf CODEX_HOME=1); x=42; let x; codex",
		// Reassignment to a non-tainted copy also clears.
		"x=$(printf CODEX_HOME=1); y=0; x=$y; : $((x)); codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount()),
			"command %q overwrites the tainted variable with a clean value and must stay allowed", command)
	}
}
