package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
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

func accountEnvironmentNameDenied(name string, names map[string]struct{}) bool {
	_, denied := names[name]
	return denied
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
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "nice"):
		var unsafe bool
		words, unsafe = unwrapNice(words[1:], names, memo)
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "timeout"):
		var unsafe bool
		words, unsafe = unwrapTimeout(words[1:], names, memo)
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "setsid"):
		var unsafe bool
		words, unsafe = unwrapSetsid(words[1:], names, memo)
		if unsafe {
			return nil, false, true
		}
	case isAccountCommandName(words[0], "stdbuf"):
		var unsafe bool
		words, unsafe = unwrapStdbuf(words[1:], names, memo)
		if unsafe {
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
		if unsafe {
			return nil, false, true
		}
	default:
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
	return wrapperTailHidesAccountAssignment(words[1:], isAccountCommandName(words[0], "strace"), names, memo)
}

// wrapperTailHidesAccountAssignment scans an unrecognized wrapper's tail from
// words[0]. Every position the scan steps on gets the scan's answer: the scan
// from there follows the same steps to the same end.
func wrapperTailHidesAccountAssignment(words []*syntax.Word, strace bool, names map[string]struct{}, memo operandTailMemo) bool {
	var chain []*syntax.Word
	var chainInOption []bool
	answer := false
	// strace parses options only up to its first non-option word (the traced
	// command) or "--"; a -E/--env-shaped word past that point is the
	// command's argument and cannot alter the traced environment. Track the
	// option region across the scan so the strace-specific arms — and the
	// generic --opt=DENIED arm, which is a wrapper-option check — apply only
	// to strace's options, not to the traced command's argv. (The env and
	// shell arms are not gated: a traced command that is itself env or a shell
	// is still judged by them.) pending marks the argv word a value-taking
	// strace option consumes as its value, so a value word (e.g. the file
	// after -o) is not mistaken for the traced command. Non-strace wrappers
	// never leave the region, preserving their existing scan.
	inOption := true
	pending := false
	for len(words) > 0 {
		if cached, seen := memo.wrapperTails[wrapperTailKey{word: words[0], strace: strace, inOption: inOption}]; seen {
			answer = cached
			break
		}
		chain = append(chain, words[0])
		chainInOption = append(chainInOption, inOption)
		width, hides, boundary, pendingValue := wrapperTailWordHidesAccountAssignment(words, strace, inOption, names, memo)
		if hides {
			answer = true
			break
		}
		switch {
		case pending:
			// This word is a value-taking strace option's value, not the
			// traced command; step past it and keep parsing strace options.
			pending = false
		case strace && boundary:
			inOption = false
		case strace && pendingValue:
			pending = true
		}
		words = words[width:]
	}
	for i, word := range chain {
		memo.wrapperTails[wrapperTailKey{word: word, strace: strace, inOption: chainInOption[i]}] = answer
	}
	return answer
}

// wrapperTailWordHidesAccountAssignment judges the tail word at words[0] and
// reports how many words it consumed. boundary reports that this word ends
// strace's option region (a literal non-option word — the traced command — or
// "--"); pendingValue reports that this word is a strace value-taking option
// whose value is the NEXT argv word (so that next word is a value, not the
// command). Both are meaningful only for a strace wrapper still inside its
// option region (strace && inOption); the caller uses them to stop applying
// the strace-specific and generic wrapper-option arms to the traced command's
// argv.
func wrapperTailWordHidesAccountAssignment(words []*syntax.Word, strace bool, inOption bool, names map[string]struct{}, memo operandTailMemo) (int, bool, bool, bool) {
	word := words[0]
	if isAccountCommandName(word, "env") {
		// A nested env only mutates the child it execs; requireCommand
		// keeps an `env CODEX_HOME=/tmp` whose argv ends there — env's
		// print mode, which overrides nothing — allowed. env is judged by
		// this arm regardless of the strace option region (it is the traced
		// command under strace too), so it leaves the region as the caller
		// found it and any strace options env's argv re-enters are judged
		// the same way the pre-boundary scan judged them.
		return 1, envCallMutatesAccountEnvironment(words[1:], names, true, memo), false, false
	}
	literal, ok := literalShellWord(word)
	if !ok {
		if strace && inOption && straceAttachedEnvOptionHides(word, names) {
			// The attached -E NAME[=value] and --env=NAME[=value] forms
			// whose value holds a shell expansion land here:
			// literalShellWord fails on the ParamExp/CmdSubst, discarding
			// the literal option marker and NAME that precede it. The
			// separate-word `-E <word>` and the attached literal
			// `-ENAME=` forms already fail closed; the non-literal
			// attached form must fail closed the same way. This is
			// strace-only: -E means extended-regexp to grep and others
			// (account_environment_wrapper_test.go:250), and --env=
			// belongs to strace only when its caller set the strace flag
			// from the outermost wrapper.
			return 1, true, false, false
		}
		// An unprovable tail word can itself expand to `env` (or to a
		// multiword `env NAME=value` after word splitting); judge the
		// words after it as that invocation's argv. envScan admits a
		// substituted xargs marker after a literal env's command slot, but
		// this env is only a hypothesis, so a later marker is refused here as
		// a later "$x" is. A non-literal word is left in the option region:
		// its expansion can still be or complete a strace env option, so the
		// attached check stays armed for the words that follow it.
		return 1, memo.xargsItemFollows(words[1:]) ||
			envCallMutatesAccountEnvironment(words[1:], names, true, memo), false, false
	}
	if !strings.HasPrefix(literal, "-") {
		// A shell in the wrapper's tail gets the same verdict a bare
		// shell command gets: `strace sh -c 'unset CODEX_HOME; codex'`
		// execs the literal script the modeled path already refuses
		// under `nice sh -c ...`. Only the trusted account-shell form
		// proves out. A trailing shell name with no argv (echo sh,
		// strace -p 1 sh) has nothing to judge and stays allowed. This
		// literal non-option word is the traced command under strace,
		// so it ends the option region for the words that follow it.
		return 1, len(words) > 1 && knownShellName(filepath.Base(literal)) &&
			shellCommandIsUnproven(words), strace && inOption, false
	}
	// A literal option word. "--" ends strace's option region; any other
	// option keeps parsing options. boundary reports that boundary so the
	// caller stops applying the wrapper-option arms to the trailing argv.
	boundary := strace && inOption && literal == "--"
	pendingValue := false
	if strace && inOption {
		switch {
		case literal == "-E" || literal == "--env":
			// strace's env option takes var[=val] as a separate word and
			// injects or REMOVES the variable in the traced child's
			// environment — the same mutation the env arm refuses, in
			// option spelling. It is strace-only because -E means
			// extended-regexp to grep and friends. The value word is
			// consumed here, so it is not pending.
			if len(words) < 2 {
				return 1, true, boundary, false
			}
			value, ok := literalShellWord(words[1])
			return 2, !ok || accountEnvironmentOperandDenied(value, names), boundary, false
		case strings.HasPrefix(literal, "-E"):
			return 1, accountEnvironmentOperandDenied(literal[2:], names), boundary, false
		default:
			pendingValue = straceOptionAwaitsValue(literal)
		}
	}
	// An unrecognized wrapper may expose options that mutate its child's
	// environment in option-value form — xargs's --process-slot-var=NAME
	// sets NAME on every exec'd command, and strace's --env=var=val is
	// analogous. A literal `--opt=DENIED` or `--opt=DENIED=value` is refused
	// when its value names a denied variable or carries a denied assignment.
	// This is a wrapper-option check: it runs for a non-strace wrapper's
	// options and for strace's options while the scan is still inside
	// strace's option region. Past strace's traced command a --opt-shaped
	// word is the command's argument, not a wrapper option, so a denied name
	// it carries stays allowed there.
	if !strace || inOption {
		if _, value, ok := strings.Cut(literal, "="); ok {
			return 1, accountEnvironmentOperandDenied(value, names), boundary, pendingValue
		}
	}
	return 1, false, boundary, pendingValue
}

func variableTestMutatesAccountEnvironment(words []*syntax.Word) bool {
	for idx := 0; idx < len(words); idx++ {
		option, literal := literalShellWord(words[idx])
		if !literal {
			return true
		}
		if option != "-v" {
			continue
		}
		if idx+1 >= len(words) {
			return true
		}
		operand, literal := literalShellWord(words[idx+1])
		if !literal || strings.Contains(operand, "[") {
			return true
		}
		idx++
	}
	return false
}

func unaryTestMutatesAccountEnvironment(test *syntax.UnaryTest) bool {
	if test.Op != syntax.TsVarSet {
		return false
	}
	word, ok := test.X.(*syntax.Word)
	if !ok {
		return true
	}
	operand, literal := literalShellWord(word)
	return !literal || strings.Contains(operand, "[")
}

func unwrapCommandBuiltin(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			words = words[1:]
			break
		}
		if !strings.HasPrefix(option, "-") || option == "-" {
			break
		}
		for _, flag := range option[1:] {
			switch flag {
			case 'p':
			case 'v', 'V':
				return nil, false // query-only: command does not execute its operand
			default:
				return nil, true
			}
		}
		words = words[1:]
	}
	if len(words) > 0 {
		if _, literal := literalShellWord(words[0]); !literal {
			return nil, true
		}
	}
	return words, false
}

// envCallMutatesAccountEnvironment reports whether the env invocation formed by
// words mutates a denied name or execs something unprovable. requireCommand
// restricts that verdict to env invocations carrying a command word: print-mode
// env mutates only its own process, which is the right reading when the words
// sit in an unrecognized wrapper's tail rather than forming the command itself —
// an `env CODEX_HOME=/tmp` with nothing after it runs env's print path, not an
// override of a running agent.
//
// It is envcommand.Parse over the words literalized by envArgvWord — any word
// that fails envArgvWord refuses the call, wherever it sits — computed from
// per-position memos so that nested env words cost O(1) each (#4966).
func envCallMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	key := envCallKey{word: words[0], requireCommand: requireCommand}
	if answer, seen := memo.envCalls[key]; seen {
		return answer
	}
	answer := envCallMutatesAccountEnvironmentUncached(words, names, requireCommand, memo)
	memo.envCalls[key] = answer
	return answer
}

func envCallMutatesAccountEnvironmentUncached(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if memo.envArgvSuffixUnprovable(words) {
		return true
	}
	scan := memo.envScan(words, envcommand.Start, names)
	if scan.refused {
		return true
	}
	if requireCommand && scan.command == nil {
		return false
	}
	if scan.denied {
		return true
	}
	if scan.command == nil {
		return false
	}
	// env's command word is literal: envArgvWord passes a non-literal word
	// only when it spells an assignment, which the scan consumed as one. So
	// the operand-tail judgment — unwrap, then judge the peeled command — is
	// exactly the command's verdict, and shares its memo. The walk here has
	// no access to tainted; the taint check is applied at the arithmetic-node
	// level by nodeMutatesAccountEnvironment.
	return wrapperOperandTailMutates(scan.command, names, memo)
}

func shellCommandIsUnproven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	if !literal || !knownShellName(filepath.Base(command)) {
		return false
	}
	return !accountShellCommandWordsProven(words)
}

func accountShellCommandWordsProven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	// A sibling shell may read profiles, stdin, a script, or a command string.
	// The only statically proven form is the same absolute, startup-free command
	// AccountShellCommand generates for a dedicated shell tab. What stdin can
	// carry is a property of the whole command, not of these words, so
	// ValidateAccountEnvironmentCommand checks it separately
	// (commandFeedsProvenShell).
	if !literal || !filepath.IsAbs(command) {
		return false
	}
	// zsh's startup-freedom is only half in its argv: the other half is the
	// ZDOTDIR pin, which ApplyAccountEnvironment attaches to the exact
	// generated command alone. Inside a longer command, zsh runs with whatever
	// environment a wrapper or earlier statement hands it, so it is never
	// proven here; ValidateAccountEnvironmentCommand admits the exact form
	// before this walk runs (#4474 review).
	if filepath.Base(command) == "zsh" {
		return false
	}
	// Every rejection that needs only the head and the word count runs before
	// the arguments are literalized: fileHasProvenShell asks this of every
	// suffix of every call, so literalizing first made any long command
	// quadratic (#4966). A suffix is literalized only when its length already
	// matches the trusted form's, which is a handful of words.
	want := trustedAccountShellArgs(command)
	if want == nil || len(words)-1 != len(want) {
		return false
	}
	args, literal := literalCommandArgs(words[1:])
	return literal && slices.Equal(args, want)
}

func knownShellName(name string) bool {
	switch name {
	case "ash", "bash", "csh", "dash", "fish", "ksh", "mksh", "sh", "tcsh", "zsh":
		return true
	default:
		return false
	}
}

func isAccountCommandName(word *syntax.Word, want string) bool {
	value, literal := literalShellWord(word)
	return literal && filepath.Base(value) == want
}

func unsetMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	functionsOnly := false
	options := true
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-f":
				functionsOnly = true
				continue
			case "-v":
				functionsOnly = false
				continue
			}
			if strings.HasPrefix(value, "-") {
				return true
			}
			options = false
		}
		if !functionsOnly {
			if accountEnvironmentOperandDenied(value, names) {
				return true
			}
		}
	}
	return false
}

// hashMutatesAccountEnvironment reports whether a `hash` call remaps a command
// name to a path of its own choosing.
//
// `hash -p pathname name` makes `name` resolve to `pathname`, so every later
// executable-name check in this walk answers about a different binary than the
// one that will actually run: after `hash -p /usr/bin/env runner`, the
// otherwise-unknown `runner` IS env, and `runner CODEX_HOME=/other codex`
// applies the replacement root through a name this walk never modelled.
//
// Only the remapping form is refused — `hash -r` and `hash name` merely
// maintain the lookup cache and leave every name meaning what it meant.
func hashMutatesAccountEnvironment(words []*syntax.Word) bool {
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if value == "--" || !strings.HasPrefix(value, "-") {
			return false
		}
		if strings.ContainsRune(value[1:], 'p') {
			return true
		}
	}
	return false
}

func isAccountDeclarationBuiltin(word *syntax.Word) bool {
	for _, name := range []string{"export", "readonly", "declare", "typeset", "local"} {
		if isBareName(word, name) {
			return true
		}
	}
	return false
}

func declarationMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for _, word := range words {
		if name, assignment := shellWordAssignmentName(word); assignment {
			if accountEnvironmentOperandDenied(name, names) {
				return true
			}
			options = false
			continue
		}
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			if value == "--" {
				options = false
				continue
			}
			if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
				if value == "-n" || value == "+n" || value == "--nameref" || value == "-i" {
					return true
				}
				if len(value) != 2 {
					return true
				}
				continue
			}
			options = false
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func readMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for len(words) > 0 && wordEquals(words[0], "-r") {
		words = words[1:]
	}
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if strings.HasPrefix(value, "-") {
			return true
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func getoptsMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	if len(words) < 2 {
		return false
	}
	name, literal := literalShellWord(words[1])
	if !literal {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func printfMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	if len(words) == 0 {
		return false
	}
	option, literal := literalShellWord(words[0])
	if !literal {
		return true
	}
	if strings.HasPrefix(option, "-v") && option != "-v" {
		return true
	}
	if option != "-v" || len(words) < 2 {
		return false
	}
	name, literal := literalShellWord(words[1])
	if !literal {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

func accountEnvironmentOperandDenied(value string, names map[string]struct{}) bool {
	name := value
	if strings.IndexByte(name, '[') >= 0 {
		// Bash evaluates indexed-array subscripts as arithmetic and can assign a
		// protected variable while resolving an unrelated base name. Proving the
		// subscript is side-effect-free requires runtime semantics, so fail closed.
		return true
	}
	if equal := strings.IndexByte(name, '='); equal >= 0 {
		name = name[:equal]
	}
	return accountEnvironmentNameDenied(name, names)
}
