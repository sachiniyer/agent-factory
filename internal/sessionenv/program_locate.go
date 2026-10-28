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
	// PATHDynamic is true when the command assigns PATH from something computed
	// at run time. It only matters to a bare-name lookup: a path-qualified
	// executable never consults PATH, so it is reported, not refused (#5781).
	PATHDynamic bool
	// Opaque is true when the command changes lookup in a way a host probe
	// cannot reproduce — env -i, env -u PATH, env -C. Such a command is not
	// resolved; it runs as written.
	Opaque bool
}

// LocateProgramExecutable finds the executable word of one simple command,
// looking through leading assignments, the exec builtin and an env wrapper. It
// returns ErrNoProgramExecutable when the command is only assignments or empty,
// ErrProgramNotLiteral when it is not one simple call (a pipeline), and
// ErrProgramDynamic when the executable word is computed at run time. A
// computed PATH is not an error here: it sets PATHDynamic, and the caller
// refuses it only when the executable is a bare name that PATH would resolve.
//
// Only what decides WHICH program runs has to be literal: the leading words up
// to and including the executable, and a PATH assignment. Arguments after the
// executable, and the values of other assignments, may be dynamic — they cannot
// change which file runs (#5767). The grant paths keep callIsLiteral's strict
// reading; this is resolution only.
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
	var out ProgramExecutable
	for _, assign := range call.Assigns {
		if assign == nil || assign.Name == nil || assign.Name.Value != "PATH" {
			continue
		}
		value, literal := "", false
		if assign.Value != nil && !assign.Append && assign.Index == nil && assign.Array == nil {
			value, literal = literalShellWord(assign.Value)
		}
		out.PATH, out.HasPATH, out.PATHDynamic = value, literal, !literal
	}
	words, _ := stripExecPrefix(call.Args)
	if len(words) == 0 {
		return ProgramExecutable{}, ErrNoProgramExecutable
	}
	args := make([]string, len(words))
	dynamic := make([]bool, len(words))
	for i, word := range words {
		if value, ok := literalShellWord(word); ok {
			args[i] = value
			continue
		}
		args[i], dynamic[i] = dynamicWordPlaceholder(word), true
	}
	idx := 0
	if !dynamic[0] && isEnvExecutableByBase(args[0]) {
		invocation, err := envcommand.Parse(args[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return ProgramExecutable{}, ErrProgramNotLiteral
		}
		idx = 1 + invocation.CommandIndex
		// A computed word in front of the executable is harmless only as the
		// value of an assignment; as an option operand it could name anything.
		for i := 1; i < idx; i++ {
			if dynamic[i] && !strings.HasSuffix(args[i], "="+dynamicMark) {
				return ProgramExecutable{}, ErrProgramDynamic
			}
		}
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
			dyn := strings.Contains(m.Value, dynamicMark)
			out.PATH, out.HasPATH, out.PATHDynamic = m.Value, !dyn, dyn
			if dyn {
				out.PATH = ""
			}
		}
	}
	word := words[idx]
	out.Name = args[idx]
	out.Start, out.End = int(word.Pos().Offset()), int(word.End().Offset())
	if dynamic[idx] {
		// Offsets stay filled so a caller can tell a computed path-shaped word
		// ($HOME/bin/claude) from a computed bare name ($AGENT).
		out.Name = ""
		return out, ErrProgramDynamic
	}
	return out, nil
}

const dynamicMark = "\x00dynamic"

// dynamicWordPlaceholder stands in for a word whose value is computed at run
// time when env's operands are parsed. A word that starts NAME= keeps that
// shape — env treats it as an assignment whatever the value expands to, and
// whether or not the whole word is quoted ("VAR=$HOME/x", #5780) — so its
// name, and only its name, is known. That holds only while the word stays ONE
// operand: an unquoted expansion ("VAR="$X, VAR=$X) is field-split by the
// shell, and `X='v PATH=/custom/bin'` hands env a second, real assignment
// (#5872). A word that can split is as computed as any other and is refused.
func dynamicWordPlaceholder(word *syntax.Word) string {
	if !staysOneField(word.Parts) {
		return dynamicMark
	}
	if name, _, found := strings.Cut(leadingLiteral(word.Parts), "="); found && validAssignmentName(name) {
		return name + "=" + dynamicMark
	}
	return dynamicMark
}

// staysOneField reports whether the shell is certain to pass the word as a
// single operand: every expansion sits inside double quotes, and no unquoted
// literal is a glob pattern.
func staysOneField(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[") {
				return false
			}
		case *syntax.SglQuoted, *syntax.DblQuoted:
		default:
			return false
		}
	}
	return true
}

// leadingLiteral returns the literal text a word starts with, looking through
// the first quoting layer; it stops at the first expansion.
func leadingLiteral(parts []syntax.WordPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			b.WriteString(leadingLiteral(p.Parts))
			if !allLiteral(p.Parts) {
				return b.String()
			}
		default:
			return b.String()
		}
	}
	return b.String()
}

func allLiteral(parts []syntax.WordPart) bool {
	for _, part := range parts {
		if _, ok := part.(*syntax.Lit); !ok {
			return false
		}
	}
	return true
}

func validAssignmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

var (
	// ErrNoProgramExecutable: the command holds no executable word at all.
	ErrNoProgramExecutable = errors.New("the command has no executable word")
	// ErrProgramNotLiteral: the command is not one literal simple call, so no
	// word can be named as its executable without guessing.
	ErrProgramNotLiteral = errors.New("the command is not one literal simple command")
	// ErrProgramDynamic: the executable word is computed at run time. The
	// returned offsets, when set, locate the word.
	ErrProgramDynamic = errors.New("the executable word or its PATH is computed at run time")
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
