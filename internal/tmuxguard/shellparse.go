package tmuxguard

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

var errUnsupportedShell = errors.New("unsupported shell construct")
var errOpaqueStdin = errors.New("unmodeled here-document or stdin consumer")

// parseShellCommands parses Bash syntax with a maintained parser, but resolves
// only a deliberately small, literal subset. Dynamic words fail closed before
// command inspection, so new expansion shapes cannot silently become allows.
func parseShellCommands(command string) ([][]string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnsupportedShell, err)
	}

	var commands [][]string
	var walkErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || walkErr != nil {
			return false
		}
		if isOpaqueStdinSyntax(node) {
			walkErr = errOpaqueStdin
			return false
		}
		switch node := node.(type) {
		case *syntax.CallExpr:
			words := make([]string, 0, len(node.Args))
			for _, word := range node.Args {
				value, err := literalWord(word)
				if err != nil {
					walkErr = err
					return false
				}
				words = append(words, value)
			}
			if len(words) > 0 {
				commands = append(commands, words)
			}
		case *syntax.Word:
			_, walkErr = literalWord(node)
		case *syntax.Assign:
			// Even a literal assignment can change later command resolution
			// (PATH, BASH_ENV, LD_PRELOAD, exported shell functions, and more).
			walkErr = errUnsupportedShell
		case *syntax.ArithmCmd, *syntax.CStyleLoop, *syntax.FuncDecl, *syntax.LetClause:
			walkErr = errUnsupportedShell
		}
		return walkErr == nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return commands, nil
}

func isOpaqueStdinSyntax(node syntax.Node) bool {
	switch node := node.(type) {
	case *syntax.BinaryCmd:
		return node.Op == syntax.Pipe || node.Op == syntax.PipeAll
	case *syntax.Redirect:
		return node.Op == syntax.Hdoc || node.Op == syntax.DashHdoc || node.Op == syntax.WordHdoc
	default:
		return false
	}
}

func literalWord(word *syntax.Word) (string, error) {
	for _, part := range word.Parts {
		if err := validateLiteralPart(part, false); err != nil {
			return "", err
		}
	}
	fields, err := expand.Fields(&expand.Config{}, word)
	if err != nil || len(fields) != 1 {
		return "", errUnsupportedShell
	}
	return fields[0], nil
}

func validateLiteralPart(part syntax.WordPart, quoted bool) error {
	switch part := part.(type) {
	case *syntax.Lit:
		if !quoted && strings.ContainsAny(part.Value, "*?[{}~") {
			return errUnsupportedShell
		}
		return nil
	case *syntax.SglQuoted:
		if part.Dollar {
			return errUnsupportedShell
		}
		return nil
	case *syntax.DblQuoted:
		if part.Dollar {
			return errUnsupportedShell
		}
		for _, nested := range part.Parts {
			if err := validateLiteralPart(nested, true); err != nil {
				return err
			}
		}
		return nil
	default:
		return errUnsupportedShell
	}
}
