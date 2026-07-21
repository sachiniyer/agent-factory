package tmuxguard

import "strings"

func inspectTmux(args []string) string {
	scoped, commandAt, versionOnly, err := parseTmuxPrefix(args)
	if err != nil {
		return unknownShellReason
	}
	if tmuxKillServerIsBroad(args[commandAt:], scoped) {
		return broadTmuxReason
	}
	for _, arg := range args {
		if strings.Contains(arg, "#(") {
			return unknownShellReason
		}
	}
	if commandAt == len(args) {
		if scoped || versionOnly {
			return ""
		}
		return unknownShellReason
	}

	command := strings.ToLower(args[commandAt])
	if isKillServerCommand(command) {
		return "" // A preceding literal -L/-S made this teardown socket-scoped.
	}
	if tmuxCommandBuildsCommands(command) {
		return unknownShellReason
	}
	if scoped || safeUnscopedTmuxCommand(command) {
		return ""
	}
	return unknownShellReason
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

func tmuxKillServerIsBroad(args []string, socketScoped bool) bool {
	for _, arg := range args {
		if isKillServerCommand(arg) {
			return !socketScoped
		}
	}
	return false
}

func isKillServerCommand(arg string) bool {
	arg = strings.ToLower(arg)
	return arg != "" && strings.HasPrefix("kill-server", arg)
}

func tmuxCommandBuildsCommands(command string) bool {
	commands := []string{
		"bind-key", "choose-buffer", "choose-client", "choose-tree", "command-prompt",
		"confirm-before", "display-menu", "display-popup", "if-shell", "new-session",
		"new-window", "pipe-pane", "respawn-pane", "respawn-window", "run-shell",
		"set-hook", "source-file", "split-window",
	}
	for _, opaque := range commands {
		if command != "" && strings.HasPrefix(opaque, command) {
			return true
		}
	}
	return false
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
