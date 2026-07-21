package tmuxguard

import (
	"path/filepath"
	"strings"
)

// commandDispatch is a property of the selected program, not of the shell
// spelling that selected it. It supports a best-effort policy, not a complete
// program-semantics proof: file contents, inherited configuration, plugins,
// and future executable behavior remain outside this model. Unknown programs
// deny to make accidental bypasses harder; host containment remains necessary.
type commandDispatch uint8

const (
	dispatchOpaque commandDispatch = iota
	dispatchNone
	dispatchAudited
	dispatchTrusted
)

type programRole uint8

const (
	roleOpaque programRole = iota
	roleData
	roleTrusted
	roleShell
	roleCommand
	roleEnv
	roleFind
	roleTimeout
	roleTmux
	rolePatternKill
	rolePIDKill
	rolePrintf
	roleTest
	roleGit
	roleDocker
	roleRipgrep
	roleSort
	roleGitHub
	roleGo
	rolePython
	roleSed
	roleJournalctl
	roleMake
	roleFile
)

type programPolicy struct {
	dispatch       commandDispatch
	role           programRole
	directoryInert bool
}

func classifyProgram(executable string) programPolicy {
	name, trustedPath := classifiedProgramName(executable)
	if !trustedPath {
		return programPolicy{dispatch: dispatchOpaque, role: roleOpaque}
	}
	switch name {
	case ":", "agent-browser", "basename", "cat", "cd", "chmod", "cmp", "comm", "cp", "curl",
		"cut", "date", "df", "diff", "dirname", "du", "echo", "false", "gofmt",
		"grep", "head", "hostname", "id", "jq", "ln", "ls", "mkdir", "mktemp", "mv",
		"paste", "pgrep", "printenv", "ps", "pwd", "readlink", "realpath", "rm", "rmdir",
		"seq", "sha256sum", "shellcheck", "sleep", "ss", "stat", "strings", "tail",
		"tee", "touch", "tr", "true", "uniq", "uptime", "wc", "which":
		return programPolicy{dispatch: dispatchNone, role: roleData, directoryInert: true}
	case "af":
		return programPolicy{dispatch: dispatchTrusted, role: roleTrusted}
	case "ash", "bash", "dash", "fish", "ksh", "mksh", "sh", "yash", "zsh":
		return programPolicy{dispatch: dispatchAudited, role: roleShell}
	case "command":
		return programPolicy{dispatch: dispatchAudited, role: roleCommand}
	case "env":
		return programPolicy{dispatch: dispatchAudited, role: roleEnv}
	case "find":
		return programPolicy{dispatch: dispatchAudited, role: roleFind, directoryInert: true}
	case "timeout":
		return programPolicy{dispatch: dispatchAudited, role: roleTimeout}
	case "tmux":
		return programPolicy{dispatch: dispatchAudited, role: roleTmux}
	case "killall", "pkill":
		return programPolicy{dispatch: dispatchAudited, role: rolePatternKill, directoryInert: true}
	case "kill":
		return programPolicy{dispatch: dispatchAudited, role: rolePIDKill, directoryInert: true}
	case "printf":
		return programPolicy{dispatch: dispatchAudited, role: rolePrintf, directoryInert: true}
	case "[", "test":
		return programPolicy{dispatch: dispatchAudited, role: roleTest, directoryInert: true}
	case "git":
		return programPolicy{dispatch: dispatchAudited, role: roleGit}
	case "docker":
		return programPolicy{dispatch: dispatchAudited, role: roleDocker}
	case "rg":
		return programPolicy{dispatch: dispatchAudited, role: roleRipgrep, directoryInert: true}
	case "sort":
		return programPolicy{dispatch: dispatchAudited, role: roleSort, directoryInert: true}
	case "gh":
		return programPolicy{dispatch: dispatchAudited, role: roleGitHub}
	case "go":
		return programPolicy{dispatch: dispatchAudited, role: roleGo}
	case "python", "python2", "python3", "pypy", "pypy3":
		return programPolicy{dispatch: dispatchAudited, role: rolePython}
	case "sed":
		return programPolicy{dispatch: dispatchAudited, role: roleSed, directoryInert: true}
	case "journalctl":
		return programPolicy{dispatch: dispatchAudited, role: roleJournalctl}
	case "make":
		return programPolicy{dispatch: dispatchAudited, role: roleMake}
	case "file":
		return programPolicy{dispatch: dispatchAudited, role: roleFile, directoryInert: true}
	default:
		if pythonVersionedExecutable(name) {
			return programPolicy{dispatch: dispatchAudited, role: rolePython}
		}
		// This includes gcloud, make, interpreters, task runners, and any
		// future executable. New programs must be classified deliberately;
		// they never inherit an allow path.
		return programPolicy{dispatch: dispatchOpaque, role: roleOpaque}
	}
}

func classifiedProgramName(executable string) (string, bool) {
	name := strings.ToLower(filepath.Base(executable))
	if !strings.ContainsRune(executable, filepath.Separator) {
		return name, true
	}
	cleaned := filepath.Clean(executable)
	for _, root := range []string{"/bin", "/usr/bin", "/usr/local/bin", "/usr/local/go/bin"} {
		if filepath.Dir(cleaned) == root {
			return name, true
		}
	}
	return name, false
}
