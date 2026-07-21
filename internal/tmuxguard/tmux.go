package tmuxguard

import "strings"

func inspectTmux(args []string) string {
	scoped, commandAt, versionOnly, err := parseTmuxPrefix(args)
	if err != nil {
		return unknownShellReason
	}
	commandArgs := args[commandAt:]
	for _, arg := range args {
		if strings.Contains(arg, "#(") {
			return unknownShellReason
		}
	}
	if commandAt == len(args) {
		if versionOnly {
			return ""
		}
		return unknownShellReason
	}

	commands, err := splitTmuxCommands(commandArgs)
	if err != nil {
		return unknownShellReason
	}
	for _, words := range commands {
		if reason := inspectTmuxSequenceCommand(words[0], scoped); reason != "" {
			return reason
		}
	}
	return ""
}

// inspectTmuxSequenceCommand is the only command-level policy decision. It is
// called after separator normalization, so the first, middle, and last command
// in a chain cannot be judged by different token shapes.
func inspectTmuxSequenceCommand(rawCommand string, scoped bool) string {
	command := strings.ToLower(rawCommand)
	if isKillServerCommand(command) {
		if scoped {
			return ""
		}
		return broadTmuxReason
	}
	if safeUnscopedTmuxCommand(command) {
		return ""
	}
	if scoped && safeScopedTmuxCommand(command) {
		return ""
	}
	// Socket scope limits which tmux server a verb can mutate, but tmux verbs
	// may still launch host shell commands. Unknown verbs therefore deny until
	// they are explicitly audited and added to the small safe set below.
	return unknownShellReason
}

// splitTmuxCommands models tmux's command-sequence boundary. Shell quoting is
// already gone here: an escaped or quoted semicolon reaches tmux as either its
// own argument or an unescaped suffix. Reject empty sequence elements instead
// of guessing how a particular tmux version interprets them.
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
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-L" || arg == "-S":
			if i+1 >= len(args) || args[i+1] == "" {
				return false, 0, false, errUnsupportedShell
			}
			scoped = true
			i++
		case (strings.HasPrefix(arg, "-L") || strings.HasPrefix(arg, "-S")) && len(arg) > 2:
			scoped = true
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

// safeScopedTmuxCommand is intentionally exact and small. A scoped server is
// safe to mutate with these verbs, but scope does not make a verb that can
// launch a host process safe. Aliases, abbreviations, and future tmux verbs
// remain denied until audited rather than inheriting an allow by default.
func safeScopedTmuxCommand(command string) bool {
	switch command {
	case "clock-mode", "copy-mode", "kill-pane", "kill-session", "kill-window",
		"last-pane", "last-window", "next-layout", "next-window", "previous-layout",
		"previous-window", "refresh-client", "rename-session", "rename-window",
		"resize-pane", "resize-window", "rotate-window", "select-layout", "select-pane",
		"select-window", "swap-pane", "swap-window", "switch-client", "unlink-window":
		return true
	default:
		return false
	}
}
