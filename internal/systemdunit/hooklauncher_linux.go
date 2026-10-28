//go:build linux

package systemdunit

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// hookLauncherProgram is the binary that creates a hook scope. Matching on it as
// well as on the --unit= value is what makes the identity strong: an ordinary
// process that merely MENTIONS a scope name — an operator's `grep`, a shell
// history line, the hook script itself — is not a launcher, and adopting one
// would wedge every rebuild of that session behind a process that will never
// register anything.
const hookLauncherProgram = "systemd-run"

// RunningHookLaunchers reports the live launchers that are about to register a
// scope under one of prefixes.
//
// It is the second oracle beside RunningHookScopes, and it exists because the
// first one's silence proves nothing on its own: between systemd-run's execve
// and its StartTransientUnit reply the unit does not exist, and the daemon unit
// is KillMode=process, so a daemon that dies in that interval leaves the
// launcher alive and unscoped. A successor that read the empty unit list as
// "no hook survives" would rebuild or relocate the tree, and the launcher would
// then exec the operator's command with the pre-move cwd — #2770 across the
// restart boundary (#3667).
//
// The prefix carries the session id AND the daemon generation that minted it,
// so a match is a strong claim of identity rather than a guess about which
// reparented processes are ours: nothing but this session's hook run ever names
// that prefix. A launcher for a DIFFERENT prefix is another session's business
// and is never adopted.
//
// A process table that cannot be read is an error, never an empty list. Callers
// on a path that is about to touch the tree must fail closed on it, exactly as
// they already do for an unreachable user manager.
func RunningHookLaunchers(prefixes ...string) ([]HookLauncher, error) {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil, nil
	}
	matched, err := proctree.ProcessesMatchingArgv(func(argv []string) bool {
		unit, ok := hookLauncherUnit(argv)
		return ok && hasHookPrefix(unit, present)
	})
	if err != nil {
		return nil, fmt.Errorf(
			"cannot read the process table to look for hook launchers under %s: %w",
			strings.Join(present, ", "), err)
	}
	launchers := make([]HookLauncher, 0, len(matched))
	for _, process := range matched {
		unit, _ := hookLauncherUnit(process.Argv)
		launchers = append(launchers, HookLauncher{PID: process.Process.PID, Unit: unit})
	}
	return launchers, nil
}

// hookLauncherUnit reports the scope a systemd-run argv is about to register,
// and whether the argv is a launcher at all.
//
// It stops at systemd-run's own "--" separator on purpose. Everything past it is
// the COMMAND being scoped, so a hook whose script happens to contain the text
// "--unit=af-hook-…" — a test, a doc example, an af invocation of its own — is
// read as what it is rather than adopted as a launcher for that scope.
func hookLauncherUnit(argv []string) (string, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != hookLauncherProgram {
		return "", false
	}
	for i := 1; i < len(argv); i++ {
		switch arg := argv[i]; {
		case arg == "--":
			return "", false
		case strings.HasPrefix(arg, "--unit="):
			return strings.TrimPrefix(arg, "--unit="), true
		case arg == "--unit" && i+1 < len(argv):
			return argv[i+1], true
		}
	}
	return "", false
}

// hasHookPrefix reports whether a scope name belongs to one of the prefixes.
// The trailing "-" is load-bearing: unit names are "<prefix>-<generation>-<n>",
// so a bare HasPrefix would let "af-hook-s1x" match the prefix "af-hook-s1".
func hasHookPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}
	return false
}
