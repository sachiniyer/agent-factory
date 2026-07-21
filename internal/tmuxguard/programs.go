package tmuxguard

import (
	"path/filepath"
	"strings"
)

func inspectPrintf(args []shellWord) string {
	if len(args) == 0 {
		return ""
	}
	if !args[0].resolved {
		return unknownShellReason
	}
	if strings.HasPrefix(args[0].literal, "-v") {
		// Bash treats the next argument as a variable name, and array indices
		// in that name undergo another round of arithmetic/shell evaluation.
		return unknownShellReason
	}
	return ""
}

func inspectTest(name string, args []shellWord) string {
	if name == "[" {
		if len(args) == 0 || !args[len(args)-1].resolved || args[len(args)-1].literal != "]" {
			return unknownShellReason
		}
		args = args[:len(args)-1]
	}
	if len(args) <= 1 {
		return ""
	}
	if len(args) >= 4 && hasDynamicWord(args) {
		return unknownShellReason
	}
	for i, arg := range args {
		if !arg.resolved {
			if testOperatorPosition(i, len(args)) {
				return unknownShellReason
			}
			continue
		}
		switch arg.literal {
		case "-v", "-R", "-eq", "-ne", "-le", "-ge", "-lt", "-gt":
			// Bash re-evaluates variable names and arithmetic operands for
			// these operators, including command substitutions stored in data.
			return unknownShellReason
		}
	}
	return ""
}

func testOperatorPosition(index, count int) bool {
	switch count {
	case 2:
		return index == 0
	case 3:
		return index == 1
	default:
		return true
	}
}

func inspectPIDKill(args []shellWord) string {
	if len(args) < 2 || !args[0].resolved || args[0].literal != "--" {
		return unknownShellReason
	}
	for _, arg := range args[1:] {
		if !arg.resolved || !safeProcessID(arg.literal) {
			return unknownShellReason
		}
	}
	return ""
}

func safeProcessID(value string) bool {
	if !allDecimal(value) {
		return false
	}
	trimmed := strings.TrimLeft(value, "0")
	return trimmed != "" && trimmed != "1"
}

func inspectGit(args []shellWord) string {
	if len(args) == 1 && args[0].resolved && (args[0].literal == "--help" || args[0].literal == "--version") {
		return ""
	}
	commandAt, ok := safeGitSubcommandAt(args)
	if !ok {
		return unknownShellReason
	}
	command := args[commandAt].literal
	staticArgs, ok := staticGitOptions(args[commandAt+1:])
	if !ok || !safeGitSubcommand(command) || gitArgsDispatch(command, staticArgs) {
		return unknownShellReason
	}
	return ""
}

func safeGitSubcommandAt(args []shellWord) (int, bool) {
	for i, arg := range args {
		if !arg.resolved {
			return 0, false
		}
		switch arg.literal {
		case "--":
			return i + 1, i+1 < len(args)
		case "--bare", "--glob-pathspecs", "--icase-pathspecs", "--literal-pathspecs",
			"--no-pager", "--no-replace-objects", "--noglob-pathspecs":
			continue
		case "--help", "--version":
			return 0, len(args) == 1
		}
		if strings.HasPrefix(arg.literal, "-") {
			// In particular, reject -c/--config-env and every option that can
			// select configuration, a work tree, or an executable directory.
			return 0, false
		}
		return i, true
	}
	return 0, false
}

func staticGitOptions(args []shellWord) ([]string, bool) {
	literals := make([]string, 0, len(args))
	optionsDone := false
	for _, arg := range args {
		if !arg.resolved {
			if !optionsDone {
				return nil, false
			}
			continue
		}
		if arg.literal == "--" {
			optionsDone = true
		}
		literals = append(literals, arg.literal)
	}
	return literals, true
}

func safeGitSubcommand(name string) bool {
	switch name {
	case "add", "am", "apply", "archive", "branch", "cat-file", "check-attr", "check-ignore",
		"check-mailmap", "checkout", "cherry", "cherry-pick", "clean", "commit",
		"count-objects", "describe", "diff", "diff-files", "diff-index", "diff-tree",
		"format-patch", "fsck", "gc", "grep", "hash-object", "init", "log", "ls-files",
		"ls-tree", "merge", "merge-base", "merge-file", "merge-tree", "mktag",
		"mktree", "mv", "name-rev", "notes", "prune", "range-diff",
		"read-tree", "reflog", "repack", "replace", "reset", "restore", "rev-list",
		"rev-parse", "revert", "rm", "shortlog", "show", "show-branch", "show-ref",
		"sparse-checkout", "stash", "status", "stripspace", "switch", "symbolic-ref", "tag",
		"update-index", "update-ref", "verify-commit", "verify-pack", "verify-tag", "whatchanged",
		"worktree", "write-tree":
		return true
	default:
		// Unknown names may be shell aliases or git-<name> executables.
		return false
	}
}

func gitArgsDispatch(command string, args []string) bool {
	for _, arg := range args {
		option := arg
		if before, _, found := strings.Cut(option, "="); found {
			option = before
		}
		switch option {
		case "--config-env", "--exec", "--ext-diff", "--receive-pack", "--strategy",
			"--textconv", "--upload-pack":
			return true
		}
		if command == "grep" && (longOptionMatches(option, "--open-files-in-pager", "--open") || option == "-O" ||
			(strings.HasPrefix(arg, "-O") && len(arg) > 2)) {
			return true
		}
		if command == "clone" && (option == "-c" || option == "--config" || option == "-u" ||
			(strings.HasPrefix(arg, "-c") && len(arg) > 2) ||
			(strings.HasPrefix(arg, "-u") && len(arg) > 2)) {
			return true
		}
		if command == "merge" && (option == "-s" || (strings.HasPrefix(arg, "-s") && len(arg) > 2)) {
			return true
		}
		if command == "archive" && option == "--remote" {
			return true
		}
	}
	return false
}

func inspectDocker(args []shellWord) string {
	commandAt, noCommand, ok := dockerSubcommandAt(args)
	if !ok {
		return unknownShellReason
	}
	if noCommand {
		return ""
	}
	if !args[commandAt].resolved {
		return unknownShellReason
	}
	command := args[commandAt].literal
	if dockerInspectionCommand(command) {
		return ""
	}
	if !dockerCommandGroup(command) || commandAt+1 >= len(args) || !args[commandAt+1].resolved {
		return unknownShellReason
	}
	if dockerInspectionPath(command + "/" + args[commandAt+1].literal) {
		return ""
	}
	return unknownShellReason
}

func dockerSubcommandAt(args []shellWord) (int, bool, bool) {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return 0, false, false
		}
		arg := args[i].literal
		switch arg {
		case "--help", "--version", "-v":
			return 0, len(args) == 1, len(args) == 1
		case "--debug", "-D", "--tls", "--tlsverify":
			continue
		case "--config", "-c", "--context", "--host", "-H", "--log-level", "-l",
			"--tlscacert", "--tlscert", "--tlskey":
			if i+1 >= len(args) {
				return 0, false, false
			}
			i++
			continue
		case "--":
			if i+1 >= len(args) {
				return 0, false, false
			}
			return i + 1, false, true
		}
		if strings.HasPrefix(arg, "--config=") || strings.HasPrefix(arg, "--context=") ||
			strings.HasPrefix(arg, "--host=") || strings.HasPrefix(arg, "--log-level=") ||
			strings.HasPrefix(arg, "--tlscacert=") || strings.HasPrefix(arg, "--tlscert=") ||
			strings.HasPrefix(arg, "--tlskey=") || strings.HasPrefix(arg, "-H=") ||
			strings.HasPrefix(arg, "-l=") {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return 0, false, false
		}
		return i, false, true
	}
	return 0, false, false
}

func dockerInspectionCommand(name string) bool {
	switch name {
	case "diff", "events", "history", "images", "info", "inspect", "logs", "port", "ps",
		"search", "stats", "top", "version", "wait":
		return true
	default:
		return false
	}
}

func dockerCommandGroup(name string) bool {
	switch name {
	case "container", "image", "network", "node", "plugin", "secret", "service", "stack",
		"system", "trust", "volume":
		return true
	default:
		return false
	}
}

func dockerInspectionPath(path string) bool {
	switch path {
	case "container/diff", "container/inspect", "container/list", "container/logs", "container/port",
		"container/stats", "container/top", "container/wait", "image/history", "image/inspect",
		"image/list", "network/inspect", "network/list", "node/inspect", "node/list", "node/ps",
		"plugin/inspect", "plugin/list", "secret/inspect", "secret/list", "service/inspect",
		"service/logs", "service/list", "service/ps", "stack/list", "stack/ps", "stack/services",
		"system/df", "system/info", "trust/inspect", "volume/inspect", "volume/list":
		return true
	default:
		return false
	}
}

func inspectRipgrep(args []shellWord) string {
	optionsDone := false
	for _, arg := range args {
		if !arg.resolved {
			if !optionsDone {
				return unknownShellReason
			}
			continue
		}
		if arg.literal == "--" {
			optionsDone = true
			continue
		}
		if optionsDone {
			continue
		}
		if arg.literal == "--pre" || strings.HasPrefix(arg.literal, "--pre=") ||
			arg.literal == "--hostname-bin" || strings.HasPrefix(arg.literal, "--hostname-bin=") ||
			(arg.literal == "--search-zip" || strings.HasPrefix(arg.literal, "--search-zip=")) ||
			(strings.HasPrefix(arg.literal, "-") &&
				!strings.HasPrefix(arg.literal, "--") && strings.ContainsRune(arg.literal[1:], 'z')) {
			return unknownShellReason
		}
	}
	return ""
}

func inspectSort(args []shellWord) string {
	optionsDone := false
	for _, arg := range args {
		if !arg.resolved {
			if !optionsDone {
				return unknownShellReason
			}
			continue
		}
		if arg.literal == "--" {
			optionsDone = true
			continue
		}
		if optionsDone {
			continue
		}
		option, _, _ := strings.Cut(arg.literal, "=")
		if longOptionMatches(option, "--compress-program", "--co") {
			return sortReason
		}
	}
	return ""
}

func inspectGo(args []shellWord) string {
	literals, ok := literalWords(args)
	if !ok || len(literals) == 0 {
		return unknownShellReason
	}
	// This best-effort grammar rejects command-line executor selectors. Literal
	// package and source operands remain a compatibility surface; their contents
	// are explicitly outside this guard's model and require host containment.
	command := literals[0]
	switch command {
	case "help", "version":
		return ""
	case "build", "clean", "fmt", "get", "install", "list", "mod", "run", "test", "vet", "work":
	case "env":
		for _, arg := range literals[1:] {
			if arg == "-w" || arg == "-u" || strings.HasPrefix(arg, "-w=") || strings.HasPrefix(arg, "-u=") {
				return unknownShellReason
			}
		}
		return ""
	default:
		// generate, tool, and future subcommands can select executables.
		return unknownShellReason
	}
	for _, arg := range literals[1:] {
		option := arg
		if before, _, found := strings.Cut(option, "="); found {
			option = before
		}
		switch option {
		case "-exec", "-toolexec", "-vettool":
			return unknownShellReason
		}
	}
	return ""
}

func pythonVersionedExecutable(name string) bool {
	for _, prefix := range []string{"python2.", "python3.", "pypy2.", "pypy3."} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		version := strings.TrimPrefix(name, prefix)
		if version == "" {
			return false
		}
		for _, char := range version {
			if (char < '0' || char > '9') && char != '.' {
				return false
			}
		}
		return true
	}
	return false
}

func inspectPython(args []shellWord) string {
	for i := 0; i < len(args); i++ {
		if !args[i].resolved {
			return unknownShellReason
		}
		arg := args[i].literal
		switch {
		case arg == "--help" || arg == "--version" || arg == "-h" || arg == "-V":
			return ""
		case arg == "-c" || arg == "-m" || arg == "-":
			return unknownShellReason
		case arg == "--":
			if i+1 >= len(args) || !args[i+1].resolved || !literalPythonScriptPath(args[i+1].literal) {
				return unknownShellReason
			}
			return ""
		case arg == "-W":
			if i+1 >= len(args) || !args[i+1].resolved {
				return unknownShellReason
			}
			i++
		case strings.HasPrefix(arg, "-W"):
			continue
		case strings.HasPrefix(arg, "-"):
			if !safePythonFlags(arg) {
				return unknownShellReason
			}
		default:
			// A literal script path is the compatibility boundary for argv. The
			// guard cannot prove its contents reviewed; containment remains the
			// boundary for source-file execution.
			if literalPythonScriptPath(arg) {
				return ""
			}
			return unknownShellReason
		}
	}
	return unknownShellReason
}

func safePythonFlags(arg string) bool {
	if len(arg) < 2 {
		return false
	}
	for _, flag := range arg[1:] {
		if !strings.ContainsRune("BbEhIOPqRsSuvx", flag) {
			return false
		}
	}
	return true
}

func literalPythonScriptPath(path string) bool {
	cleaned := strings.ToLower(filepath.Clean(path))
	if !strings.HasSuffix(cleaned, ".py") {
		return false
	}
	for _, opaqueRoot := range []string{"/dev", "/proc", "/sys"} {
		if cleaned == opaqueRoot || strings.HasPrefix(cleaned, opaqueRoot+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

func inspectJournalctl(args []shellWord) string {
	for _, arg := range args {
		if arg.resolved && (arg.literal == "--no-pager" || arg.literal == "-P") {
			return ""
		}
	}
	return unknownShellReason
}

func inspectMake(args []shellWord) string {
	for _, arg := range args {
		if !arg.resolved || strings.HasPrefix(arg.literal, "-") || strings.ContainsRune(arg.literal, '=') {
			return unknownShellReason
		}
	}
	// Bare targets exclude command-line dispatch syntax. Recipe/source contents
	// remain outside this best-effort model and require host containment.
	return ""
}

func longOptionMatches(option, full, minimum string) bool {
	return len(option) >= len(minimum) && strings.HasPrefix(full, option)
}

func inspectFile(args []shellWord) string {
	optionsDone := false
	for _, arg := range args {
		if !arg.resolved {
			if !optionsDone {
				return unknownShellReason
			}
			continue
		}
		if arg.literal == "--" {
			optionsDone = true
			continue
		}
		if optionsDone {
			continue
		}
		if arg.literal == "--uncompress" || arg.literal == "--uncompress-noreport" ||
			(strings.HasPrefix(arg.literal, "-") && !strings.HasPrefix(arg.literal, "--") &&
				strings.ContainsAny(arg.literal[1:], "zZ")) {
			return unknownShellReason
		}
	}
	return ""
}
