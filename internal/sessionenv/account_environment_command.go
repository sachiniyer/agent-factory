package sessionenv

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// commandMutatesAccountEnvironment recognizes command-local mutations that can
// replace a sibling tab's selected account. Unlike commandOverridesName, this
// path launches arbitrary user processes, so unrelated configuration such as
// PORT=3000 remains allowed. Shell decorations do not hide calls from the walk,
// and an unsupported form of a recognized environment mutator fails closed.
func commandMutatesAccountEnvironment(command string, names map[string]struct{}) bool {
	if command == "" {
		return false
	}
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			return true
		}
		// Inverted arithmetic guard: refuse any command whose arithmetic context
		// has an operand that is not provably a numeric constant. Rather than
		// enumerating the ways a variable's stored value can become hazardous
		// (command substitution, hazardous literal, runtime-input builtin,
		// dynamically composed strings, …), the guard inverts the burden: an
		// arithmetic context is refused UNLESS its operand provably consists of
		// only integer literals, arithmetic operators, and parentheses. Any
		// variable reference inside arithmetic — $((x)), (( x )), let x,
		// arr[x], etc. — is unprovable regardless of how x was assigned.
		//
		// This single check subsumes the three coarse rules that preceded it:
		//   - CmdSubst+arith: a CmdSubst inside arithmetic is never a constant.
		//   - Literal-assignment+arith: a variable holding a hazardous literal
		//     appears in arithmetic as a variable reference — not a constant.
		//   - Runtime-input+arith: same as the literal case above.
		//   - Dynamic-composition bypass (n=CODEX_HOME; x="${n}=1"; : $((x))):
		//     x is a variable reference in arithmetic — not a constant.
		//
		// Safe-side false positives: commands that combine arithmetic with any
		// non-constant expression are refused, including provably safe ones like
		// n=5; : $((n+1)). That cost is documented and priced as acceptable:
		// arithmetic over non-constant operands is uncommon in agent invocation
		// strings, and the precision gain from tracking whether the variable was
		// actually hazardous is outweighed by the unbounded enumeration gap it
		// creates.
		if fileHasArithmeticContextWithVariableOperand(file) {
			return true
		}
		mutates := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if nodeMutatesAccountEnvironment(node, names, nil) {
				mutates = true
				return false
			}
			return true
		})
		if mutates {
			return true
		}
	}
	return false
}

func nodeMutatesAccountEnvironment(node syntax.Node, names map[string]struct{}, tainted map[string]struct{}) bool {
	switch node := node.(type) {
	case *syntax.CallExpr:
		return callMutatesAccountEnvironment(node, names, tainted)
	case *syntax.Assign:
		// An indexed assignment (`arr[i]=val`) evaluates the subscript as
		// arithmetic; a command substitution in the index is re-evaluated as
		// fresh arithmetic by bash and can assign a denied name via its output
		// even when `arr` itself is not denied.
		if node.Index != nil && arithmeticExprHasCommandSubstitution(node.Index) {
			return true
		}
		if node.Index != nil && arithmeticExprReferencesTaintedVar(node.Index, tainted) {
			return true
		}
		return node.Name != nil && accountEnvironmentNameDenied(node.Name.Value, names)
	case *syntax.WordIter:
		return node.Name != nil && accountEnvironmentNameDenied(node.Name.Value, names)
	case *syntax.ParamExp:
		// An indexed subscript (`${arr[i]}`) is evaluated as arithmetic by
		// bash, so a command substitution inside the index — e.g.
		// `${arr[$(printf CODEX_HOME=1)]}` — is re-evaluated as fresh
		// arithmetic and can assign a denied name even when `arr` itself is
		// not denied. Fail closed when the index contains a substitution.
		if node.Index != nil && arithmeticExprHasCommandSubstitution(node.Index) {
			return true
		}
		if node.Index != nil && arithmeticExprReferencesTaintedVar(node.Index, tainted) {
			return true
		}
		// Slice expressions (`${x:offset:length}`) also evaluate their
		// operands as arithmetic; a command substitution in either position
		// is re-evaluated as fresh arithmetic by bash and can assign a denied
		// name (e.g. `${x:$(printf CODEX_HOME=1)}`). Fail closed on either.
		if node.Slice != nil {
			if node.Slice.Offset != nil && arithmeticExprHasCommandSubstitution(node.Slice.Offset) {
				return true
			}
			if node.Slice.Offset != nil && arithmeticExprReferencesTaintedVar(node.Slice.Offset, tainted) {
				return true
			}
			if node.Slice.Length != nil && arithmeticExprHasCommandSubstitution(node.Slice.Length) {
				return true
			}
			if node.Slice.Length != nil && arithmeticExprReferencesTaintedVar(node.Slice.Length, tainted) {
				return true
			}
		}
		return node.Param != nil && node.Exp != nil &&
			(node.Exp.Op == syntax.AssignUnset || node.Exp.Op == syntax.AssignUnsetOrNull) &&
			accountEnvironmentNameDenied(node.Param.Value, names)
	case *syntax.BinaryArithm:
		return arithmeticAssignmentMutatesAccountEnvironment(node, names)
	case *syntax.UnaryArithm:
		return arithmeticIncrementMutatesAccountEnvironment(node, names)
	case *syntax.ArithmCmd:
		// `(( expr ))`. A command substitution inside the arithmetic is
		// re-evaluated as fresh arithmetic by bash and can assign a denied name
		// via its output; the literal assignment form is still caught by the
		// BinaryArithm/UnaryArithm arms below once this returns false.
		// A variable reference to a tainted var (one assigned from a command
		// substitution earlier in the same command) is equally unprovable: bash
		// re-evaluates the variable's value as arithmetic, so the prior
		// substitution's stdout becomes a deferred arithmetic mutation.
		if arithmeticExprHasCommandSubstitution(node.X) {
			return true
		}
		return arithmeticExprReferencesTaintedVar(node.X, tainted)
	case *syntax.ArithmExp:
		// `$(( expr ))`. Same re-evaluation hazard as `(( ))`; appears inside a
		// word (e.g. `echo $(( ... ))` or `x=$(( ... ))`), and the literal
		// assignment form is still caught by the arithm arms below.
		if arithmeticExprHasCommandSubstitution(node.X) {
			return true
		}
		return arithmeticExprReferencesTaintedVar(node.X, tainted)
	case *syntax.LetClause:
		// A bash `let` clause parsed as a builtin (the POSIX parse keeps `let` as
		// a CallExpr and is handled by letMutatesAccountEnvironment). The
		// command-substitution hazard is the same; returning false lets the walk
		// descend so a literal assignment (BinaryArithm/UnaryArithm) is still
		// caught.
		for _, expr := range node.Exprs {
			if arithmeticExprHasCommandSubstitution(expr) {
				return true
			}
			if arithmeticExprReferencesTaintedVar(expr, tainted) {
				return true
			}
		}
		return false
	case *syntax.BinaryTest:
		// Numeric comparison operators in `[[ ]]` (-eq, -ne, -lt, -gt, -le,
		// -ge) cause bash to evaluate both operands as arithmetic. A command
		// substitution in either operand is re-evaluated as fresh arithmetic by
		// bash and can assign a denied name via its output, e.g.
		// `[[ 0 -eq $(printf CODEX_HOME=1) ]]`. A tainted variable in either
		// operand is the deferred form of the same bypass. Fail closed on either.
		switch node.Op {
		case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
			if wordHasCommandSubstitution(node.X) || wordHasCommandSubstitution(node.Y) {
				return true
			}
			if wordReferencesTaintedVar(node.X, tainted) || wordReferencesTaintedVar(node.Y, tainted) {
				return true
			}
		}
		return false
	case *syntax.CStyleLoop:
		// C-style `for (( init; cond; post ))`. All three clauses are
		// evaluated as arithmetic by bash; the same command-substitution and
		// tainted-variable hazards apply. Fail closed on either. Returning
		// true here prevents the walk from descending into Init/Cond/Post a
		// second time; the BinaryArithm/UnaryArithm arms below still catch
		// literal assignments when the loop is safe (no substitution, no
		// tainted variable).
		for _, expr := range []syntax.ArithmExpr{node.Init, node.Cond, node.Post} {
			if expr == nil {
				continue
			}
			if arithmeticExprHasCommandSubstitution(expr) {
				return true
			}
			if arithmeticExprReferencesTaintedVar(expr, tainted) {
				return true
			}
		}
		return false
	case *syntax.UnaryTest:
		return unaryTestMutatesAccountEnvironment(node)
	default:
		return false
	}
}

func arithmeticAssignmentMutatesAccountEnvironment(expr *syntax.BinaryArithm, names map[string]struct{}) bool {
	switch expr.Op {
	case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn,
		syntax.QuoAssgn, syntax.RemAssgn, syntax.AndAssgn, syntax.OrAssgn,
		syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn, syntax.AndBoolAssgn,
		syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
		return arithmeticTargetMutates(expr.X, names)
	default:
		return false
	}
}

func arithmeticIncrementMutatesAccountEnvironment(expr *syntax.UnaryArithm, names map[string]struct{}) bool {
	if expr.Op != syntax.Inc && expr.Op != syntax.Dec {
		return false
	}
	return arithmeticTargetMutates(expr.X, names)
}

// arithmeticTargetMutates decides an arithmetic assignment or increment by its
// TARGET, and fails closed when it cannot read that target literally.
//
// Reading it as "not literal, therefore not a denied name" was the hole:
// `CODEX_HOME[0]` is an indexed-array element, which this parser reports as a
// non-literal word, so an assignment straight at the protected name scored as
// safe. bash applies `(( CODEX_HOME[0]=1 ))` to the exported SCALAR and
// converts it to an indexed array — after which a child reads CODEX_HOME as
// EMPTY, so the selected account root is destroyed rather than merely
// replaced. A dynamic target such as `${name}` is unprovable for the same
// reason.
//
// accountEnvironmentOperandDenied already fails closed on any subscript it CAN
// read; this is the same rule for the ones it cannot.
func arithmeticTargetMutates(target syntax.ArithmExpr, names map[string]struct{}) bool {
	name, ok := arithmeticAccountEnvironmentName(target)
	if !ok {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func arithmeticAccountEnvironmentName(expr syntax.ArithmExpr) (string, bool) {
	word, ok := expr.(*syntax.Word)
	if !ok {
		return "", false
	}
	return literalShellWord(word)
}

func callMutatesAccountEnvironment(call *syntax.CallExpr, names map[string]struct{}, tainted map[string]struct{}) bool {
	for _, assign := range call.Assigns {
		if assign != nil && assign.Name != nil {
			if _, denied := names[assign.Name.Value]; denied {
				return true
			}
		}
	}

	memo := newOperandTailMemo()
	words, unsafe := unwrapAccountCommand(call.Args, names, memo)
	if unsafe || len(words) == 0 {
		return unsafe
	}
	return unwrappedAccountCommandMutates(words, names, tainted, memo)
}

func unwrappedAccountCommandMutates(words []*syntax.Word, names map[string]struct{}, tainted map[string]struct{}, memo operandTailMemo) bool {
	if _, literal := literalShellWord(words[0]); !literal {
		// A dynamic command name can resolve to env or a same-shell builtin such
		// as unset/export, so its effect on the selected identity is unprovable.
		return true
	}
	switch {
	case isAccountCommandName(words[0], "env"):
		return envCallMutatesAccountEnvironment(words[1:], names, false, memo)
	case isBareName(words[0], "unset"):
		return unsetMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "set"):
		return setMutatesAccountEnvironment(words[1:])
	case isBareName(words[0], "hash"):
		return hashMutatesAccountEnvironment(words[1:])
	case isAccountDeclarationBuiltin(words[0]):
		return declarationMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "read"):
		return readMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "getopts"):
		return getoptsMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "printf"):
		return printfMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "let"):
		return letMutatesAccountEnvironment(words[1:], names, tainted)
	case isBareName(words[0], "mapfile"), isBareName(words[0], "readarray"):
		return arrayReadMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "wait"):
		return waitMutatesAccountEnvironment(words[1:], names)
	case isBareName(words[0], "test"), isBareName(words[0], "["):
		return variableTestMutatesAccountEnvironment(words[1:])
	case isBareName(words[0], "eval"), isBareName(words[0], "."),
		isBareName(words[0], "source"), isBareName(words[0], "trap"),
		isBareName(words[0], "alias"), isBareName(words[0], "fc"),
		isBareName(words[0], "history"), isBareName(words[0], "enable"):
		// These builtins execute or schedule another shell program in the same
		// environment, define commands that do, or load/replay unparsed history.
		// Proving their effects would require a second parser pass with runtime
		// expansion, so a scoped sibling refuses them.
		return true
	case shellCommandIsUnproven(words):
		return true
	default:
		return false
	}
}

// unwrapAccountCommand removes shell wrappers that still execute the remaining
// words in the current environment. Dynamic or unsupported wrapper forms are
// unsafe because they could resolve to env or an environment-mutating builtin.
//
// Every position the peel passes through gets the chain's result, so a later
// walk that starts inside the chain (an operand check, or an env command word)
// answers from the memo instead of re-peeling the rest of it (#4966).
func unwrapAccountCommand(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool) {
	var chain []*syntax.Word
	var result unwrapResult
	for len(words) > 0 {
		if cached, seen := memo.unwrapped[words[0]]; seen {
			result = cached
			break
		}
		chain = append(chain, words[0])
		next, peeled, unsafe := peelAccountWrapper(words, names, memo)
		if !peeled {
			result = unwrapResult{words: next, unsafe: unsafe}
			break
		}
		words = next
	}
	for _, word := range chain {
		memo.unwrapped[word] = result
	}
	return result.words, result.unsafe
}

// peelAccountWrapper removes the one wrapper at the head of words. It returns
// (next, peeled, unsafe): when peeled, the walk continues with next; otherwise
// next and unsafe are unwrapAccountCommand's result.
func peelAccountWrapper(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) ([]*syntax.Word, bool, bool) {
	switch {
	case isBareName(words[0], "exec"):
		words = words[1:]
		if len(words) > 0 {
			option, literal := literalShellWord(words[0])
			if !literal {
				return nil, false, true
			}
			if option == "--" {
				words = words[1:]
			} else if strings.HasPrefix(option, "-") && option != "-" {
				// Bash and other shells give exec options environment-changing
				// behavior (notably `exec -c`). No option is needed by af's
				// sibling path, so unsupported forms fail closed.
				return nil, false, true
			}
		}
		for len(words) > 0 {
			name, assignment := shellWordAssignmentName(words[0])
			if !assignment {
				break
			}
			if _, denied := names[name]; denied {
				return nil, false, true
			}
			words = words[1:]
		}
	case isBareName(words[0], "command"):
		var unsafe bool
		words, unsafe = unwrapCommandBuiltin(words[1:])
		if unsafe {
			return nil, false, true
		}
	case isBareName(words[0], "builtin"):
		words = words[1:]
		if len(words) > 0 && wordEquals(words[0], "--") {
			words = words[1:]
		}
		if len(words) > 0 {
			if _, literal := literalShellWord(words[0]); !literal {
				return nil, false, true
			}
		}
	case isAccountCommandName(words[0], "nohup"):
		var unsafe bool
		words, unsafe = unwrapNohup(words[1:])
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "nice"):
		var unsafe bool
		words, unsafe = unwrapNice(words[1:], names, memo)
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "timeout"):
		var unsafe bool
		words, unsafe = unwrapTimeout(words[1:], names, memo)
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "setsid"):
		var unsafe bool
		words, unsafe = unwrapSetsid(words[1:], names, memo)
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "stdbuf"):
		var unsafe bool
		words, unsafe = unwrapStdbuf(words[1:], names, memo)
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "ionice"):
		var unsafe bool
		words, unsafe = unwrapIonice(words[1:], names, memo)
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "taskset"):
		var unsafe bool
		words, unsafe = unwrapTaskset(words[1:], names, memo)
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "xargs"):
		var unsafe bool
		words, unsafe = unwrapXargs(words[1:], names, memo)
		if unsafe || childTailBuriesMutation(words, names, memo) {
			return nil, false, true
		}
	default:
		// An unmodeled argv-passthrough wrapper (strace/perf/valgrind/
		// gdb --args/chrt/numactl/unshare/prlimit/...) runs a child
		// command and forwards argv, so a PATH-shadowed copy
		// (`shift N; exec "$@"`) can replace the selected account
		// environment exactly the way a shadowed nohup or ionice can. A
		// static command-line check cannot enumerate every shape a
		// shadowed wrapper can shift to (a buried suffix scan was bypassed
		// twice by Codex), so for the wrappers in this class the check
		// fails closed whatever their tail contains: the effect of an
		// unmodeled wrapper on the account environment is unknown, so the
		// command is unverifiable rather than clean.
		if isUnmodeledArgvPassthroughWrapper(words[0]) {
			return nil, false, true
		}
		if unrecognizedWrapperHidesAccountAssignment(words, names, memo) {
			return nil, false, true
		}
		return words, false, false
	}
	return words, true, false
}

// unrecognizedWrapperHidesAccountAssignment reports whether the literal tail
// words of an unrecognized argv-passthrough wrapper carry a NAME=... assignment
// whose NAME is one af removes for the selected account, specifically by
// nesting an env invocation inside the wrapper's argument list.
//
// unwrapAccountCommand peels a CLOSED list of wrappers (exec/command/builtin/
// nohup/nice/timeout/setsid/stdbuf/ionice/taskset/xargs); every other binary
// that runs a child command and passes argv through (strace/perf/valgrind/
// gdb --args/...) falls to the default arm and was returned opaque. Wrapping the
// modelled `env NAME=value <agent>` mutation — which the guard already refuses
// bare and under every modelled wrapper — in an unmodeled wrapper hid the inner
// assignment from the walk, so `strace env CODEX_HOME=/other codex` was accepted
// while `nohup env CODEX_HOME=/other codex` was refused.
//
// This lifts envCallMutatesAccountEnvironment's NAME= rule one level: scan
// the wrapper's literal argv tail for an `env` invocation and delegate to
// envCallMutatesAccountEnvironment when one is found. Commands that do not
// contain a nested env invocation are not refused even when an argument
// resembles a NAME=value token, so noun-uses such as `echo CODEX_HOME=/tmp`,
// `rg 'OPENAI_API_KEY='`, `man env`, `make env`, `git grep env`, `ls env/bin`,
// `pip show env`, or `strace -p 1234 env` (none of which carries a nested env
// invocation with a denied assignment) stay allowed.
//
// The same tail scan applies the modeled path's other two rules one level
// down: a shell word with argv is judged by shellCommandIsUnproven exactly as
// a bare `sh -c '...'` command is, and a literal option word whose VALUE is a
// denied name or assignment (xargs --process-slot-var=NAME, strace -E var=val
// and --env=var=val) is refused — a wrapper's own options can place the
// mutation without any env word.
//
// The scan is memoized per (position, strace): the verdict at a tail word
// depends only on the words from there on, so the tail of a wrapper nested in
// another wrapper's tail — `echo env env … env x` — is scanned once, not once
// per enclosing env word (#4966).
func unrecognizedWrapperHidesAccountAssignment(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	strace := isAccountCommandName(words[0], "strace")
	if strace && straceOptionsRefused(words[1:], names, memo) {
		return true
	}
	return wrapperTailHidesAccountAssignment(words[1:], strace, names, memo)
}

// wrapperTailHidesAccountAssignment scans an unrecognized wrapper's tail from
// words[0]. Every position the scan steps on gets the scan's answer: the scan
// from there follows the same steps to the same end.
func wrapperTailHidesAccountAssignment(words []*syntax.Word, strace bool, names map[string]struct{}, memo operandTailMemo) bool {
	var chain []*syntax.Word
	answer := false
	for len(words) > 0 {
		if cached, seen := memo.wrapperTails[wrapperTailKey{word: words[0], strace: strace}]; seen {
			answer = cached
			break
		}
		chain = append(chain, words[0])
		width, hides := wrapperTailWordHidesAccountAssignment(words, strace, names, memo)
		if hides {
			answer = true
			break
		}
		words = words[width:]
	}
	for _, word := range chain {
		memo.wrapperTails[wrapperTailKey{word: word, strace: strace}] = answer
	}
	return answer
}

// wrapperTailWordHidesAccountAssignment judges the tail word at words[0] and
// reports how many words it consumed.
func wrapperTailWordHidesAccountAssignment(words []*syntax.Word, strace bool, names map[string]struct{}, memo operandTailMemo) (int, bool) {
	word := words[0]
	if isAccountCommandName(word, "strace") && straceOptionRunRefused(words[1:], names, false, memo) {
		// A strace in an unrecognized wrapper's — or another strace's — child
		// command position gets the same option-run judgement the outermost one
		// does (#5033); the recognized wrappers reach it through the default arm.
		return 1, true
	}
	if isAccountCommandName(word, "env") {
		// A nested env only mutates the child it execs; requireCommand
		// keeps an `env CODEX_HOME=/tmp` whose argv ends there — env's
		// print mode, which overrides nothing — allowed.
		return 1, envCallMutatesAccountEnvironment(words[1:], names, true, memo)
	}
	literal, ok := literalShellWord(word)
	if !ok && strace && stracePrefixMutates(word, names) {
		// The option word itself is judged before the unprovable-word path
		// below, which only reads the words AFTER it (#5033).
		return 1, true
	}
	if !ok {
		// An unprovable tail word can itself expand to `env` (or to a
		// multiword `env NAME=value` after word splitting); judge the
		// words after it as that invocation's argv. envScan admits a
		// substituted xargs marker after a literal env's command slot, but
		// this env is only a hypothesis, so a later marker is refused here as
		// a later "$x" is.
		return 1, memo.xargsItemFollows(words[1:]) ||
			envCallMutatesAccountEnvironment(words[1:], names, true, memo)
	}
	if !strings.HasPrefix(literal, "-") {
		// A shell in the wrapper's tail gets the same verdict a bare
		// shell command gets: `strace sh -c 'unset CODEX_HOME; codex'`
		// execs the literal script the modeled path already refuses
		// under `nice sh -c ...`. Only the trusted account-shell form
		// proves out. A trailing shell name with no argv (echo sh,
		// strace -p 1 sh) has nothing to judge and stays allowed.
		return 1, len(words) > 1 && knownShellName(filepath.Base(literal)) &&
			shellCommandIsUnproven(words)
	}
	if strace {
		switch {
		case literal == "-E" || straceEnvLongName(literal):
			// strace's env option takes var[=val] as a separate word and
			// injects or REMOVES the variable in the traced child's
			// environment — the same mutation the env arm refuses, in
			// option spelling. It is strace-only because -E means
			// extended-regexp to grep and friends.
			if len(words) < 2 {
				return 1, true
			}
			value, ok := literalShellWord(words[1])
			return 2, !ok || accountEnvironmentOperandDenied(value, names)
		case strings.HasPrefix(literal, "--") && strings.Contains(literal, "="):
			name, operand, _ := strings.Cut(literal, "=")
			if straceEnvLongName(name) {
				return 1, accountEnvironmentOperandDenied(operand, names)
			}
		case !strings.HasPrefix(literal, "--"):
			// A short-option cluster reaches -E after its flags
			// (`-fECODEX_HOME=/o`), but an option that takes an operand
			// consumes the REST of the cluster (`-oECODEX_HOME=x` is -o with
			// that file name), so read it with strace's real option table.
			if operand, isEnv := straceClusterEnvOperand(literal[1:]); isEnv {
				if operand != "" {
					return 1, accountEnvironmentOperandDenied(operand, names)
				}
				// A trailing -E takes the NEXT word (`-fE CODEX_HOME=/o`).
				if len(words) < 2 {
					return 1, true
				}
				value, ok := literalShellWord(words[1])
				return 2, !ok || accountEnvironmentOperandDenied(value, names)
			}
		}
	}
	// An unrecognized wrapper may expose options that mutate its
	// child's environment in option-value form — xargs's
	// --process-slot-var=NAME sets NAME on every exec'd command, and
	// strace's --env=var=val is analogous. A literal `--opt=DENIED` or
	// `--opt=DENIED=value` is refused when its value names a denied
	// variable or carries a denied assignment.
	if _, value, ok := strings.Cut(literal, "="); ok {
		return 1, accountEnvironmentFreeTextOptionValueDenied(value, names)
	}
	return 1, false
}

// childTailBuriesMutation scans the tail a modeled wrapper returns for a
// LITERAL buried suffix a PATH-shadowed wrapper (`shift N; exec "$@"`) would
// promote to the executed command, as unwrapIonice and unwrapTaskset do for
// their own tails (#4708). nohup, nice, timeout, setsid, stdbuf and xargs
// returned the tail unscanned, so `nice echo a a a xargs --process-slot-var
// CODEX_HOME codex` passed where the same payload under ionice was refused
// (#5034); xargs returned its child tail through the separate
// unwrapXargs/xargsChild input-reach path, so it was the last of the modeled
// wrappers to be closed. words[0] is the child command the main walk judges
// next; the scan covers what follows it.
//
// Only literal boundaries are judged. ionice also treats a non-literal word as
// a possible boundary, which refuses every `ionice codex "$PROMPT"`; applying
// that to nohup, nice, timeout, setsid and stdbuf would refuse the ordinary
// `nohup claude "$ARGS"` launch, so the dynamic-boundary reading stays open
// (see #5034) and the literal payloads the report names are closed here.
func childTailBuriesMutation(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	return len(words) > 1 && laterLiteralChildSuffixMutates(words[1:], names, memo)
}

// isUnmodeledArgvPassthroughWrapper reports whether word is one of the argv-
// passthrough wrappers the modeled peel list does not name but the threat
// model still treats as a child-executing wrapper: the binary runs a child
// command and forwards argv, so a PATH-shadowed copy (`shift N; exec "$@"`)
// can replace the selected account environment exactly the way a shadowed
// nohup or ionice can. Matched by basename, the way the modeled arms match
// (isAccountCommandName uses filepath.Base), so a path form (/usr/bin/perf)
// is the same wrapper. The class is open in principle; this names the members
// the parity test enumerates — the tracing tools (strace/ltrace/perf/valgrind/
// gdb) and the util-linux argv-passthrough launchers that exec a child and
// forward argv the same way (chrt/numactl/unshare/prlimit) — mirroring the
// closed list the strace special-case in unrecognizedWrapperHidesAccountAssignment
// already keeps.
//
// peelAccountWrapper's default arm refuses a listed passthrough wrapper
// whatever its tail contains, rather than pattern-matching a buried suffix.
// Ordinary commands that do not exec a child (echo/rg/codex/grep/...) are
// intentionally NOT wrappers: their arguments never reach an exec boundary,
// so they fall through to unrecognizedWrapperHidesAccountAssignment (which
// still judges a visible account-environment assignment under any such
// command) and otherwise stay admitted.
//
// Scope: this check covers the visible command line and the modeled
// wrappers it peels. A user's own wrapper script (a program_overrides
// target af does not model) has internals the command line cannot see, so
// what it does with the account environment is outside what af can verify;
// refusing every unmodeled executable would break every custom-script
// program_overrides, which is out of this fix's scope and is a product
// decision for the owner.
func isUnmodeledArgvPassthroughWrapper(word *syntax.Word) bool {
	value, literal := literalShellWord(word)
	if !literal {
		return false
	}
	switch filepath.Base(value) {
	case "strace", "ltrace", "perf", "valgrind", "gdb",
		"chrt", "numactl", "unshare", "prlimit":
		return true
	}
	return false
}

// stracePrefixMutates judges a strace option word that carries an expansion by
// the literal text before its first non-literal part, where that text alone
// decides: strace's -E/--env takes var[=val], so a literal NAME ahead of the
// `=` refuses on its own whatever the value expands to (`-ECODEX_HOME=$V`
// refuses, `-EPORT=$V` does not). Whether an unprovable NAME or option identity
// refuses depends on a traced command following, which a single word cannot
// see; straceOptionsRefused judges that over strace's option run.
func stracePrefixMutates(word *syntax.Word, names map[string]struct{}) bool {
	var prefix strings.Builder
	for _, part := range word.Parts {
		if !appendLiteralShellPart(&prefix, part) {
			break
		}
	}
	text := prefix.String()
	if !strings.HasPrefix(text, "-") {
		return false
	}
	var operand string
	isEnv := false
	if name, rest, found := strings.Cut(text, "="); found && straceEnvLongName(name) {
		operand, isEnv = rest, true
	}
	if !isEnv && !strings.HasPrefix(text, "--") {
		operand, isEnv = straceClusterEnvOperand(text[1:])
	}
	return isEnv && strings.Contains(operand, "=") && accountEnvironmentOperandDenied(operand, names)
}

// straceEnvLongName reports whether word is --env or an abbreviation of it:
// strace parses long options with getopt_long, which accepts any unambiguous
// prefix, so `--en=CODEX_HOME=/o` sets the variable under the installed
// strace. Three or more characters are treated as --env (fail closed — `--e`
// would be ambiguous to getopt, and refusing it costs nothing real).
func straceEnvLongName(word string) bool {
	return len(word) >= 3 && strings.HasPrefix("--env", word)
}

// straceClusterEnvOperand reads a short-option cluster (the word without its
// leading dash) with strace's option table: flags that take no operand are
// skipped, and the first option that takes one consumes the rest of the
// cluster. It reports that operand only when that option is -E; any other
// operand-taking option (-o, -p, -s, …) owns the rest, so `oECODEX_HOME=x` is
// a file name. A letter the table cannot place ends the read without a verdict
// — straceOptionsRefused refuses it where the word is a strace option.
func straceClusterEnvOperand(cluster string) (operand string, isEnv bool) {
	for i := 0; i < len(cluster); i++ {
		switch {
		case strings.IndexByte(straceShortFlags, cluster[i]) >= 0:
		case strings.IndexByte(straceShortValueFlags, cluster[i]) >= 0:
			return cluster[i+1:], cluster[i] == 'E'
		default:
			return "", false
		}
	}
	return "", false
}
