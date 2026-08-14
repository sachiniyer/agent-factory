package sessionenv

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

var accountShellStartupNames = map[string]struct{}{
	"BASH_ENV":       {},
	"ENV":            {},
	"PROMPT_COMMAND": {},
	"PS0":            {},
	"PS1":            {},
	"PS2":            {},
	"PS4":            {},
	"ZDOTDIR":        {},
}

// AccountShellStartupNames returns environment variables that can execute or
// select shell startup code before an account-scoped command reaches its exec
// shim. Tmux removes these from its own command-shell boundary as well.
func AccountShellStartupNames() []string {
	names := make([]string, 0, len(accountShellStartupNames))
	for name := range accountShellStartupNames {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// AccountShellCommand turns the user's shell executable into an interactive,
// startup-file-free command. Shell startup files are code and could replace the
// identity variables after the account boundary established them.
func AccountShellCommand(shell string) (string, error) {
	if isAccountShellCommand(shell) {
		return shell, nil
	}
	words, ok := literalAccountShellWords(shell)
	if !ok || len(words) != 1 || !filepath.IsAbs(words[0]) {
		return "", fmt.Errorf("account-scoped shell requires one absolute supported shell executable, got %q", shell)
	}

	args := trustedAccountShellArgs(words[0])
	if args == nil {
		return "", fmt.Errorf("shell %q has no credential-safe account launch mode; supported system shells are bash, csh, dash, ksh, mksh, sh, and tcsh directly under /bin or /usr/bin", words[0])
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
	case "csh", "tcsh":
		return []string{"-f", "-i"}
	case "sh", "dash", "ksh", "mksh":
		return []string{"-i"}
	default:
		return nil
	}
}

// trustedAccountShellArgs requires a system-controlled executable path, not
// merely a supported basename. A repository or user wrapper named "bash" can
// ignore --noprofile/--norc and restore ambient credentials before execing the
// real shell; /bin and /usr/bin are the operator-controlled trust boundary.
func trustedAccountShellArgs(path string) []string {
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	if clean != path || (dir != "/bin" && dir != "/usr/bin") {
		return nil
	}
	return accountShellArgs(filepath.Base(clean))
}

func isAccountShellCommand(command string) bool {
	words, ok := literalAccountShellWords(command)
	if !ok || len(words) < 2 || !filepath.IsAbs(words[0]) {
		return false
	}
	want := trustedAccountShellArgs(words[0])
	return want != nil && slices.Equal(words[1:], want)
}

// IsAccountShellCommand reports whether command is the exact startup-free
// shell form AccountShellCommand generates. Tmux uses this proof when choosing
// the default command for additional windows in an account-scoped shell tab.
func IsAccountShellCommand(command string) bool {
	return isAccountShellCommand(command)
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
