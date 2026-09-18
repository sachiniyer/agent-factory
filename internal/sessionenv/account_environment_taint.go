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

// arithmExprIsNumericConstant reports whether an arithmetic expression is a
// provably numeric constant — that is, it contains only literal integer digits,
// arithmetic operators, and parentheses. No variable references, no parameter
// expansions, and no command substitutions are present. An expression that
// satisfies this predicate cannot perform a re-evaluation of a stored string as
// arithmetic, because it never reads a variable whose value could have been set
// to a hazardous string.
//
// Examples of numeric constants: `0`, `42`, `1+2`, `(3*4)`, `-1`.
// Examples of non-constants: `x` (variable reference), `$x`, `${x}`, `$(cmd)`.
func arithmExprIsNumericConstant(expr syntax.ArithmExpr) bool {
	constant := true
	syntax.Walk(expr, func(node syntax.Node) bool {
		if !constant {
			return false
		}
		if node == nil {
			// End-of-children sentinel; nothing to check.
			return true
		}
		switch n := node.(type) {
		case *syntax.BinaryArithm, *syntax.UnaryArithm, *syntax.ParenArithm,
			*syntax.FlagsArithm:
			// Structural nodes: continue walking into children.
		case *syntax.Word:
			// A word in arithmetic context is a variable reference unless it
			// consists entirely of digit characters. Check every part: if any
			// part is not a Lit, or the Lit contains non-digit characters, the
			// word is not a constant. Return false to stop descending into the
			// word's Lit parts (they are already checked above).
			for _, part := range n.Parts {
				lit, ok := part.(*syntax.Lit)
				if !ok {
					constant = false
					return false
				}
				for _, ch := range lit.Value {
					if ch < '0' || ch > '9' {
						constant = false
						return false
					}
				}
			}
			// All parts are numeric Lit nodes; do not descend further.
			return false
		default:
			_ = n
			// Any other node (ParamExp, CmdSubst, etc.) inside an arithmetic
			// expression is not a numeric constant.
			constant = false
			return false
		}
		return true
	})
	return constant
}

// wordIsNumericLiteralForArith reports whether a shell word, when used as an
// arithmetic operand (e.g. in `[[ X -eq Y ]]`), is a provably numeric literal.
// A provably numeric literal is a word whose only part is a Lit consisting
// entirely of decimal digit characters, optionally preceded by a minus sign.
func wordIsNumericLiteralForArith(word *syntax.Word) bool {
	if len(word.Parts) == 0 {
		return false
	}
	// Allow a leading '-' part if it's a Lit.
	parts := word.Parts
	if lit, ok := parts[0].(*syntax.Lit); ok && lit.Value == "-" && len(parts) > 1 {
		parts = parts[1:]
	}
	if len(parts) != 1 {
		return false
	}
	lit, ok := parts[0].(*syntax.Lit)
	if !ok || len(lit.Value) == 0 {
		return false
	}
	for _, ch := range lit.Value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// fileHasArithmeticContextWithVariableOperand reports whether a parsed shell
// file contains any arithmetic-evaluation context whose operand is not provably
// a numeric constant. Rather than listing which constructs ARE arithmetic
// contexts and pairing with separate predicates that enumerate the ways a
// variable's value can become hazardous, it directly asks whether any arithmetic
// context can re-evaluate a runtime string.
//
// An arithmetic context's operand is "provably numeric" only when it consists
// entirely of integer literals, arithmetic operators, and parentheses — with no
// variable references, parameter expansions, or command substitutions. This
// predicate returns true whenever any arithmetic context has an operand that
// does not satisfy that condition.
//
// This single predicate subsumes all three coarse rules:
//   - Rule 1 (CmdSubst+arith): a CmdSubst inside arithmetic is never a numeric
//     constant, so it is caught here.
//   - Rule 2 (literal-arith-assignment+arith): a variable holding a literal
//     denied-assignment string appears in arithmetic as a variable reference
//     (not a constant), so it is caught here.
//   - Rule 3 (runtime-input+arith): a variable assigned by read/mapfile appears
//     in arithmetic as a variable reference, so it is caught here.
//   - The dynamic-composition bypass (n=CODEX_HOME; x="${n}=1"; : $((x))): x is
//     a variable reference in arithmetic, not a numeric constant — caught here.
//
// The predicate is enumeration-free in the sense that matters: it does not
// enumerate the ways a value can become hazardous (CmdSubst, literal, read, …),
// only the ways an arithmetic operand can be provably constant (integer digits,
// operators). The "safe" list is controlled by this code and does not silently
// grow when bash or mvdan.cc/sh adds a new form of string-building.
func fileHasArithmeticContextWithVariableOperand(file syntax.Node) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if found {
			return false
		}
		switch n := node.(type) {
		case *syntax.ArithmExp:
			// $(( expr )) — the expression must be a numeric constant.
			if !arithmExprIsNumericConstant(n.X) {
				found = true
				return false
			}
		case *syntax.ArithmCmd:
			// (( expr )) — same.
			if !arithmExprIsNumericConstant(n.X) {
				found = true
				return false
			}
		case *syntax.LetClause:
			// let expr … — each expression must be a numeric constant.
			for _, expr := range n.Exprs {
				if !arithmExprIsNumericConstant(expr) {
					found = true
					return false
				}
			}
		case *syntax.CStyleLoop:
			// for (( init; cond; post )) — any non-constant expression is unsafe.
			if n.Init != nil && !arithmExprIsNumericConstant(n.Init) {
				found = true
				return false
			}
			if n.Cond != nil && !arithmExprIsNumericConstant(n.Cond) {
				found = true
				return false
			}
			if n.Post != nil && !arithmExprIsNumericConstant(n.Post) {
				found = true
				return false
			}
		case *syntax.BinaryTest:
			// Numeric [[ ]] operators evaluate operands as arithmetic.
			switch n.Op {
			case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
				// Operands are TestExpr (can be *Word or another test node).
				// A numeric literal must be a plain *Word with digit-only Lit parts.
				xWord, xOk := n.X.(*syntax.Word)
				yWord, yOk := n.Y.(*syntax.Word)
				if !xOk || !yOk || !wordIsNumericLiteralForArith(xWord) || !wordIsNumericLiteralForArith(yWord) {
					found = true
					return false
				}
			}
		case *syntax.ParamExp:
			// ${arr[i]} and ${x:offset:length} evaluate subscripts/slices as arithmetic.
			if n.Index != nil && !arithmExprIsNumericConstant(n.Index) {
				found = true
				return false
			}
			if n.Slice != nil {
				if n.Slice.Offset != nil && !arithmExprIsNumericConstant(n.Slice.Offset) {
					found = true
					return false
				}
				if n.Slice.Length != nil && !arithmExprIsNumericConstant(n.Slice.Length) {
					found = true
					return false
				}
			}
		case *syntax.Assign:
			// arr[i]=val evaluates the subscript as arithmetic.
			if n.Index != nil && !arithmExprIsNumericConstant(n.Index) {
				found = true
				return false
			}
		case *syntax.CallExpr:
			// `command let …` and `builtin let …` execute let in the current shell.
			if isWrappedLetCall(n) {
				// Treat the args after stripping wrappers as let expressions.
				// If any wrapped-let invocation exists, it is non-constant by
				// definition (the args are shell words, not arithmetic ASTs).
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
