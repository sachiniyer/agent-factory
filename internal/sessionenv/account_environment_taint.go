package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// arithmeticExprHasCommandSubstitution reports whether an arithmetic
// expression tree contains a command substitution (`$(...)` or backticks).
//
// bash re-evaluates the stdout of a command substitution as FRESH arithmetic
// before using it, including inside an array subscript that the parser reports
// as a plain read. The substitution can therefore print `NAME=value` and have
// bash execute it as an arithmetic assignment to a denied account-identity
// variable while the surrounding expression only appears to read it. The
// guard's own analysis judges the substitution's inner command (an `echo`) as
// inert data and never models the re-evaluation, and accountSubscriptInArithmetic
// only finds a literal `name[`, so neither can prove safety. Wherever a parsed
// arithmetic AST is treated as authoritative — `let`, `(( ))`, `$(( ))`, or a
// bash `let` clause — the presence of a command substitution makes the
// expression unprovable and the guard fails closed.
func arithmeticExprHasCommandSubstitution(expr syntax.ArithmExpr) bool {
	found := false
	syntax.Walk(expr, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// wordHasCommandSubstitution reports whether a shell word contains a command
// substitution (`$(...)` or backticks).
func wordHasCommandSubstitution(word syntax.Node) bool {
	found := false
	syntax.Walk(word, func(node syntax.Node) bool {
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// literalContainsDeniedArithAssignment reports whether a literal string, when
// evaluated by bash as an arithmetic expression, would perform an assignment to
// a denied account-environment variable. bash evaluates the entire string as
// arithmetic, so compound expressions like `0,CODEX_HOME=1` (comma operator)
// and compound assignments like `CODEX_HOME+=1` are also detected.
//
// The check parses the string as a bash arithmetic expression and walks the
// resulting AST for assignment nodes whose target is a denied name. A string
// that is not valid arithmetic cannot perform an arithmetic assignment, so
// parse failure is treated as not hazardous.
func literalContainsDeniedArithAssignment(value string, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	// Parse the literal as an arithmetic expression to catch compound forms
	// like `0,CODEX_HOME=1`, `CODEX_HOME+=1`, etc. If parsing fails the
	// expression is unprovable; treat it as hazardous.
	parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Arithmetic(strings.NewReader(value))
	if err != nil || parsed == nil {
		// Not valid arithmetic (or empty expression) — not hazardous as an
		// arithmetic assignment.
		return false
	}
	found := false
	syntax.Walk(parsed, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.BinaryArithm:
			switch n.Op {
			case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn,
				syntax.QuoAssgn, syntax.RemAssgn, syntax.AndAssgn, syntax.OrAssgn,
				syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn, syntax.AndBoolAssgn,
				syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
				// Fail closed when the assignment target cannot be read
				// literally: e.g. `CODEX_HOME[0]` is an indexed word that
				// arithmeticAccountEnvironmentName cannot literalize, so
				// `ok=false` must be treated as hazardous rather than safe.
				name, ok := arithmeticAccountEnvironmentName(n.X)
				if !ok || accountEnvironmentNameDenied(name, names) {
					found = true
					return false
				}
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				name, ok := arithmeticAccountEnvironmentName(n.X)
				if !ok || accountEnvironmentNameDenied(name, names) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// arithmeticExprReferencesTaintedVar reports whether an arithmetic expression
// tree contains a variable reference (bare word or `$name` expansion) of a
// tainted name. When a tainted variable appears inside arithmetic, bash
// re-evaluates that variable's value as fresh arithmetic — the same
// re-evaluation hazard as an inline command substitution — so the expression
// is unprovable.
//
// Array subscripts are NOT stripped: `arr[x]` evaluates `x` as arithmetic, so
// `x` must be checked against the tainted set in addition to `arr`. All
// identifiers inside `[…]` are extracted and tested. Additionally, `$name`
// expansions (ParamExp nodes) inside arithmetic are also checked, covering the
// `$(( $x ))` form.
func arithmeticExprReferencesTaintedVar(expr syntax.ArithmExpr, tainted map[string]struct{}) bool {
	if len(tainted) == 0 {
		return false
	}
	found := false
	syntax.Walk(expr, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.Word:
			name, literal := literalShellWord(n)
			if !literal {
				return true
			}
			// The word may be an array reference like `arr[x]`; bash evaluates
			// the subscript as arithmetic, so a tainted name inside `[…]` is
			// just as hazardous as the base name itself.
			if checkArithWordForTaint(name, tainted) {
				found = true
				return false
			}
		case *syntax.ParamExp:
			// `$name` inside arithmetic — bash expands the variable and then
			// re-evaluates its value as arithmetic, which is the same hazard
			// as a bare reference. Check the parameter name directly.
			if n.Param != nil {
				if _, taint := tainted[n.Param.Value]; taint {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// checkArithWordForTaint reports whether any shell identifier in the arithmetic
// word token (which may have the form `name`, `arr[idx]`, or `arr[a+b]`)
// appears in the tainted set.
//
// In arithmetic context, bash evaluates array subscripts as arithmetic too, so
// every identifier within `[…]` must be tested — not only the base array name.
func checkArithWordForTaint(word string, tainted map[string]struct{}) bool {
	// Collect every identifier in the word (base name and subscript names).
	// Identifiers are contiguous runs of [A-Za-z0-9_] not starting with a digit.
	start := -1
	checkIdent := func(s string) bool {
		if s == "" || (s[0] >= '0' && s[0] <= '9') {
			return false
		}
		_, taint := tainted[s]
		return taint
	}
	for i := 0; i <= len(word); i++ {
		if i < len(word) && isShellNameByte(word[i]) {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 && checkIdent(word[start:i]) {
				return true
			}
			start = -1
		}
	}
	return false
}

// wordReferencesTaintedVar reports whether a shell word (as used in a [[ ]]
// test operand) references a tainted variable name. bash re-evaluates the
// expanded value as arithmetic when the word appears in a numeric [[ ]]
// operand, so `[[ 0 -eq $x ]]` after `x=$(printf CODEX_HOME=1)` is the
// deferred form of the inline bypass.
//
// In bash arithmetic syntax, variable names can appear either as `$x`
// (ParamExp) or as the bare word `x` without `$`. A literal word that exactly
// matches a tainted variable name is therefore also a reference that must be
// refused.
func wordReferencesTaintedVar(word syntax.Node, tainted map[string]struct{}) bool {
	if len(tainted) == 0 {
		return false
	}
	found := false
	syntax.Walk(word, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.ParamExp:
			// `$x` form — explicit parameter expansion.
			if n.Param != nil {
				if _, taint := tainted[n.Param.Value]; taint {
					found = true
					return false
				}
			}
		case *syntax.Word:
			// Bare-word form — arithmetic syntax allows `x` (no `$`) as a
			// variable reference.  A literal word whose text matches a tainted
			// name is a reference to that variable.
			name, literal := literalShellWord(n)
			if literal {
				if checkArithWordForTaint(name, tainted) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

func accountSubscriptInArithmetic(expression string, names map[string]struct{}) bool {
	for name := range names {
		for offset := 0; offset < len(expression); {
			index := strings.Index(expression[offset:], name+"[")
			if index < 0 {
				break
			}
			index += offset
			if index == 0 || !isShellNameByte(expression[index-1]) {
				return true
			}
			offset = index + len(name)
		}
	}
	return false
}

func isShellNameByte(value byte) bool {
	return value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

// fileHasRuntimeInputToVariable reports whether a parsed shell file contains a
// same-shell builtin (`read`, `mapfile`, or `readarray`) that writes runtime
// input directly into a shell variable without a command substitution. When
// such a builtin is present AND the command also contains an arithmetic context,
// the value written by the builtin may be evaluated as arithmetic: e.g.
// `read x </tmp/payload; : $((x)); codex` lets an attacker store
// `CODEX_HOME=1` in x via the file, and bash re-evaluates it as arithmetic
// inside `$(( ))`. Neither coarse rule 1 (CmdSubst + arith) nor coarse rule 2
// (literal arith assignment + arith) fires, so this third coarse rule closes
// the gap.
func fileHasRuntimeInputToVariable(file syntax.Node) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name, literal := literalShellWord(call.Args[0])
		if !literal {
			return true
		}
		switch name {
		case "read", "mapfile", "readarray":
			found = true
			return false
		}
		return true
	})
	return found
}

// fileHasCmdSubst reports whether a parsed shell file contains any command
// substitution ($(…) or backtick form) anywhere in its AST, including inside
// subshells and nested constructs.
func fileHasCmdSubst(file syntax.Node) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		if _, ok := node.(*syntax.CmdSubst); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// fileHasArithmeticContext reports whether a parsed shell file contains any
// arithmetic-evaluation context: $(( )), (( )), let, or a C-style for loop.
// These are the shell constructs that re-evaluate a variable's string value as
// fresh arithmetic, making a prior command substitution stored in a variable
// into a deferred arithmetic mutation.
//
// The check also covers numeric [[ ]] operators (-eq/-ne/-lt/-gt/-le/-ge) and
// arithmetic subscripts and slice offsets in parameter expansions and indexed
// assignments, all of which trigger the same re-evaluation.
//
// Additionally, a CallExpr whose effective command name (after stripping the
// `command` and `builtin` wrappers) is `let` is treated as an arithmetic
// context. bash's `command` and `builtin` builtins execute `let` in the
// current shell, so `command let x` carries the same re-evaluation hazard as
// bare `let x`; the wrapped form is not represented by a LetClause AST node
// and would otherwise escape this check.
func fileHasArithmeticContext(file syntax.Node) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.ArithmExp, *syntax.ArithmCmd, *syntax.LetClause, *syntax.CStyleLoop:
			found = true
			return false
		case *syntax.BinaryTest:
			switch n.Op {
			case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
				found = true
				return false
			}
		case *syntax.ParamExp:
			if n.Index != nil || n.Slice != nil {
				found = true
				return false
			}
		case *syntax.Assign:
			if n.Index != nil {
				found = true
				return false
			}
		case *syntax.CallExpr:
			// Recognize `command let …` and `builtin let …` as arithmetic
			// contexts: bash's command/builtin wrappers execute let in the
			// current shell, so the wrapped form carries the same re-evaluation
			// hazard as a bare let, but is represented as a CallExpr rather
			// than a LetClause.
			if isWrappedLetCall(n) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// isWrappedLetCall reports whether a CallExpr resolves to a `let` invocation
// after stripping any leading `command` and `builtin` wrappers. These wrappers
// run the inner command in the current shell environment, so `command let x`
// and `builtin let x` evaluate x as arithmetic exactly like bare `let x`.
func isWrappedLetCall(call *syntax.CallExpr) bool {
	words := call.Args
	for len(words) > 0 {
		name, literal := literalShellWord(words[0])
		if !literal {
			return false
		}
		switch name {
		case "command", "builtin":
			words = words[1:]
		// Skip any options (e.g. `command -p let`), consuming `--` when present.
		for len(words) > 0 {
			opt, ok := literalShellWord(words[0])
			if !ok || !strings.HasPrefix(opt, "-") {
				break
			}
			words = words[1:]
			if opt == "--" {
				break
			}
		}
		case "let":
			return true
		default:
			return false
		}
	}
	return false
}

// fileHasLiteralDeniedArithAssignment reports whether a parsed shell file
// contains any literal string value that, when evaluated by bash as arithmetic,
// would perform an assignment to a denied account-environment variable.
//
// This is the counterpart to fileHasCmdSubst for the literal-assignment bypass:
// `x='CODEX_HOME=1'; : $((x)); codex` stores a literal arithmetic-assignment
// string in x, and bash re-evaluates it as fresh arithmetic when x appears
// inside an arithmetic context. No command substitution is involved, so
// fileHasCmdSubst does not fire; this predicate detects the hazardous literal.
//
// Combined with fileHasArithmeticContext, this forms the second coarse rule:
// if any literal in the command is a denied arithmetic assignment AND the
// command contains any arithmetic context, refuse.
//
// Both *syntax.Lit (unquoted or double-quoted text) and *syntax.SglQuoted
// (single-quoted strings) can hold the hazardous literal, so both are checked.
// Additionally, complete *syntax.Word nodes are checked: the shell concatenates
// adjacent literal word parts at parse time (e.g. CODEX_HOME"=1" becomes the
// single string "CODEX_HOME=1"), so the per-fragment checks on Lit and
// SglQuoted alone are insufficient — only the fully-assembled word catches
// split literals.
func fileHasLiteralDeniedArithAssignment(file syntax.Node, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.Word:
			// Check the fully concatenated word value: CODEX_HOME"=1" has two
			// AST fragments ("CODEX_HOME" and "=1") that are individually
			// harmless but spell "CODEX_HOME=1" when joined. literalShellWord
			// performs the same concatenation the shell does; if it returns
			// false the word has a non-literal part and the individual-fragment
			// arms below still run during the walk's descent.
			if value, ok := literalShellWord(n); ok {
				if literalContainsDeniedArithAssignment(value, names) {
					found = true
					return false
				}
			}
		case *syntax.Lit:
			if literalContainsDeniedArithAssignment(n.Value, names) {
				found = true
				return false
			}
		case *syntax.SglQuoted:
			if literalContainsDeniedArithAssignment(n.Value, names) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
