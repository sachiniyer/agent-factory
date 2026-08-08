//go:build !windows

package sessionenv

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

var processExec = syscall.Exec

// WrapCommand builds the shell command handed to tmux. It contains only the af
// executable path, the selected agent, explicit variable NAMES, and the
// original pane command; environment values never enter argv.
func WrapCommand(executable, agent string, extras []string, command string) (string, error) {
	return wrapCommand(executable, agent, "", nil, extras, command)
}

// WrapAccountCommand is WrapCommand for a session scoped to a named account.
//
// Only the account NAME travels in argv — never its directory and never a
// credential. The shim resolves the name against its own AF home, so argv stays
// free of anything worth reading out of `ps` (#3051).
// generated carries the argument words af's own launcher appended to the pane
// command, so the boundary can verify its own output instead of refusing it
// (#3083). These are not secrets and are already visible in the command operand
// beside them; what argv gains is the CLAIM that af authored them.
func WrapAccountCommand(executable, agent, account string, generated, extras []string, command string) (string, error) {
	if strings.TrimSpace(account) == "" {
		return "", fmt.Errorf("account-scoped launch requires an account name")
	}
	return wrapCommand(executable, agent, account, generated, extras, command)
}

func wrapCommand(executable, agent, account string, generated, extras []string, command string) (string, error) {
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	marker := ExecMarker
	if account != "" {
		marker = AccountExecMarker
	}
	args := []string{executable, marker, agent, strconv.Itoa(len(normalized))}
	if account != "" {
		// Both COUNTS are length-prefixed rather than delimiter-separated, for the
		// reason the extras count already is: a generated argument is an arbitrary
		// string (a path can contain anything), so any sentinel could appear inside
		// one and a mis-split would hand the guard a different claim than the
		// launcher made.
		args = append(args, account, strconv.Itoa(len(generated)))
		args = append(args, generated...)
	}
	args = append(args, normalized...)
	args = append(args, command)
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellquote.Quote(arg)
	}
	return strings.Join(quoted, " "), nil
}

// HandleInternalExec handles the private session exec protocol when present.
// On an ordinary invocation it returns immediately. On a helper invocation it
// replaces the current process on success and exits 127 with a value-free error
// on failure.
func HandleInternalExec() {
	if len(os.Args) < 2 {
		return
	}
	scoped := os.Args[1] == AccountExecMarker
	if os.Args[1] != ExecMarker && !scoped {
		return
	}
	if err := execInvocation(os.Args[2:], scoped); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "af: could not start the filtered session process")
		os.Exit(127)
	}
}

func execInvocation(args []string, scoped bool) error {
	trailing := 3
	if scoped {
		trailing = 5
	}
	if len(args) < trailing {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	agent := args[0]
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	offset := 2
	account := ""
	var generated []string
	if scoped {
		account = args[2]
		generatedCount, gerr := strconv.Atoi(args[3])
		// Compared against the REMAINING room, never `trailing+generatedCount`: a
		// maximum-sized integer makes that addition overflow to a negative bound, the
		// check passes, and the slice below PANICS instead of returning this
		// function's generic refusal (#3083 review). Subtraction cannot overflow here
		// because trailing is a constant no larger than len(args), already checked.
		if gerr != nil || generatedCount < 0 || generatedCount > len(args)-trailing {
			return fmt.Errorf("malformed internal session environment invocation")
		}
		generated = args[4 : 4+generatedCount]
		offset = 4 + generatedCount
	}
	// The exact total, checked AFTER both counts are known. A length that merely
	// fits leaves room for an unaccounted argument between the two lists, and this
	// argv is what the boundary's whole claim rests on.
	if len(args) != offset+count+1 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	extras, err := NormalizeExtraNames(args[offset : offset+count])
	if err != nil {
		return err
	}
	command := args[len(args)-1]
	environ := FilterForCommand(os.Environ(), agent, command, extras)
	// The account boundary is applied HERE, in the pane, after filtering and
	// immediately before exec — the last point where anything can still change
	// what the agent will see. A failure REFUSES the launch rather than falling
	// through to the ambient identity, which would be the silent wrong-account
	// outcome the whole feature exists to prevent (#3051).
	if scoped {
		environ, err = applyAccountScope(environ, agent, account, command, generated)
		if err != nil {
			return err
		}
	}
	// tmux runs shell-command through the system shell, not the user's login
	// shell. Keep that POSIX contract: program overrides and injected commands
	// commonly use assignment prefixes, redirects, and quoting that fish/tcsh
	// interpret differently.
	shell := "/bin/sh"
	return processExec(shell, []string{shell, "-c", command}, environ)
}
