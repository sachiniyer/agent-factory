//go:build linux

package systemdunit

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BoundChildStopTimeout is shorter than the unit's RestartSec, but the
// correctness barrier does not depend on that gap: BindsTo+After puts stopping
// the old scope and restarting its owner in one ordered transaction. The bound
// keeps a TERM-ignoring child from delaying recovery for systemd's default 90s.
const BoundChildStopTimeout = "4s"

const systemdRunNoExpandFlag = "--expand-environment=no"

// systemdRunNoExpandOption is feature-detected because environment expansion
// and the switch that disables it were both added in systemd 254. Ubuntu 22.04
// predates the option, while newer releases may rewrite ${NAME} in every child
// argument unless it is explicitly disabled. Cache the answer for the process's
// lifetime; if help itself fails, pass the option and let the spawn fail closed
// instead of running a potentially rewritten command.
var systemdRunNoExpandOption = sync.OnceValue(func() string {
	help, err := exec.Command("systemd-run", "--help").Output()
	if err != nil || bytes.Contains(help, []byte("--expand-environment=")) {
		return systemdRunNoExpandFlag
	}
	return ""
})

func scopePreamble() []string {
	args := []string{"--user", "--scope", "--quiet", "--collect"}
	if option := systemdRunNoExpandOption(); option != "" {
		args = append(args, option)
	}
	return args
}

// NewBoundChildCommand places a long-lived daemon-owned process tree in a
// transient scope bound to the daemon service. The service itself intentionally
// uses KillMode=process so legacy tmux servers trapped in its cgroup survive an
// upgrade. A separate bound scope gives systemd authoritative ownership of
// watchers/editors without putting those unrelated lifetime classes back in one
// kill domain.
//
// BindsTo stops the scope when the daemon unit becomes inactive after a panic,
// SIGKILL, or OOM. After supplies both halves of the ordering guarantee: the
// child cannot start before its owner, and on restart the old child scope is
// fully stopped before the replacement daemon may start. There is therefore no
// startup reconciliation window in which two watcher/editor generations run.
//
// This shape is for children whose lifetime IS the daemon's. It is the wrong
// shape for a one-shot provisioning hook — see NewUnboundScopeCommand.
func NewBoundChildCommand(name string, args ...string) *exec.Cmd {
	return newBoundChildCommandForUnit(DaemonUnitName, name, args...)
}

// newBoundChildCommandForUnit is NewBoundChildCommand with the owning unit as a
// parameter, so the bound shape's defining property — the owner going inactive
// takes the scope with it — is testable against a throwaway stand-in unit
// instead of the installed daemon service.
func newBoundChildCommandForUnit(unit, name string, args ...string) *exec.Cmd {
	if !RunningDaemonProcess() {
		return exec.Command(name, args...)
	}
	scopeArgs := append(scopePreamble(),
		"--property=BindsTo="+unit,
		"--property=After="+unit,
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec="+BoundChildStopTimeout,
		"--", name,
	)
	return exec.Command("systemd-run", append(scopeArgs, args...)...)
}

// NewUnboundScopeCommand places a one-shot, operator-authored process tree in a
// named transient scope with NO dependency edge to the daemon unit at all — no
// BindsTo, no After, no PartOf. It is the shape session/tmux/server_scope_linux.go
// already uses for the shared tmux server (#2185), picked here for the same
// reason: the scope is a SIBLING of agent-factory-daemon.service under app.slice,
// so it buys two properties at once.
//
//  1. Off the daemon's books. Nothing the hook allocates is charged to the daemon
//     unit, so MemoryPeak is a daemon number again and a future MemoryMax= on the
//     unit constrains the daemon rather than the operator's build (#3650, #3625).
//  2. Survives the daemon. Stopping, restarting or auto-upgrading the daemon does
//     not stop the scope, so a `make dev_install` that post_worktree_commands
//     started keeps running. The bound shape above would kill it mid-pnpm on
//     every restart, which is the trade this deliberately does not make.
//
// unit is a derived name rather than systemd's random run-r<hex> precisely so it
// is a DURABLE HANDLE: a later daemon generation, which has no pgid and no
// cmd.Wait for a survivor, can still name and stop it. See HookScopeUnitPrefix.
// HookScopeStopTimeout bounds how long systemd waits after SIGTERM before it
// SIGKILLs a hook scope that is being stopped. It exists because the default is
// DefaultTimeoutStopSec — measured at 1min 30s on this box's user manager — and
// the survivor sweep in session/git FAILS CLOSED on a stop it cannot complete.
// Left at the default, a single TERM-ignoring hook would make af refuse to
// rebuild the worktree for a minute and a half and then succeed on retry, rather
// than kill the survivor it is entitled to kill. It is not a dependency edge and
// does not affect survival: nothing stops the scope when the daemon stops.
const HookScopeStopTimeout = "10s"

func NewUnboundScopeCommand(unit, name string, args ...string) *exec.Cmd {
	program, argv := UnboundScopeArgv(unit, name, args...)
	return exec.Command(program, argv...)
}

// UnboundScopeArgv is NewUnboundScopeCommand's argv, for the one caller that
// must build its command through exec.CommandContext — a context cannot be
// attached to an *exec.Cmd after the fact.
func UnboundScopeArgv(unit, name string, args ...string) (string, []string) {
	if !RunningDaemonProcess() || strings.TrimSpace(unit) == "" {
		return name, args
	}
	scopeArgs := append(scopePreamble(),
		"--unit="+unit,
		"--property=TimeoutStopSec="+HookScopeStopTimeout,
		"--", name,
	)
	return "systemd-run", append(scopeArgs, args...)
}

// hookScopeUnitRoot prefixes every scope this package names for a hook, so an
// operator reading `systemctl --user list-units` can tell at a glance which
// scopes belong to Agent Factory's post-worktree and archive hooks.
const hookScopeUnitRoot = "af-hook"

// HookScopeUnitPrefix derives the durable handle recorded alongside a session.
// Every scope a hook run enters is named "<prefix>-<generation>-<index>.scope",
// so the prefix alone names every generation's scopes for that session — which
// is what makes a survivor findable by a daemon that did not start it.
//
// It is a PREFIX and not one exact unit name on purpose. post_worktree_commands
// run sequentially, each in its own scope, so any single recorded name is stale
// the moment the next command starts; a daemon killed in that window would have
// persisted a handle to a scope that has already exited while the live one went
// unrecorded. The prefix is correct at every instant of the run.
//
// An empty sessionID yields an empty prefix, which every caller reads as "no
// scope" — that is the TUI/CLI path and stays byte-for-byte as it is today.
func HookScopeUnitPrefix(sessionID string) string {
	sanitized := sanitizeUnitComponent(sessionID)
	if sanitized == "" {
		return ""
	}
	return hookScopeUnitRoot + "-" + sanitized
}

// NewHookScopeGeneration returns a token distinguishing one hook RUN from every
// other run of the same session, including runs by a previous daemon generation
// whose scopes may still be alive. systemd-run fails outright on a name that is
// already taken, so a colliding generation would not silently share a scope — it
// would fail the hook, which is why this is time-derived rather than a counter
// that restarts at zero with the process.
func NewHookScopeGeneration() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// HookScopeUnit builds the scope unit name for one command of one hook run.
func HookScopeUnit(prefix, generation string, index int) string {
	if prefix == "" {
		return ""
	}
	return fmt.Sprintf("%s-%s-%d.scope", prefix, sanitizeUnitComponent(generation), index)
}

// sanitizeUnitComponent maps an identifier onto the character set systemd
// accepts in a unit name without escaping ("A"-"Z", "a"-"z", "0"-"9", ":", "-",
// "_", "."). Session ids are already hex-and-dashes; this exists so a future id
// format cannot silently produce an unrunnable unit name.
func sanitizeUnitComponent(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ':':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
