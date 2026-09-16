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

// taintAccumulator tracks the set of "tainted" variable names across a
// sequence of shell statements, processing them in execution order. A variable
// is tainted when its value originates from a command substitution at top-level
// (not inside a subshell), or when its literal value is an arithmetic
// assignment expression to a denied name: bash re-evaluates such values as
// fresh arithmetic when the variable appears inside an arithmetic context, so
// the substitution's output (or the literal assignment string) becomes a
// deferred arithmetic mutation.
//
// Taint propagates transitively through parameter-expansion copies: when `y=$x`
// and `x` is tainted, bash stores x's contents in y, so `: $((y))` carries the
// same re-evaluation hazard as `: $((x))`. The fixed-point propagation covers
// chains of arbitrary length.
//
// A definite unconditional reassignment to a provably clean value removes the
// variable from taint. This ensures that `x=$(cmd); x=0; : $((x))` is allowed:
// the `x=0` overwrites the tainted value with a literal zero, so the later
// arithmetic expression is safe.
//
// Processing statements individually rather than whole-file ensures that an
// assignment that appears AFTER an arithmetic expression does not cause that
// earlier expression to be refused: the taint contributed by a statement is
// only visible to SUBSEQUENT statements.
type taintAccumulator struct {
	// tainted is the set of variable names currently known to be tainted.
	tainted map[string]struct{}
	// allAssigns accumulates the current-reaching non-CmdSubst assign for each
	// variable name, for use in the fixed-point propagation step. A later
	// assignment supersedes all prior ones for the same name.
	allAssigns []taintAssignRecord
	// names is the set of denied account-environment variable names, used to
	// detect literal values that are arithmetic assignments to denied names.
	names map[string]struct{}
}

// taintAssignRecord holds a non-CmdSubst assignment target and its RHS word,
// which may transitively reference a tainted variable.
type taintAssignRecord struct {
	name  string
	value *syntax.Word
}

// newTaintAccumulator creates an empty taintAccumulator. names is the set of
// denied account-environment variable names.
func newTaintAccumulator(names map[string]struct{}) *taintAccumulator {
	return &taintAccumulator{tainted: make(map[string]struct{}), names: names}
}

// Tainted returns the current set of tainted variable names. The returned map
// must not be modified by the caller.
func (t *taintAccumulator) Tainted() map[string]struct{} {
	return t.tainted
}

// AddStmt scans node for assignments and updates the tainted set. Call this
// AFTER checking node for mutations so that an assignment in this statement
// does not taint variables used in this same statement's arithmetic
// expressions; the taint only applies to subsequent statements.
//
// Assignments inside a subshell or command substitution are skipped because
// they run in a child process and cannot affect the parent's environment.
//
// A non-CmdSubst assignment to a currently-tainted variable removes it from
// taint when the new value is provably clean, implementing reaching-assignment
// semantics: `x=$(cmd); x=0; : $((x))` is allowed because `x=0` overwrites
// the tainted value before the arithmetic.
//
// Only an unindexed, non-append assignment that fully replaces the variable's
// value is allowed to clear taint. An append (`x+=0`) only prepends/appends
// to the existing value, so `x=$(printf CODEX_HOME=1); x+=0` still leaves `x`
// tainted. An indexed assignment (`x[1]=0`) does not overwrite `x[0]`.
//
// Command-local (prefix) assignments — `Assign` nodes attached to a CallExpr
// that have a non-nil command (Assigns on CallExpr.Assigns) — only affect the
// child process's environment, not the parent's. These are distinguished by
// their position inside a CallExpr; since AddStmt walks raw syntax nodes the
// caller is responsible for not feeding prefix assignments through this path.
// The walk skips them by not entering CallExpr bodies.
func (t *taintAccumulator) AddStmt(node syntax.Node) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.Subshell, *syntax.CmdSubst:
			return false
		case *syntax.CallExpr:
			// A CallExpr WITH Args has a command: its Assigns list contains
			// prefix assignments that only affect the child process's
			// environment, not the parent shell. We must NOT process
			// n.Assigns as persistent taint changes.
			//
			// However, we DO need to walk the Args words because they may
			// contain ParamExp assignment expansions (e.g. `${x:=...}`) that
			// persist in the parent shell. We handle this by manually
			// processing only the args (not n.Assigns) and then tracking
			// certain builtins (printf -v) that write to parent-shell vars.
			if len(n.Args) > 0 {
				// Track printf -v VAR taint.
				if len(n.Args) >= 3 {
					name, isName := literalShellWord(n.Args[0])
					if isName && name == "printf" {
						t.trackPrintfV(n.Args[1:])
					}
				}
				// Walk the Args words to find ParamExp assignments like
				// ${x:=...} that persist in the current shell.
				// We use a nested walk that SKIPS any sub-CallExpr nodes
				// to avoid double-processing.
				t.walkWordsForTaint(n.Args)
				return false
			}
			// No Args: standalone assignments — fall through to walk the
			// Assign nodes in n.Assigns normally.
		case *syntax.Assign:
			// Only a simple, non-append, non-indexed assignment to a named
			// variable can clear taint. Append (`x+=0`) and indexed (`x[1]=0`)
			// assignments do not replace the entire value.
			if n.Name != nil && n.Value != nil && n.Index == nil && !n.Append {
				if wordHasCommandSubstitution(n.Value) {
					// Command substitution: taint directly and remove any
					// prior clean-assignment record for this name.
					t.tainted[n.Name.Value] = struct{}{}
					t.removeAllAssigns(n.Name.Value)
				} else {
					// Definite reassignment: supersede any prior record.
					t.removeAllAssigns(n.Name.Value)
					// A literal value that is an arithmetic assignment
					// expression to a denied name is itself a hazard: bash
					// evaluates the variable's contents as fresh arithmetic,
					// so `x='CODEX_HOME=1'; : $((x))` assigns CODEX_HOME.
					val, isLiteral := literalShellWord(n.Value)
					if isLiteral && literalContainsDeniedArithAssignment(val, t.names) {
						t.tainted[n.Name.Value] = struct{}{}
					} else {
						// Clear stale taint from a prior CmdSubst assignment
						// now superseded by this definite assignment.
						delete(t.tainted, n.Name.Value)
						t.allAssigns = append(t.allAssigns, taintAssignRecord{n.Name.Value, n.Value})
					}
				}
			} else if n.Name != nil && n.Value != nil && wordHasCommandSubstitution(n.Value) {
				// Append or indexed CmdSubst: taint the variable without
				// clearing prior taint records (they remain relevant).
				t.tainted[n.Name.Value] = struct{}{}
			}
		case *syntax.ParamExp:
			// ${x:=...} / ${x=...} assigns to x when x is unset (or null).
			if n.Param != nil && n.Exp != nil &&
				(n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) &&
				n.Exp.Word != nil {
				if wordHasCommandSubstitution(n.Exp.Word) {
					t.tainted[n.Param.Value] = struct{}{}
					t.removeAllAssigns(n.Param.Value)
				} else {
					t.allAssigns = append(t.allAssigns, taintAssignRecord{n.Param.Value, n.Exp.Word})
				}
			}
		case *syntax.WordIter:
			// A `for x in item1 item2; do ...` loop assigns each item to the
			// iteration variable in the current shell. If any item in the list
			// is a literal that looks like an arithmetic assignment to a denied
			// name, taint the variable — bash re-evaluates it as arithmetic
			// when the variable appears inside $(( )), (( )), or let.
			if n.Name != nil {
				for _, item := range n.Items {
					if wordHasCommandSubstitution(item) {
						// A CmdSubst item: taint directly.
						t.tainted[n.Name.Value] = struct{}{}
						t.removeAllAssigns(n.Name.Value)
						break
					}
					val, isLiteral := literalShellWord(item)
					if !isLiteral {
						// A non-literal item is dynamically composed and may
						// contain a denied arithmetic assignment.
						t.tainted[n.Name.Value] = struct{}{}
						t.removeAllAssigns(n.Name.Value)
						break
					}
					if literalContainsDeniedArithAssignment(val, t.names) {
						t.tainted[n.Name.Value] = struct{}{}
						t.removeAllAssigns(n.Name.Value)
						break
					}
				}
			}
		}
		return true
	})
	t.propagate()
}

// walkWordsForTaint walks a list of words for ParamExp assignment expansions
// (${x:=...} / ${x=...}) that persist in the current shell and updates the
// taint accumulator accordingly. Called when processing the Args of a CallExpr
// that has a command (so its own Assigns are prefix-only and skipped).
func (t *taintAccumulator) walkWordsForTaint(words []*syntax.Word) {
	for _, word := range words {
		syntax.Walk(word, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.CmdSubst, *syntax.Subshell:
				return false
			case *syntax.ParamExp:
				if n.Param != nil && n.Exp != nil &&
					(n.Exp.Op == syntax.AssignUnset || n.Exp.Op == syntax.AssignUnsetOrNull) &&
					n.Exp.Word != nil {
					if wordHasCommandSubstitution(n.Exp.Word) {
						t.tainted[n.Param.Value] = struct{}{}
						t.removeAllAssigns(n.Param.Value)
					} else {
						t.allAssigns = append(t.allAssigns, taintAssignRecord{n.Param.Value, n.Exp.Word})
					}
				}
			}
			return true
		})
	}
}

// trackPrintfV checks whether a `printf -v VAR ...` call has a CmdSubst in
// its arguments (after the -v flag), and if so marks VAR as tainted.
// words is everything AFTER the `printf` command name.
func (t *taintAccumulator) trackPrintfV(words []*syntax.Word) {
	if len(words) < 2 {
		return
	}
	opt, literal := literalShellWord(words[0])
	if !literal {
		return
	}
	var varName string
	switch {
	case opt == "-v":
		// -v VAR: next word is the variable name.
		if len(words) < 3 {
			return
		}
		v, ok := literalShellWord(words[1])
		if !ok {
			return
		}
		varName = v
		words = words[2:]
	case strings.HasPrefix(opt, "-v") && len(opt) > 2:
		// -vVAR (attached).
		varName = opt[2:]
		words = words[1:]
	default:
		return
	}
	if varName == "" {
		return
	}
	// If any remaining word (format or args) contains a command substitution,
	// the value written to varName may be derived from CmdSubst output.
	for _, w := range words {
		if wordHasCommandSubstitution(w) {
			t.tainted[varName] = struct{}{}
			t.removeAllAssigns(varName)
			return
		}
	}
}

// removeAllAssigns removes all allAssigns records for the given variable name.
// Used when a new assignment supersedes all prior ones.
func (t *taintAccumulator) removeAllAssigns(name string) {
	out := t.allAssigns[:0]
	for _, rec := range t.allAssigns {
		if rec.name != name {
			out = append(out, rec)
		}
	}
	t.allAssigns = out
}

// propagate runs a fixed-point pass over all accumulated non-CmdSubst assigns,
// marking any whose RHS references an already-tainted variable as tainted.
// Repeated until stable to handle transitive chains (x→y→z).
//
// Additionally, any RHS value that mixes dynamic expansions with literal `=`
// text is treated as potentially composing a denied arithmetic assignment:
// `x="${n}=1"` expands to `CODEX_HOME=1` when n=CODEX_HOME, even when `n`
// is not itself tainted. A word is considered compositionally hazardous when
// it contains at least one parameter expansion (or other non-literal part) AND
// at least one literal part containing `=`.
func (t *taintAccumulator) propagate() {
	// First pass: taint variables assigned from compositionally hazardous
	// RHS values — words that combine dynamic expansions with literal `=`.
	for _, rec := range t.allAssigns {
		if _, already := t.tainted[rec.name]; already {
			continue
		}
		if wordIsCompositionallyHazardous(rec.value) {
			t.tainted[rec.name] = struct{}{}
		}
	}
	// Fixed-point pass: propagate taint through chains referencing tainted vars.
	for {
		added := false
		for _, rec := range t.allAssigns {
			if _, already := t.tainted[rec.name]; already {
				continue
			}
			if wordReferencesTaintedVar(rec.value, t.tainted) {
				t.tainted[rec.name] = struct{}{}
				added = true
			}
		}
		if !added {
			break
		}
	}
}

// wordIsCompositionallyHazardous reports whether a word mixes dynamic parts
// (parameter expansions, arithmetic expansions, etc.) with a literal segment
// that contains `=`. Such a word could expand to `NAME=value` even when no
// single constituent is tainted, enabling a denied arithmetic assignment.
//
// Example: `"${n}=1"` has parts [ParamExp(n), Lit("=1")]. When n=CODEX_HOME,
// the word expands to `CODEX_HOME=1`.
func wordIsCompositionallyHazardous(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	hasDynamic := false
	hasLiteralEquals := false
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsRune(p.Value, '=') {
				hasLiteralEquals = true
			}
		default:
			// Any non-literal part (ParamExp, ArithmExp, CmdSubst, etc.)
			// is a dynamic component.
			hasDynamic = true
		}
	}
	return hasDynamic && hasLiteralEquals
}

// literalContainsDeniedArithAssignment reports whether a literal string, when
// evaluated by bash as an arithmetic expression, would perform an assignment to
// a denied account-environment variable. bash evaluates the entire string as
// arithmetic, so compound expressions like `0,CODEX_HOME=1` (comma operator)
// and compound assignments like `CODEX_HOME+=1` are also detected.
//
// The check parses the string as a bash arithmetic expression and walks the
// resulting AST for assignment nodes whose target is a denied name. Any
// expression that cannot be parsed is treated as potentially hazardous and
// causes the check to return true.
func literalContainsDeniedArithAssignment(value string, names map[string]struct{}) bool {
	if len(names) == 0 {
		return false
	}
	// Parse the literal as an arithmetic expression to catch compound forms
	// like `0,CODEX_HOME=1`, `CODEX_HOME+=1`, etc. If parsing fails the
	// expression is unprovable; treat it as hazardous.
	parsed, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Arithmetic(strings.NewReader(value))
	if err != nil {
		// Not valid arithmetic — not hazardous as an arithmetic assignment.
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
				name, ok := arithmeticAccountEnvironmentName(n.X)
				if ok && accountEnvironmentNameDenied(name, names) {
					found = true
					return false
				}
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				name, ok := arithmeticAccountEnvironmentName(n.X)
				if ok && accountEnvironmentNameDenied(name, names) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// cmdSubstAssignedVars returns the set of variable names that are assigned
// (at the top-level, not inside a subshell) from command substitutions or
// hazardous literals in the given file. These variables are "tainted": bash
// re-evaluates their value as fresh arithmetic when they appear inside an
// arithmetic context (`$(( ))`, `(( ))`, `let`, numeric `[[ ]]`), so a prior
// `x=$(printf CODEX_HOME=1)` followed by `: $((x))` carries the same bypass
// as an inline substitution.
//
// This whole-file variant is kept for callers that do not need statement-level
// ordering. Prefer taintAccumulator when ordering matters.
func cmdSubstAssignedVars(file syntax.Node, names map[string]struct{}) map[string]struct{} {
	acc := newTaintAccumulator(names)
	acc.AddStmt(file)
	return acc.Tainted()
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
