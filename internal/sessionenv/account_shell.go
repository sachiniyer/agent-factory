package sessionenv

import (
	"fmt"
	"os"
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

var accountShellsPath = "/etc/shells"

// AccountShellCommand turns the user's shell executable into the exact
// interactive, startup-file-free command af permits inside a selected account.
// Shell startup files are code: allowing them here would let a ~/.bashrc or
// ~/.zshrc replace the identity variables after af established the boundary.
func AccountShellCommand(shell string) (string, error) {
	words, ok := literalAccountShellWords(shell)
	if !ok || len(words) == 0 || !filepath.IsAbs(words[0]) {
		return "", fmt.Errorf("account-scoped shell requires one absolute supported shell executable, got %q", shell)
	}

	args := accountShellArgs(filepath.Base(words[0]))
	if args == nil {
		return "", fmt.Errorf("shell %q has no startup-file-free account launch mode; supported shells: bash, csh, dash, fish, ksh, mksh, sh, tcsh, zsh", words[0])
	}
	if len(words) > 1 {
		if !slices.Equal(words[1:], args) {
			return "", fmt.Errorf("account-scoped shell %q does not use af's exact startup-file-free arguments", shell)
		}
		if err := validateAccountShellExecutable(words[0]); err != nil {
			return "", err
		}
		return shell, nil
	}
	if err := validateAccountShellExecutable(words[0]); err != nil {
		return "", err
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
	return want != nil && slices.Equal(words[1:], want) && validateAccountShellExecutable(words[0]) == nil
}

// validateAccountShellExecutable binds the startup-file flags to a real login
// shell chosen by the host, not merely to an arbitrary executable with a trusted
// basename. Canonical-path equality permits normal /bin -> /usr/bin layouts and
// harmless aliases to the same executable while refusing a repository or /tmp
// program that happens to be named bash, zsh, and so on.
func validateAccountShellExecutable(shell string) error {
	info, err := os.Stat(shell)
	if err != nil {
		return fmt.Errorf("account-scoped shell executable %q is not available: %w", shell, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("account-scoped shell executable %q is not an executable regular file", shell)
	}
	resolved, err := filepath.EvalSymlinks(shell)
	if err != nil {
		return fmt.Errorf("resolve account-scoped shell executable %q: %w", shell, err)
	}
	contents, err := os.ReadFile(accountShellsPath)
	if err != nil {
		return fmt.Errorf("read trusted login shells from %s: %w", accountShellsPath, err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		candidate := fields[0]
		if !filepath.IsAbs(candidate) || filepath.Base(candidate) != filepath.Base(shell) {
			continue
		}
		trusted, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil && trusted == resolved {
			return nil
		}
	}
	return fmt.Errorf("account-scoped shell executable %q is not a trusted login shell listed in %s", shell, accountShellsPath)
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
