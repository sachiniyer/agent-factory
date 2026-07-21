package tmuxguard

import (
	"path/filepath"
	"strings"
)

func inspectTmux(args []string) string {
	scoped, commandAt, versionOnly, err := parseTmuxPrefix(args)
	if err != nil {
		return unknownShellReason
	}
	for _, arg := range args {
		if strings.Contains(arg, "#(") || strings.ContainsAny(arg, "\r\n") {
			return unknownShellReason
		}
	}
	if commandAt == len(args) {
		if versionOnly {
			return ""
		}
		return unknownShellReason
	}

	commands, err := splitTmuxCommands(args[commandAt:])
	if err != nil {
		return unknownShellReason
	}
	for _, words := range commands {
		if isKillServerCommand(words[0]) && !scoped {
			return broadTmuxReason
		}
	}
	for _, words := range commands {
		command := strings.ToLower(words[0])
		if isKillServerCommand(command) || safeUnscopedTmuxCommand(command) {
			continue
		}
		if scoped && safeScopedTmuxCommand(command) {
			continue
		}
		// Scoped servers may still execute host commands, so socket scope is
		// not proof that an unknown tmux verb is safe. Only explicitly modeled
		// commands reach this point; additions in future tmux releases deny by
		// default until audited.
		return unknownShellReason
	}
	return ""
}

// splitTmuxCommands models tmux's command-sequence boundary. A semicolon is a
// separator when it is its own argument or is unescaped at the end of one;
// a backslash reaching tmux escapes it as data. Empty sequence elements are
// rejected instead of guessing how a particular tmux version treats them.
func splitTmuxCommands(args []string) ([][]string, error) {
	var commands [][]string
	var current []string
	flush := func() error {
		if len(current) == 0 {
			return errUnsupportedShell
		}
		commands = append(commands, current)
		current = nil
		return nil
	}

	for _, arg := range args {
		separator := arg == ";" || hasUnescapedTrailingSemicolon(arg)
		if separator && arg != ";" {
			arg = strings.TrimSuffix(arg, ";")
			if arg == "" || hasUnescapedTrailingSemicolon(arg) {
				return nil, errUnsupportedShell
			}
			current = append(current, arg)
		} else if !separator {
			current = append(current, arg)
		}
		if separator {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if len(current) > 0 {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if len(commands) == 0 {
		return nil, errUnsupportedShell
	}
	return commands, nil
}

func hasUnescapedTrailingSemicolon(arg string) bool {
	if !strings.HasSuffix(arg, ";") {
		return false
	}
	backslashes := 0
	for i := len(arg) - 2; i >= 0 && arg[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 0
}

func parseTmuxPrefix(args []string) (scoped bool, commandAt int, versionOnly bool, err error) {
	scopeSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-L" || arg == "-S":
			if scopeSeen || i+1 >= len(args) || args[i+1] == "" {
				return false, 0, false, errUnsupportedShell
			}
			scopeSeen = true
			scoped = isolatedTmuxSocket(args[i+1])
			i++
		case (strings.HasPrefix(arg, "-L") || strings.HasPrefix(arg, "-S")) && len(arg) > 2:
			if scopeSeen {
				return false, 0, false, errUnsupportedShell
			}
			scopeSeen = true
			scoped = isolatedTmuxSocket(arg[2:])
		case arg == "-T":
			if i+1 >= len(args) || args[i+1] == "" {
				return false, 0, false, errUnsupportedShell
			}
			i++
		case strings.HasPrefix(arg, "-T") && len(arg) > 2:
		case arg == "-c" || arg == "-f" || strings.HasPrefix(arg, "-c") || strings.HasPrefix(arg, "-f"):
			return false, 0, false, errUnsupportedShell
		case isTmuxFlagCluster(arg):
			versionOnly = versionOnly || strings.ContainsRune(arg, 'V')
		case strings.HasPrefix(arg, "-"):
			return false, 0, false, errUnsupportedShell
		default:
			return scoped, i, versionOnly, nil
		}
	}
	return scoped, len(args), versionOnly, nil
}

func isolatedTmuxSocket(socket string) bool {
	// tmux's implicit shared server is named "default", including when -S
	// spells its usual /tmp/tmux-<uid>/default path explicitly. Naming that
	// socket does not make a kill-server isolated.
	return socket != "" && filepath.Base(socket) != "default"
}

func isTmuxFlagCluster(arg string) bool {
	return len(arg) > 1 && arg[0] == '-' && strings.IndexFunc(arg[1:], func(flag rune) bool {
		return !strings.ContainsRune("2CDlNuVv", flag)
	}) == -1
}

func isKillServerCommand(arg string) bool {
	arg = strings.ToLower(arg)
	return arg != "" && strings.HasPrefix("kill-server", arg)
}

func safeUnscopedTmuxCommand(command string) bool {
	switch command {
	case "capture-pane", "capturep", "display-message", "display",
		"has-session", "has", "info", "list-buffers", "lsb", "list-clients", "lsc",
		"list-commands", "lscm", "list-keys", "lsk", "list-panes", "lsp",
		"list-sessions", "ls", "list-windows", "lsw", "show-environment", "showenv",
		"show-hooks", "show-messages", "show-options", "show", "show-window-options", "showw":
		return true
	default:
		return false
	}
}

func safeScopedTmuxCommand(command string) bool {
	switch command {
	case "copy-mode", "clock-mode", "kill-pane", "kill-session", "kill-window",
		"last-pane", "last-window", "next-layout", "next-window", "previous-layout",
		"previous-window", "refresh-client", "rename-session", "rename-window",
		"resize-pane", "resize-window", "rotate-window", "select-layout", "select-pane",
		"select-window", "swap-pane", "swap-window", "switch-client", "unlink-window":
		return true
	default:
		return false
	}
}
