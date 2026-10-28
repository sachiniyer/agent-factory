package sessionenv

import (
	"errors"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// ProgramExecutable is where a configured program command's executable word
// sits and what the command itself says about how it is looked up (#5571 W2.1
// follow-up). It is the one answer remote program resolution reads, derived by
// the same literal reader and env unwrapping the account proofs use, so
// `exec codex`, `PATH=/opt/bin codex` and `env NAME=v codex` are understood
// once rather than re-parsed ad hoc.
type ProgramExecutable struct {
	// Name is the executable word, unquoted: the thing a host lookup resolves.
	Name string
	// Start and End are the byte offsets of the RAW word in the command
	// (quotes included), so a rewrite replaces exactly that word and leaves
	// every other byte — assignments, env, exec, arguments — in place.
	Start, End int
	// PATH is the command-local PATH= value that governs the lookup, when
	// HasPATH. The last assignment wins, as in a shell.
	PATH    string
	HasPATH bool
	// Opaque is true when the command changes lookup in a way a host probe
	// cannot reproduce — env -i, env -u PATH, env -C. Such a command is not
	// resolved; it runs as written.
	Opaque bool
}

// LocateProgramExecutable finds the executable word of one literal simple
// command, looking through leading assignments, the exec builtin and an env
// wrapper. It returns ErrNoProgramExecutable when the command is only
// assignments or empty, and ErrProgramNotLiteral when it is not one literal
// call (a pipeline, a dynamic word).
func LocateProgramExecutable(command string) (ProgramExecutable, error) {
	if strings.TrimSpace(command) == "" {
		return ProgramExecutable{}, ErrNoProgramExecutable
	}
	call, ok := singleCallIgnoringRedirections(command)
	if !ok {
		if onlyAssignments(command) {
			return ProgramExecutable{}, ErrNoProgramExecutable
		}
		return ProgramExecutable{}, ErrProgramNotLiteral
	}
	if !callIsLiteral(call) {
		return ProgramExecutable{}, ErrProgramNotLiteral
	}
	var out ProgramExecutable
	for _, assign := range call.Assigns {
		if assign.Name != nil && assign.Name.Value == "PATH" {
			value, _ := literalShellWord(assign.Value)
			out.PATH, out.HasPATH = value, true
		}
	}
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 {
		return ProgramExecutable{}, ErrNoProgramExecutable
	}
	args, ok := literalCommandArgs(words)
	if !ok {
		return ProgramExecutable{}, ErrProgramNotLiteral
	}
	idx := 0
	if isEnvExecutableByBase(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return ProgramExecutable{}, ErrProgramNotLiteral
		}
		idx = 1 + invocation.CommandIndex
		if invocation.ClearEnvironment || invocation.Chdir != "" {
			out.Opaque = true
		}
		for _, m := range invocation.Mutations {
			if m.Name != "PATH" {
				continue
			}
			if m.Unset {
				out.Opaque = true
				continue
			}
			out.PATH, out.HasPATH = m.Value, true
		}
	}
	word := words[idx]
	out.Name = args[idx]
	out.Start, out.End = int(word.Pos().Offset()), int(word.End().Offset())
	return out, nil
}

var (
	// ErrNoProgramExecutable: the command holds no executable word at all.
	ErrNoProgramExecutable = errors.New("the command has no executable word")
	// ErrProgramNotLiteral: the command is not one literal simple call, so no
	// word can be named as its executable without guessing.
	ErrProgramNotLiteral = errors.New("the command is not one literal simple command")
)

// onlyAssignments reports whether command parses as one statement made of
// assignments and nothing else (FOO=1 BAR=2).
func onlyAssignments(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil || len(file.Stmts) != 1 || file.Stmts[0] == nil {
		return false
	}
	call, ok := file.Stmts[0].Cmd.(*syntax.CallExpr)
	return ok && len(call.Args) == 0 && len(call.Assigns) > 0
}
