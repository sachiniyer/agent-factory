package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func arrayReadMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for len(words) > 0 {
		value, literal := literalShellWord(words[0])
		if !literal {
			return true
		}
		words = words[1:]
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-t":
				continue
			case "-d":
				if len(words) == 0 {
					return true
				}
				if _, literal := literalShellWord(words[0]); !literal {
					return true
				}
				words = words[1:]
				continue
			}
			if strings.HasPrefix(value, "-") {
				// The remaining options accept arithmetic expressions or callbacks.
				// Either can assign an identity indirectly, so unsupported option
				// forms fail closed.
				return true
			}
		}
		if len(words) != 0 {
			return true
		}
		return accountEnvironmentOperandDenied(value, names)
	}
	return false
}

// stmtsMutate processes a slice of statements in execution order, propagating
// taint incrementally through acc. Returns true if any statement mutates the
// account environment.
func stmtsMutate(stmts []*syntax.Stmt, names map[string]struct{}, acc *taintAccumulator) bool {
	for _, stmt := range stmts {
		if stmtMutates(stmt, names, acc) {
			return true
		}
	}
	return false
}

// stmtMutates checks a single statement for account-environment mutations,
// recursing into compound constructs that execute their children sequentially
// in the current shell so that taint from an earlier child is visible when
// checking a later one.
//
// Constructs whose children run in a subshell (Subshell, pipes) do not
// propagate taint outward and fall through to the default walk.
func stmtMutates(stmt *syntax.Stmt, names map[string]struct{}, acc *taintAccumulator) bool {
	switch cmd := stmt.Cmd.(type) {
	case *syntax.Block:
		// { stmts } — all children execute in the current shell sequentially.
		return stmtsMutate(cmd.Stmts, names, acc)
	case *syntax.BinaryCmd:
		switch cmd.Op {
		case syntax.AndStmt, syntax.OrStmt:
			// X && Y or X || Y: X always executes; Y executes conditionally.
			// Taint from X is always visible when checking Y (X always runs
			// first), so we propagate X's taint to acc before checking Y.
			//
			// Y's effect on the accumulator is conservative: since Y may or
			// may not execute, we compute the taint changes from Y in a
			// temporary accumulator that starts with X's state. After Y is
			// checked, we UNION Y's resulting taint with the current
			// accumulator — variables tainted in Y but not in pre-Y state are
			// added (Y might have tainted them), but variables that Y cleared
			// are NOT cleared (Y might not have run, so the parent-shell taint
			// may still be present).
			if stmtMutates(cmd.X, names, acc) {
				return true
			}
			// Fork a temporary accumulator for Y at X's post-taint state.
			yAcc := newTaintAccumulator(names)
			for k := range acc.Tainted() {
				yAcc.tainted[k] = struct{}{}
			}
			// Copy existing allAssigns so transitive propagation works in yAcc.
			yAcc.allAssigns = append(yAcc.allAssigns, acc.allAssigns...)
			if stmtMutates(cmd.Y, names, yAcc) {
				return true
			}
			// Union: add any variable that Y tainted that was not tainted
			// before Y. Do NOT remove variables from acc that Y cleared — Y
			// may not have run.
			for k := range yAcc.Tainted() {
				acc.tainted[k] = struct{}{}
			}
			return false
		}
	case *syntax.IfClause:
		// if/then/elif/else — branches execute sequentially in the current
		// shell and may reference or assign variables visible to later
		// statements. We use a conservative union: any taint acquired in ANY
		// branch is committed to the accumulator, but taint cleared in only
		// one branch is retained (the other branch may not run).
		if cmd == nil {
			break
		}
		// unionAcc collects the union of taint from all branches.
		unionAcc := newTaintAccumulator(names)
		for k := range acc.Tainted() {
			unionAcc.tainted[k] = struct{}{}
		}
		// Walk the chain: if → elif → else. Each IfClause node has a .Then
		// body and a .Else pointing to the next elif/else IfClause.
		clause := cmd
		for clause != nil {
			branchAcc := newTaintAccumulator(names)
			for k := range acc.Tainted() {
				branchAcc.tainted[k] = struct{}{}
			}
			branchAcc.allAssigns = append(branchAcc.allAssigns, acc.allAssigns...)
			if stmtsMutate(clause.Then, names, branchAcc) {
				return true
			}
			// Union this branch's resulting taint.
			for k := range branchAcc.Tainted() {
				unionAcc.tainted[k] = struct{}{}
			}
			clause = clause.Else
		}
		// Commit the union back to the parent accumulator.
		for k := range unionAcc.Tainted() {
			acc.tainted[k] = struct{}{}
		}
		return false
	}
	// Default: walk the statement with the current taint snapshot extended by
	// any within-statement assignments, then record any new taint assignments
	// so subsequent statements see them.
	//
	// Within a simple command, bash evaluates words left-to-right. A
	// ParamExp assignment expansion (`${x:=cmd}`) may store a CmdSubst
	// value in `x` before a later `$((x))` in the same word list is reached.
	// To detect this, we pre-scan the statement for ParamExp CmdSubst
	// assignments and include those as additional transient taint when
	// checking the arithmetic nodes.
	transientTaint := make(map[string]struct{})
	for k, v := range acc.Tainted() {
		transientTaint[k] = v
	}
	// Pre-scan: collect variables assigned from CmdSubst via ${x:=...} within
	// this statement (not inside a subshell).
	withinStmtAcc := newTaintAccumulator(names)
	for k := range acc.Tainted() {
		withinStmtAcc.tainted[k] = struct{}{}
	}
	withinStmtAcc.allAssigns = append(withinStmtAcc.allAssigns, acc.allAssigns...)
	withinStmtAcc.AddStmt(stmt)
	for k := range withinStmtAcc.Tainted() {
		transientTaint[k] = struct{}{}
	}

	mutates := false
	syntax.Walk(stmt, func(node syntax.Node) bool {
		if nodeMutatesAccountEnvironment(node, names, transientTaint) {
			mutates = true
			return false
		}
		return true
	})
	if !mutates {
		acc.AddStmt(stmt)
	}
	return mutates
}
