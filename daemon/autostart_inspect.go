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
	// Enabled is systemd's is-enabled; AnswerYes on macOS, where RunAtLoad in
	// the plist plays that role.
	Enabled ProbeAnswer
	// Active reports whether the service manager is currently RUNNING the
	// daemon — systemd is-active, or a launchd job with a live process.
	Active ProbeAnswer
	// Loaded reports that launchd knows the job in Domain. Loaded without
	// Active is a real and quiet state: the agent is installed and registered,
	// so everything looks configured, while no daemon process is running.
	// Undetermined on Linux, where is-active answers the question directly.
	Loaded ProbeAnswer
	// Domain is the launchd domain target the restart path uses (gui/<uid>);
	// empty on Linux.
	Domain string
	// Err records why the installed unit could not be inspected (an unreadable
	// unit file). The unit is present, so this is a problem to report, never a
	// reason to skip the check.
	Err error
	// LoadedElsewhere reports a launchd agent loaded in some domain other than
	// Domain, so `launchctl kickstart -k gui/<uid>/...` restarts cannot reach
	// it (#1044).
	LoadedElsewhere ProbeAnswer
	// Detail carries the service manager's own words for the report.
	Detail string
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
		enabled, enabledWord := systemdAsk("is-enabled")
		info.Enabled = enabled
		if cause := info.Enabled.Cause(); cause != nil {
			// systemd could not be reached at all, so is-active would fail the
			// same way: asking again buys a second doomed subprocess and repeats
			// one cause. Active stays the zero value — Undetermined — by
			// construction.
			info.Active = Undetermined(cause)
			info.Detail = "is-enabled=unknown is-active=unknown"
			return info
		}
		active, activeWord := systemdAsk("is-active")
		info.Active = active
		// The manager's own words, not our answer names: "failed" and
		// "inactive" are both No, and the difference is the user's to see.
		info.Detail = fmt.Sprintf("is-enabled=%s is-active=%s", enabledWord, activeWord)
	case "darwin":
		info.Enabled = AnswerYes() // RunAtLoad; launchd has no separate enable state
		info.Domain = launchdDomainTarget()

		print := autostartProbeCommand("launchctl", "print", info.Domain)
		out, ok := print.Output()
		switch {
		case !ok:
			// launchctl never answered — missing, killed on the deadline, or
			// wedged. "Not loaded" is a claim we have not earned.
			cause := launchdProbeErr("print "+info.Domain, print)
			info.Loaded, info.LoadedElsewhere, info.Active = Undetermined(cause), Undetermined(cause), Undetermined(cause)
			info.Detail = "could not query launchd"
		case print.Succeeded():
			// print succeeding means launchd KNOWS the job, not that the daemon
			// is alive: it reports the service's properties, including a state
			// and last exit status, for a loaded job whose process has stopped.
			info.Loaded, info.LoadedElsewhere = AnswerYes(), AnswerNo()
			if launchdJobRunning(out) {
				info.Active = AnswerYes()
				info.Detail = "loaded and running in " + info.Domain
			} else {
				info.Active = AnswerNo()
				info.Detail = "loaded in " + info.Domain + " but no daemon process is running"
			}
		case launchdSaysNotFound(out, print.ExitCode()):
			// launchd answered: no such job in the domain af's restarts target.
			// That is "not loaded HERE" — plain No, not NotFound: launchd has no
			// "known but unloaded" state for NotFound to mean, unlike systemd,
			// where a unit file can exist that systemd has never loaded.
			// A legacy-domain load still answers the domain-agnostic list.
			info.Loaded = AnswerNo()
			info.Active, info.LoadedElsewhere, info.Detail = launchdListElsewhere(info.Domain)
		default:
			// It ran and failed for some other reason (permission denied, bad
			// request). That is not a not-found answer.
			cause := launchdProbeErr("print "+info.Domain, print)
			info.Loaded, info.LoadedElsewhere, info.Active = Undetermined(cause), Undetermined(cause), Undetermined(cause)
			info.Detail = "could not query launchd"
		}
	}
	return info
}

// launchdListElsewhere asks the domain-agnostic `launchctl list` whether the job
// is loaded in some OTHER domain, and reports (active, loadedElsewhere, detail).
func launchdListElsewhere(domain string) (active, elsewhere ProbeAnswer, detail string) {
	list := autostartProbeCommand("launchctl", "list", autostartLaunchdLabel)
	out, ok := list.Output()
	switch {
	case !ok:
		cause := launchdProbeErr("list "+autostartLaunchdLabel, list)
		return Undetermined(cause), Undetermined(cause), "could not query launchd"
	case list.Succeeded():
		// Loaded somewhere else. Whether that domain is RUNNING it is not
		// something `list` establishes, so Active stays undetermined rather than
		// claiming a liveness we never observed.
		return Undetermined(errors.New("the job is loaded in another domain, whose liveness launchctl list does not report")),
			AnswerYes(), "loaded outside " + domain
	case launchdSaysNotFound(out, list.ExitCode()):
		return AnswerNo(), AnswerNo(), "not loaded"
	default:
		cause := launchdProbeErr("list "+autostartLaunchdLabel, list)
		return Undetermined(cause), Undetermined(cause), "could not query launchd"
	}
}

// systemdAsk runs one `systemctl --user <verb>` and turns it into an answer.
//
// Every branch here is reachable only from a probe that COMPLETED: the result
// type does not expose the output of one that did not, so a timeout cannot be
// classified as a state at all — it leaves through the first branch, as
// Undetermined, carrying its cause.
func systemdAsk(verb string) (ProbeAnswer, string) {
	res := autostartProbeCommand("systemctl", "--user", verb, autostartUnitName)
	out, ok := res.Output()
	if !ok {
		// The state renders as "unknown", never as whatever a dying probe
		// managed to print: that text is a cause, not a state.
		return Undetermined(fmt.Errorf("could not query systemd (%s): %w", verb, res.Cause())), "unknown"
	}
	word := firstLine(strings.TrimSpace(out))
	switch {
	case res.Succeeded():
		return AnswerYes(), word
	case res.ExitCode() == systemdNoSuchUnitExit, systemdNotFoundWords[word]:
		// systemd answered that it has no such unit. Definite, and the one that
		// matters most here: the unit FILE exists (we only probe when it does),
		// so systemd not knowing it means it was never loaded — a real, fixable
		// state that neither "inactive" nor "unknown" would have told anyone.
		return AnswerNotFound(), "not-found"
	case systemdStateWords[word]:
		// A recognized state word on a non-zero exit is a real negative answer
		// ("inactive", "disabled"). The word itself goes to the report: "failed"
		// and "inactive" are both No, and a user needs to know which.
		return AnswerNo(), word
	default:
		// It ran, exited non-zero, and printed something that is not a state —
		// "Failed to connect to bus: No medium found". The question never
		// reached systemd, so there is no answer to report.
		return Undetermined(systemdProbeErr(verb, out)), "unknown"
	}
}

func systemdProbeErr(verb, out string) error {
	if text := strings.TrimSpace(out); text != "" {
		return fmt.Errorf("could not query systemd (%s): %s", verb, firstLine(text))
	}
	return fmt.Errorf("could not query systemd (%s): it exited non-zero without a state", verb)
}

// systemdStateWords are the words systemctl prints when it has actually
// answered is-enabled/is-active in the NEGATIVE. Anything else on a non-zero
// exit ("Failed to connect to bus...") means the question never reached systemd;
// "there is no such unit" is a different fact again and lives in
// systemdNotFoundWords.
var systemdStateWords = map[string]bool{
	// is-active
	"inactive": true, "activating": true, "deactivating": true,
	"failed": true, "reloading": true, "maintenance": true,
	// is-enabled
	"disabled": true, "masked": true, "masked-runtime": true,
	"static": true, "indirect": true, "linked": true,
	"linked-runtime": true, "generated": true, "transient": true,
	"enabled-runtime": true,
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
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

// launchdNotFoundExit is launchctl's exit code for a service target it has no
// record of.
const launchdNotFoundExit = 113

// launchdSaysNotFound reports whether launchctl actually answered "no such
// service", as opposed to failing for any other reason.
//
// Only a recognized not-found answer counts. A non-zero exit alone does not: a
// permission failure or a bad request also exits non-zero, and reading those as
// "not loaded" is the same conflation this file exists to remove. It takes an
// exit code rather than an error because probeResult has already established
// that the command completed — a probe that did not complete never reaches here.
func launchdSaysNotFound(out string, exitCode int) bool {
	if exitCode == launchdNotFoundExit {
		return true
	}
	lower := strings.ToLower(out)
	return strings.Contains(lower, "could not find service") ||
		strings.Contains(lower, "no such process")
}

// launchdProbeErr describes a launchctl query that could not be answered,
// naming the cause once.
func launchdProbeErr(what string, res probeResult) error {
	if cause := res.Cause(); cause != nil {
		return fmt.Errorf("could not query launchd (%s): %w", what, cause)
	}
	if text, _ := res.Output(); strings.TrimSpace(text) != "" {
		return fmt.Errorf("could not query launchd (%s): %s", what, firstLine(strings.TrimSpace(text)))
	}
	return fmt.Errorf("could not query launchd (%s): it exited %d without an answer", what, res.ExitCode())
}

// systemdNoSuchUnitExit is systemctl's exit code for a unit it has no record of.
const systemdNoSuchUnitExit = 4

// systemdNotFoundWords are what systemd prints when it knows of no such unit —
// a DEFINITE answer, not an error, and not the same as "inactive".
var systemdNotFoundWords = map[string]bool{
	"not-found": true, // is-enabled
	"unknown":   true, // is-active
}

// probeResult is the outcome of running one bounded service-manager command.
//
// The output of a command that did NOT complete is deliberately unreachable:
// Output returns ok=false and an EMPTY string. That is the structural half of
// the fix, and the half that ordering could not buy. A classifier handed
// (output, error) can read a state word out of a timed-out probe's partial
// output and call it an answer — which is exactly the mistake that survived two
// rounds of patching here, because the data that enables it was reachable. Now
// it is not: an incomplete probe has nothing to classify, so the only answer
// that can be built from one is Undetermined.
type probeResult struct {
	completed bool
	output    string
	exitCode  int
	cause     error
}

// Output returns what the manager printed, and true, ONLY if the command ran to
// completion. A probe that never completed has no output to interpret.
func (r probeResult) Output() (string, bool) {
	if !r.completed {
		return "", false
	}
	return r.output, true
}

// Succeeded reports a completed command that exited 0.
func (r probeResult) Succeeded() bool { return r.completed && r.exitCode == 0 }

// ExitCode is meaningful only when the command completed.
func (r probeResult) ExitCode() int { return r.exitCode }

// Cause is why the command did not complete; nil when it did.
func (r probeResult) Cause() error { return r.cause }

// autostartProbeCommand runs one bounded, read-only service-manager query.
// Injected in tests; production never reassigns.
//
// Kept separate from autostartUnitCommand on purpose: that one also runs the
// MUTATING install/restart verbs (`systemctl enable --now`, `launchctl load`),
// which are legitimately slower and are not what this deadline is for.
var autostartProbeCommand = runAutostartProbeCommand

func runAutostartProbeCommand(name string, args ...string) probeResult {
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

	// Killed on the deadline: whatever it managed to print is not an answer.
	if ctx.Err() != nil {
		return probeResult{cause: fmt.Errorf("%s %s timed out after %s: %w",
			name, strings.Join(args, " "), autostartProbeTimeout, ctx.Err())}
	}
	// Never ran, or died on a signal: also not an answer. Only an ExitError
	// means the command itself decided how to end.
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
		// ErrWaitDelay: the command finished and its answer is in out; only a
		// straggler held the pipe, and the reap above killed it (#676/#914).
		return probeResult{completed: true, output: string(out), exitCode: 0}
	case errors.As(err, &exitErr):
		if exitErr.ExitCode() < 0 {
			return probeResult{cause: fmt.Errorf("%s %s was killed before answering: %w",
				name, strings.Join(args, " "), err)}
		}
		return probeResult{completed: true, output: string(out), exitCode: exitErr.ExitCode()}
	default:
		return probeResult{cause: fmt.Errorf("could not run %s %s: %w",
			name, strings.Join(args, " "), err)}
	}
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
