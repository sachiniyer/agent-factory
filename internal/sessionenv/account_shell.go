package sessionenv

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

var accountShellStartupNames = map[string]struct{}{
	"BASH_ENV": {},
	"ENV":      {},
	"ZDOTDIR":  {},
}

// AccountShellCommand turns the user's shell executable into the exact
// interactive, startup-file-free command af permits inside a selected account.
// Shell startup files are code: allowing them here would let a ~/.bashrc or
// ~/.zshrc replace the identity variables after af established the boundary.
func AccountShellCommand(shell string) (string, error) {
	if isAccountShellCommand(shell) {
		return shell, nil
	}
	words, ok := literalAccountShellWords(shell)
	if !ok || len(words) != 1 || !filepath.IsAbs(words[0]) {
		return "", fmt.Errorf("account-scoped shell requires one absolute supported shell executable, got %q", shell)
	}

	args := accountShellArgs(filepath.Base(words[0]))
	if args == nil {
		return "", fmt.Errorf("shell %q has no startup-file-free account launch mode; supported shells: bash, csh, dash, fish, ksh, mksh, sh, tcsh, zsh", words[0])
	}
	command := shellquote.Quote(words[0])
	for _, arg := range args {
		command += " " + shellquote.Quote(arg)
	}
	return command, nil
}

func accountShellArgs(name string) []string {
	switch name {
	case "bash":
		return []string{"--noprofile", "--norc", "-i"}
	case "zsh":
		return []string{"--no-rcs", "--no-globalrcs", "-i"}
	case "fish":
		return []string{"--no-config", "--interactive"}
	case "csh", "tcsh":
		return []string{"-f", "-i"}
	case "sh", "dash", "ksh", "mksh":
		return []string{"-i"}
	default:
		return nil
	}
}

func isAccountShellCommand(command string) bool {
	words, ok := literalAccountShellWords(command)
	if !ok || len(words) < 2 || !filepath.IsAbs(words[0]) {
		return false
	}
	want := accountShellArgs(filepath.Base(words[0]))
	return want != nil && slices.Equal(words[1:], want)
}

func literalAccountShellWords(command string) ([]string, bool) {
	call, ok := singleSimpleCall(strings.TrimSpace(command))
	if !ok || len(call.Assigns) > 0 || !callIsLiteral(call) {
		return nil, false
	}
	return literalCommandArgs(call.Args)
}

func stripAccountShellStartupEnvironment(env []string) []string {
	out := env[:0]
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, drop := accountShellStartupNames[name]; drop {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}
