package daemon

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// AutostartUnitInfo describes the autostart unit installed on this machine:
// where it lives and, crucially, which af binary it actually launches.
// `af doctor` compares ExecPath against the running client's own binary to
// catch the split-brain install (#1044) — an upgrade that lands on one path
// while the supervised daemon keeps respawning from another, stranding an old
// daemon against new clients forever.
type AutostartUnitInfo struct {
	// Supported is false on platforms with no autostart integration.
	Supported bool
	// Path is the unit/plist location; empty when unsupported or unresolvable.
	Path string
	// Exists reports whether Path is present on disk.
	Exists bool
	// ExecPath is the binary the unit launches, parsed out of the unit file.
	// Empty when the unit is absent or its launch path could not be parsed.
	ExecPath string
	// Err records why Path/ExecPath could not be determined.
	Err error
}

// InspectAutostart reads the installed autostart unit and reports the binary
// it launches. Read-only: it never writes, loads, or reloads a unit.
func InspectAutostart() AutostartUnitInfo {
	var info AutostartUnitInfo
	switch autostartGOOS {
	case "linux", "darwin":
		info.Supported = true
	default:
		return info
	}

	path, err := autostartUnitFilePath()
	if err != nil {
		info.Err = err
		return info
	}
	info.Path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// The unit IS installed — we just cannot read it. Exists must stay
			// true or callers that gate on it drop both this error and every
			// check behind it, and doctor silently says nothing about a unit
			// that is right there (#1044). Absent is the only case that is
			// legitimately "no unit".
			info.Exists = true
			info.Err = err
		}
		return info
	}
	info.Exists = true

	if autostartGOOS == "linux" {
		info.ExecPath = parseSystemdExecStart(string(data))
	} else {
		info.ExecPath = parseLaunchdProgramPath(string(data))
	}
	if info.ExecPath == "" {
		info.Err = fmt.Errorf("could not parse the launch path out of %s", path)
	}
	return info
}

// parseSystemdExecStart extracts the binary path from a unit's ExecStart=
// line, reversing quoteExecStartPath. It lives beside the writer so the two
// stay in lockstep: a change to the quoting rules that skipped this function
// would make doctor report a phantom path mismatch on every machine.
func parseSystemdExecStart(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		value, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		return firstSystemdArg(strings.TrimSpace(value))
	}
	return ""
}

// firstSystemdArg returns the first shell-like token of an ExecStart= value,
// unescaping it back to a real path. Quoted values may contain spaces, so the
// closing quote — not the first space — ends the token.
func firstSystemdArg(value string) string {
	var out strings.Builder
	quoted := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '"':
			if !quoted && out.Len() > 0 {
				return out.String() // closing quote ends the token
			}
			quoted = !quoted
			if !quoted {
				return out.String()
			}
		case c == '\\' && i+1 < len(value):
			i++
			out.WriteByte(value[i])
		case c == '$' && i+1 < len(value) && value[i+1] == '$':
			i++
			out.WriteByte('$')
		case c == '%' && i+1 < len(value) && value[i+1] == '%':
			i++
			out.WriteByte('%')
		case !quoted && (c == ' ' || c == '\t'):
			if out.Len() > 0 {
				return out.String()
			}
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// parseLaunchdProgramPath extracts ProgramArguments[0] from a launchd plist,
// mirroring launchdAutostartPlist. The renderer XML-escapes the path, so the
// parsed value is unescaped back.
func parseLaunchdProgramPath(plist string) string {
	_, rest, ok := strings.Cut(plist, "<key>ProgramArguments</key>")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, "<array>")
	if !ok {
		return ""
	}
	array, _, ok := strings.Cut(rest, "</array>")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(array, "<string>")
	if !ok {
		return ""
	}
	first, _, ok := strings.Cut(rest, "</string>")
	if !ok {
		return ""
	}
	return html.UnescapeString(strings.TrimSpace(first))
}

// SupervisionInfo reports whether the installed autostart unit is actually
// supervising anything. A present-but-inert unit is the quiet failure: the
// user believes the daemon is unit-managed, so upgrades and restarts are
// expected to reach it, while in fact nothing restarts it at login.
type SupervisionInfo struct {
	// Supported is false on platforms with no autostart integration.
	Supported bool
	// UnitPresent reports whether a unit/plist file exists to supervise.
	UnitPresent bool
	// Enabled is systemd's is-enabled; ProbeYes on macOS, where RunAtLoad in
	// the plist plays that role.
	Enabled ProbeState
	// Active reports whether the service manager is currently RUNNING the
	// daemon — systemd is-active, or a launchd job with a live process.
	Active ProbeState
	// Loaded reports that launchd knows the job in Domain. Loaded without
	// Active is a real and quiet state: the agent is installed and registered,
	// so everything looks configured, while no daemon process is running.
	// ProbeUnknown on Linux, where is-active answers the question directly.
	Loaded ProbeState
	// Domain is the launchd domain target the restart path uses (gui/<uid>);
	// empty on Linux.
	Domain string
	// Err records why the installed unit could not be inspected (an unreadable
	// unit file). The unit is present, so this is a problem to report, never a
	// reason to skip the check.
	Err error
	// ProbeErr records why the service manager could not be QUERIED at all —
	// no binary, no user bus, permission denied, or a manager that never
	// replied. When set, the states above are ProbeUnknown and must render as
	// unknown; folding it into a negative is the lie this type exists to stop.
	ProbeErr error
	// LoadedElsewhere reports a launchd agent loaded in some domain other than
	// Domain, so `launchctl kickstart -k gui/<uid>/...` restarts cannot reach
	// it (#1044).
	LoadedElsewhere ProbeState
	// Detail carries the service manager's own words for the report.
	Detail string
}

// ProbeState is a service-manager answer that can be "we could not ask".
//
// Two states are not enough, and the gap is not academic. `systemctl is-active`
// exits non-zero both when it asked and the answer was "inactive", and when it
// could not ask at all — no binary, no user bus, permission denied, a wedged
// manager. Folding the second into the first tells the user their autostart is
// inert when we have no idea. That is precisely the disease this PR exists to
// detect — a silent no-op wearing the face of a real answer — so the detector
// must not have it.
type ProbeState int

const (
	// ProbeUnknown is the zero value deliberately: a state nobody managed to
	// probe reads as unknown, never as a negative.
	ProbeUnknown ProbeState = iota
	ProbeYes
	ProbeNo
)

func (s ProbeState) String() string {
	switch s {
	case ProbeYes:
		return "yes"
	case ProbeNo:
		return "no"
	default:
		return "unknown"
	}
}

// AutostartSupervision probes the service manager for the installed unit's
// state. Read-only: is-enabled/is-active/print never mutate the unit.
func AutostartSupervision() SupervisionInfo {
	var info SupervisionInfo
	unit := InspectAutostart()
	info.Supported = unit.Supported
	info.UnitPresent = unit.Exists
	info.Err = unit.Err
	if !unit.Supported || !unit.Exists {
		return info
	}
	// An unreadable unit file does not stop the service manager from being
	// asked about it: whether the unit is enabled and running is exactly what
	// the user needs to know, and systemctl/launchctl answer by unit name, not
	// by reading the file. Err rides along so the caller can report both.

	switch autostartGOOS {
	case "linux":
		enabled, enabledOut, enabledErr := systemdStateProbe("is-enabled")
		if enabledErr != nil {
			// systemd could not be reached at all, so is-active would fail the
			// same way for the same reason: asking again buys a second doomed
			// subprocess and says the same cause twice.
			info.Enabled, info.Active = ProbeUnknown, ProbeUnknown
			info.ProbeErr = enabledErr
			info.Detail = "is-enabled=unknown is-active=unknown"
			return info
		}
		active, activeOut, activeErr := systemdStateProbe("is-active")
		info.Enabled, info.Active = enabled, active
		info.ProbeErr = activeErr
		info.Detail = fmt.Sprintf("is-enabled=%s is-active=%s", enabledOut, activeOut)
	case "darwin":
		info.Enabled = ProbeYes // RunAtLoad; launchd has no separate enable state
		info.Domain = launchdDomainTarget()

		out, err := autostartProbeCommand("launchctl", "print", info.Domain)
		switch {
		case err == nil:
			// print succeeding means launchd KNOWS the job, not that the daemon
			// is alive: it reports the service's properties, including a state
			// and last exit status, for a loaded job whose process has stopped.
			// Equating the two reports a dead daemon as healthy supervision —
			// on the platform where that failure was actually hit.
			info.Loaded = ProbeYes
			info.LoadedElsewhere = ProbeNo
			if launchdJobRunning(string(out)) {
				info.Active = ProbeYes
				info.Detail = "loaded and running in " + info.Domain
			} else {
				info.Active = ProbeNo
				info.Detail = "loaded in " + info.Domain + " but no daemon process is running"
			}
			return info
		case !launchdSaysNotFound(string(out), err):
			// launchctl did not answer — it is missing, it was killed on the
			// deadline, or it failed for an operational reason. "Not loaded" is
			// a claim we have not earned.
			info.ProbeErr = launchdProbeErr("print "+info.Domain, out, err)
			info.Detail = "could not query launchd"
			return info
		}

		// launchd answered: the job is not in the domain the restart path
		// targets. A legacy-domain load still answers the domain-agnostic
		// `launchctl list`, which is what distinguishes "loaded somewhere else"
		// from "not loaded at all".
		info.Loaded = ProbeNo
		listOut, listErr := autostartProbeCommand("launchctl", "list", autostartLaunchdLabel)
		switch {
		case listErr == nil:
			info.LoadedElsewhere = ProbeYes
			info.Active = ProbeUnknown // some other domain runs it; not ours to claim
			info.Detail = "loaded outside " + info.Domain
		case launchdSaysNotFound(string(listOut), listErr):
			info.LoadedElsewhere = ProbeNo
			info.Active = ProbeNo
			info.Detail = "not loaded"
		default:
			info.ProbeErr = launchdProbeErr("list "+autostartLaunchdLabel, listOut, listErr)
			info.Detail = "could not query launchd"
		}
	}
	return info
}

// systemdStateProbe runs one `systemctl --user <verb>` and classifies the
// result into an answer or an inability to ask.
//
// systemctl exits non-zero for BOTH "inactive"/"disabled" (a real answer, with
// the state on stdout) and for "I could not ask" (no user bus, no binary,
// permission denied — where what it printed is a diagnostic, not a state). The
// exit code alone cannot tell them apart, so the output must: a recognized
// state word is an answer, anything else is not.
func systemdStateProbe(verb string) (state ProbeState, raw string, err error) {
	out, runErr := autostartProbeCommand("systemctl", "--user", verb, autostartUnitName)
	raw = strings.TrimSpace(string(out))
	if runErr == nil {
		return ProbeYes, raw, nil
	}
	if systemdStateWords[firstLine(raw)] {
		return ProbeNo, raw, nil
	}
	// The state is reported as "unknown", not as whatever diagnostic systemctl
	// printed: that text is a cause, not a state, and rendering it in the state
	// slot is how a non-answer starts looking like an answer. The cause rides
	// the error — once, not twice: when the command never ran there is no output
	// to add, and appending runErr again just says it twice.
	if raw == "" {
		return ProbeUnknown, "unknown", fmt.Errorf("could not query systemd (%s): %w", verb, runErr)
	}
	return ProbeUnknown, "unknown", fmt.Errorf("could not query systemd (%s): %w: %s", verb, runErr, raw)
}

// systemdStateWords are the words systemctl prints when it has actually
// answered is-enabled/is-active. Anything else on a non-zero exit ("Failed to
// connect to bus...") means the question never reached systemd.
var systemdStateWords = map[string]bool{
	// is-active
	"active": true, "inactive": true, "activating": true, "deactivating": true,
	"failed": true, "reloading": true,
	// is-enabled
	"enabled": true, "enabled-runtime": true, "disabled": true, "masked": true,
	"masked-runtime": true, "static": true, "indirect": true, "linked": true,
	"linked-runtime": true, "generated": true, "transient": true,
	// systemd's own word for "I know of no such unit" — an answer, not an error.
	"unknown": true,
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// launchdProbeErr describes a launchctl query that could not be answered,
// naming the cause once.
func launchdProbeErr(what string, out []byte, err error) error {
	if text := strings.TrimSpace(string(out)); text != "" {
		return fmt.Errorf("could not query launchd (%s): %w: %s", what, err, text)
	}
	return fmt.Errorf("could not query launchd (%s): %w", what, err)
}

// launchdNotFoundExit is launchctl's exit code for a service target it has no
// record of.
const launchdNotFoundExit = 113

// launchdSaysNotFound reports whether launchctl actually answered "no such
// service", as opposed to failing for any other reason.
//
// Only a recognized not-found answer counts. A non-zero exit alone does not: a
// permission failure or a bad request also exits non-zero, and reading those as
// "not loaded" is the same conflation this file exists to remove.
func launchdSaysNotFound(out string, err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false // never ran, or was killed on the deadline
	}
	if exitErr.ExitCode() == launchdNotFoundExit {
		return true
	}
	lower := strings.ToLower(out)
	return strings.Contains(lower, "could not find service") ||
		strings.Contains(lower, "no such process")
}

// autostartProbeTimeout bounds each read-only service-manager query. `af doctor`
// is the command a user runs BECAUSE things are wedged, so it must not be the
// thing that hangs: a stuck user bus or a wedged launchd has to become an
// answer ("unknown") within seconds, not a stall.
//
// A var, not a const, only so tests can shorten it; production never reassigns.
var autostartProbeTimeout = 5 * time.Second

// autostartProbeWaitDelay bounds how long Wait blocks after the probe is killed
// on the deadline, before its inherited pipes are force-closed.
//
// Without it the deadline is decorative: CombinedOutput blocks until the output
// pipe closes, exec.CommandContext kills only the direct child, and any
// descendant that inherited the pipe holds it open — so the call returns when
// that descendant dies, not when the deadline fires (#1967, measured; mirrors
// gitWaitDelay/tmuxWaitDelay #856/#896).
const autostartProbeWaitDelay = 2 * time.Second

// autostartProbeCommand runs one bounded, read-only service-manager query.
// Injected in tests; production never reassigns.
//
// Kept separate from autostartUnitCommand on purpose: that one also runs the
// MUTATING install/restart verbs (`systemctl enable --now`, `launchctl load`),
// which are legitimately slower and are not what this deadline is for.
var autostartProbeCommand = runAutostartProbeCommand

func runAutostartProbeCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), autostartProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// Its own process group, so the deadline tears down the whole tree rather
	// than SIGKILLing the client and orphaning a child still holding the pipe
	// (the #610/#769/#856 precedent).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = autostartProbeWaitDelay

	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // reap stragglers
	}
	if ctx.Err() != nil {
		// A timeout is an inability to ask, and must never reach the callers'
		// "did it answer?" classification as if it were an exit status.
		return out, fmt.Errorf("%s %s timed out after %s: %w",
			name, strings.Join(args, " "), autostartProbeTimeout, ctx.Err())
	}
	return out, err
}

// launchdDomainTarget is the gui/<uid>/<label> target the restart path uses.
// Kept here so doctor reports the same domain the restart actually kickstarts.
func launchdDomainTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), autostartLaunchdLabel)
}

// launchdJobRunning reports whether a `launchctl print` block describes a job
// with a live process.
//
// Two independent signals, because either can be missing:
//   - a "pid = N" line is conclusive — launchd only prints it for a job it is
//     currently running;
//   - otherwise "state = running" answers it. Note "state = not running" also
//     contains the word "running", so this compares the whole value rather than
//     searching for a substring.
//
// Neither present means launchd knows the job but is not running it, which is
// exactly the loaded-but-dead state this exists to catch.
func launchdJobRunning(out string) bool {
	running := false
	for _, line := range strings.Split(out, "\n") {
		field := strings.TrimSpace(line)
		if pid, ok := strings.CutPrefix(field, "pid = "); ok && strings.TrimSpace(pid) != "" {
			return true
		}
		if state, ok := strings.CutPrefix(field, "state = "); ok {
			running = strings.TrimSpace(state) == "running"
		}
	}
	return running
}
