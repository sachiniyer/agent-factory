//go:build !windows

package sessionenv

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var processExec = syscall.Exec

// WrapCommand builds the shell command handed to tmux. It contains only the af
// executable path, the selected agent, explicit variable NAMES, and the
// original pane command; environment values never enter argv.
func WrapCommand(executable, agent string, extras []string, command string) (string, error) {
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	args := []string{executable, ExecMarker, agent, strconv.Itoa(len(normalized))}
	args = append(args, normalized...)
	args = append(args, command)
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellQuote(arg)
	}
	return strings.Join(quoted, " "), nil
}

// HandleInternalExec handles the private session exec protocol when present.
// On an ordinary invocation it returns immediately. On a helper invocation it
// replaces the current process on success and exits 127 with a value-free error
// on failure.
func HandleInternalExec() {
	if len(os.Args) < 2 || os.Args[1] != ExecMarker {
		return
	}
	if err := execInvocation(os.Args[2:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "af: could not start the filtered session process")
		os.Exit(127)
	}
}

func execInvocation(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	agent := args[0]
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 || len(args) != count+3 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	extras, err := NormalizeExtraNames(args[2 : 2+count])
	if err != nil {
		return err
	}
	command := args[len(args)-1]
	environ := Filter(os.Environ(), agent, extras)
	// tmux runs shell-command through the system shell, not the user's login
	// shell. Keep that POSIX contract: program overrides and injected commands
	// commonly use assignment prefixes, redirects, and quoting that fish/tcsh
	// interpret differently.
	shell := "/bin/sh"
	return processExec(shell, []string{shell, "-c", command}, environ)
}

func shellQuote(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune("_@%+=:,./-", r))
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}
