package sessionenv

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
	"mvdan.cc/sh/v3/syntax"
)

const maxNestedProgramDepth = 1

// commandEnvironmentFlagState recognizes a selector assignment on the command
// that launches agent. found reports that every possible agent invocation in
// the parsed command explicitly sets or unsets the selector, so that state can
// override an exported selector. It also follows af agent-server's literal
// --program argument, because SSH and Docker apply the first environment
// filter around that handoff. Nothing is expanded or executed.
func commandEnvironmentFlagState(command, agent, name string) (found, enabled bool) {
	scan := scanCommandEnvironmentFlag(command, agent, name, 0)
	if scan.enabled {
		return true, true
	}
	if scan.hasAgent && !scan.inherits {
		return true, false
	}
	return false, false
}

type commandFlagScan struct {
	hasAgent bool
	inherits bool
	enabled  bool
}

func scanCommandEnvironmentFlag(command, agent, name string, depth int) commandFlagScan {
	if depth > maxNestedProgramDepth || strings.TrimSpace(command) == "" || agent == "" {
		return commandFlagScan{}
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return commandFlagScan{}
	}
	var scan commandFlagScan
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		if args, literal := literalCommandArgs(call.Args); literal {
			for _, nested := range agentServerPrograms(args) {
				nestedScan := scanCommandEnvironmentFlag(nested, agent, name, depth+1)
				scan.hasAgent = scan.hasAgent || nestedScan.hasAgent
				scan.inherits = scan.inherits || nestedScan.inherits
				scan.enabled = scan.enabled || nestedScan.enabled
			}
		}

		agentIndex := literalAgentIndex(call.Args, agent)
		if agentIndex < 0 {
			return false
		}
		scan.hasAgent = true
		if found, envEnabled := envCommandFlagState(call.Args, agentIndex, name); found {
			scan.enabled = scan.enabled || envEnabled
			return false
		}
		if found, assignmentEnabled := callAssignmentFlagState(call.Assigns, name); found {
			scan.enabled = scan.enabled || assignmentEnabled
			return false
		}
		scan.inherits = true
		return false
	})
	return scan
}

func agentServerPrograms(args []string) []string {
	agentServer := -1
	for idx, arg := range args {
		if arg == "agent-server" {
			agentServer = idx
			break
		}
	}
	if agentServer < 0 {
		return nil
	}

	var programs []string
	for idx := agentServer + 1; idx < len(args); idx++ {
		switch {
		case args[idx] == "--program" && idx+1 < len(args):
			programs = append(programs, args[idx+1])
			idx++
		case strings.HasPrefix(args[idx], "--program="):
			programs = append(programs, strings.TrimPrefix(args[idx], "--program="))
		}
	}
	return programs
}

func literalAgentIndex(words []*syntax.Word, agent string) int {
	for idx, word := range words {
		arg, ok := literalShellWord(word)
		if ok && strings.EqualFold(filepath.Base(arg), agent) {
			return idx
		}
	}
	return -1
}

func assignmentFlagEnabled(assignment *syntax.Assign, name string) bool {
	if assignment == nil || assignment.Name == nil || assignment.Name.Value != name ||
		assignment.Append || assignment.Naked || assignment.Index != nil || assignment.Array != nil {
		return false
	}
	if assignment.Value == nil {
		return false
	}
	value, ok := literalShellWord(assignment.Value)
	return ok && flagValueEnabled(value)
}

func callAssignmentFlagState(assignments []*syntax.Assign, name string) (found, enabled bool) {
	// Shell assignment prefixes are applied left-to-right, so the last literal
	// assignment to the selector decides its value. A dynamic value is an
	// explicit but unprovable override, so it fails closed as disabled.
	for idx := len(assignments) - 1; idx >= 0; idx-- {
		assignment := assignments[idx]
		if assignment != nil && assignment.Name != nil && assignment.Name.Value == name {
			return true, assignmentFlagEnabled(assignment, name)
		}
	}
	return false, false
}

func envCommandFlagState(words []*syntax.Word, agentIndex int, name string) (found, enabled bool) {
	envIndex := -1
	for idx := 0; idx < agentIndex; idx++ {
		arg, ok := literalShellWord(words[idx])
		if ok && strings.EqualFold(filepath.Base(arg), "env") {
			envIndex = idx
			break
		}
	}
	if envIndex < 0 {
		return false, false
	}

	args, literal := literalCommandArgs(words[envIndex+1:])
	if literal {
		invocation, err := envcommand.Parse(args, envcommand.Policy{AllowAssignments: true})
		if err == nil && invocation.CommandIndex >= 0 {
			for _, mutation := range invocation.Mutations {
				if mutation.Name != name {
					continue
				}
				found = true
				enabled = !mutation.Unset && flagValueEnabled(mutation.Value)
			}
			return found, enabled
		}
	}

	// A dynamic value cannot be handed to envcommand.Parse, but an exact
	// selector name before the literal agent still proves that the inherited
	// selector is being replaced. Treat the unknown value as disabled.
	unsetNext := false
	for idx := envIndex + 1; idx < agentIndex; idx++ {
		arg, literal := literalShellWord(words[idx])
		if unsetNext {
			if literal && arg == name {
				found, enabled = true, false
			}
			unsetNext = false
			continue
		}
		if literal {
			switch {
			case arg == "-u" || arg == "--unset":
				unsetNext = true
				continue
			case strings.HasPrefix(arg, "--unset="):
				if strings.TrimPrefix(arg, "--unset=") == name {
					found, enabled = true, false
				}
				continue
			}
		}
		if assignmentFound, assignmentEnabled := shellWordAssignmentFlagState(words[idx], name); assignmentFound {
			found, enabled = true, assignmentEnabled
		}
	}
	return found, enabled
}

func shellWordAssignmentFlagState(word *syntax.Word, name string) (found, enabled bool) {
	if word == nil || len(word.Parts) == 0 {
		return false, false
	}
	literal, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return false, false
	}
	assignmentName, _, found := strings.Cut(literal.Value, "=")
	if !found || assignmentName != name {
		return false, false
	}
	value, literalValue := literalShellWord(word)
	if !literalValue {
		return true, false
	}
	_, value, _ = strings.Cut(value, "=")
	return true, flagValueEnabled(value)
}

func literalCommandArgs(words []*syntax.Word) ([]string, bool) {
	args := make([]string, len(words))
	for idx, word := range words {
		value, ok := literalShellWord(word)
		if !ok {
			return nil, false
		}
		args[idx] = value
	}
	return args, true
}

func literalShellWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", false
	}
	var value strings.Builder
	for _, part := range word.Parts {
		if !appendLiteralShellPart(&value, part) {
			return "", false
		}
	}
	return value.String(), true
}

func appendLiteralShellPart(value *strings.Builder, part syntax.WordPart) bool {
	switch part := part.(type) {
	case *syntax.Lit:
		value.WriteString(part.Value)
		return true
	case *syntax.SglQuoted:
		if part.Dollar {
			return false
		}
		value.WriteString(part.Value)
		return true
	case *syntax.DblQuoted:
		if part.Dollar {
			return false
		}
		for _, nested := range part.Parts {
			if !appendLiteralShellPart(value, nested) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
