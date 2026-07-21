package tmuxguard

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

var errUnsupportedShell = errors.New("unsupported shell construct")

type shellWord struct {
	literal  string
	resolved bool
}

type shellAssignment struct {
	name   string
	simple bool
}

type shellDeclaration struct {
	variant     string
	assignments []shellAssignment
}

type shellCommand struct {
	words               []shellWord
	assignments         []shellAssignment
	declaration         *shellDeclaration
	hasHeredoc          bool
	cwdChangePersists   bool
	environmentAssigned bool
	directoryChanged    bool
}

// parseShellCommands parses Bash syntax with a maintained parser, but resolves
// only literal words. Dynamic words remain explicitly tainted so callers can
// allow them in data positions while refusing them in execution-sensitive ones.
func parseShellCommands(command string) ([]shellCommand, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnsupportedShell, err)
	}

	heredocCalls := make(map[*syntax.CallExpr]bool)
	var commands []shellCommand
	var walkErr error
	var scopeStack []bool
	nonPersistentDepth := 0
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil {
			last := len(scopeStack) - 1
			if last >= 0 {
				if scopeStack[last] {
					nonPersistentDepth--
				}
				scopeStack = scopeStack[:last]
			}
			return walkErr == nil
		}
		if walkErr != nil {
			return false
		}
		nonPersistent := startsNonPersistentShellScope(node)
		scopeStack = append(scopeStack, nonPersistent)
		if nonPersistent {
			nonPersistentDepth++
		}
		switch node := node.(type) {
		case *syntax.Stmt:
			for _, redirect := range node.Redirs {
				if !isHeredoc(redirect.Op) {
					continue
				}
				call, ok := node.Cmd.(*syntax.CallExpr)
				if !ok {
					walkErr = errUnsupportedShell
					return false
				}
				heredocCalls[call] = true
			}
		case *syntax.CallExpr:
			words := make([]shellWord, 0, len(node.Args))
			for _, word := range node.Args {
				words = append(words, resolveWord(word))
			}
			if len(words) > 0 || len(node.Assigns) > 0 {
				commands = append(commands, shellCommand{
					words:             words,
					assignments:       describeAssignments(node.Assigns),
					hasHeredoc:        heredocCalls[node],
					cwdChangePersists: nonPersistentDepth == 0,
				})
			}
		case *syntax.DeclClause:
			commands = append(commands, shellCommand{declaration: &shellDeclaration{
				variant:     node.Variant.Value,
				assignments: describeAssignments(node.Args),
			}})
		case *syntax.ArithmCmd, *syntax.ArithmExp, *syntax.CStyleLoop, *syntax.FuncDecl,
			*syntax.LetClause:
			walkErr = errUnsupportedShell
		case *syntax.ParamExp:
			if unsafeParamExpansion(node) {
				walkErr = errUnsupportedShell
			}
		case *syntax.UnaryTest:
			if node.Op == syntax.TsVarSet || node.Op == syntax.TsRefVar {
				walkErr = errUnsupportedShell
			}
		case *syntax.BinaryTest:
			if arithmeticTestOperator(node.Op) {
				walkErr = errUnsupportedShell
			}
		}
		return walkErr == nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return commands, nil
}

func startsNonPersistentShellScope(node syntax.Node) bool {
	switch node := node.(type) {
	case *syntax.Subshell, *syntax.CmdSubst:
		return true
	case *syntax.BinaryCmd:
		return node.Op == syntax.Pipe || node.Op == syntax.PipeAll
	case *syntax.Stmt:
		return node.Background || node.Coprocess || node.Disown
	default:
		return false
	}
}

func unsafeParamExpansion(expansion *syntax.ParamExp) bool {
	return expansion.Excl || expansion.Index != nil || expansion.Slice != nil ||
		(expansion.Exp != nil && expansion.Exp.Op == syntax.OtherParamOps)
}

func arithmeticTestOperator(operator syntax.BinTestOperator) bool {
	switch operator {
	case syntax.TsEql, syntax.TsNeq, syntax.TsLeq, syntax.TsGeq, syntax.TsLss, syntax.TsGtr:
		return true
	default:
		return false
	}
}

func describeAssignments(assignments []*syntax.Assign) []shellAssignment {
	described := make([]shellAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		name := ""
		if assignment.Name != nil {
			name = assignment.Name.Value
		}
		described = append(described, shellAssignment{
			name:   name,
			simple: name != "" && assignment.Index == nil && assignment.Array == nil && !assignment.Append,
		})
	}
	return described
}

func resolveWord(word *syntax.Word) shellWord {
	for _, part := range word.Parts {
		if !literalPartIsStatic(part, false) {
			return shellWord{}
		}
	}
	fields, err := expand.Fields(&expand.Config{}, word)
	if err != nil || len(fields) != 1 {
		return shellWord{}
	}
	return shellWord{literal: fields[0], resolved: true}
}

func literalPartIsStatic(part syntax.WordPart, quoted bool) bool {
	switch part := part.(type) {
	case *syntax.Lit:
		return quoted || !hasUnescapedExpansionMeta(part.Value)
	case *syntax.SglQuoted:
		return true // mvdan's expander resolves both ordinary and ANSI-C quotes.
	case *syntax.DblQuoted:
		if part.Dollar {
			return false
		}
		for _, nested := range part.Parts {
			if !literalPartIsStatic(nested, true) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func hasUnescapedExpansionMeta(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			i++
			continue
		}
		switch value[i] {
		case '*', '?':
			return true
		case '[':
			if hasUnescapedByte(value[i+1:], ']') {
				return true
			}
		case '{':
			if end := strings.IndexByte(value[i+1:], '}'); end >= 0 {
				inside := value[i+1 : i+1+end]
				if strings.Contains(inside, ",") || strings.Contains(inside, "..") {
					return true
				}
			}
		case '~':
			if i == 0 {
				return true
			}
		}
	}
	return false
}

func hasUnescapedByte(value string, target byte) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			i++
			continue
		}
		if value[i] == target {
			return true
		}
	}
	return false
}

func isHeredoc(operator syntax.RedirOperator) bool {
	return operator == syntax.Hdoc || operator == syntax.DashHdoc || operator == syntax.WordHdoc
}
