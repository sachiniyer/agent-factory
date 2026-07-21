package daemon

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"strconv"
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
	// MainPID is the live process owned by the service manager. It is meaningful
	// only when MainPIDPresent is Yes. Keeping the answer separate preserves the
	// difference between pid 0 and a query that never completed.
	MainPID        int
	MainPIDPresent ProbeAnswer
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
		enabled, enabledWord := systemdAsk(systemdIsEnabled)
		info.Enabled = enabled
		if cause := info.Enabled.Cause(); errors.Is(cause, errSystemdUnreachable) {
			// systemd could not be reached AT ALL, so is-active would fail the
			// same way: asking again buys a second doomed subprocess (a second
			// full timeout, if it is hanging) and repeats one cause.
			//
			// Gated on unreachability specifically, NOT on "Enabled is
			// undetermined": systemd answering "alias" is undetermined for our
			// question while systemd is perfectly reachable, and skipping
			// is-active there would throw away an answer we could have had.
			info.Active = Undetermined(cause)
			info.Detail = "is-enabled=unknown is-active=unknown"
			return info
		}
		active, activeWord := systemdAsk(systemdIsActive)
		info.Active = active
		active.Match(
			func() { info.MainPID, info.MainPIDPresent = systemdMainPID() },
			func() { info.MainPIDPresent = AnswerNo() },
			func() { info.MainPIDPresent = AnswerNotFound() },
			func(cause error) { info.MainPIDPresent = Undetermined(cause) },
		)
		// The manager's own words, not our answer names: "failed" and
		// "inactive" are both No, and the difference is the user's to see.
		info.Detail = fmt.Sprintf("is-enabled=%s is-active=%s", enabledWord, activeWord)
		if info.MainPID > 0 {
			info.Detail += fmt.Sprintf(" main-pid=%d", info.MainPID)
		}
	case "darwin":
		info.Domain = launchdDomainTarget()
		info.Enabled = launchdEnabled()

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
			if pid, ok := launchdJobPID(out); ok {
				info.Active = AnswerYes()
				info.MainPID = pid
				info.MainPIDPresent = AnswerYes()
				info.Detail = "loaded and running in " + info.Domain
			} else if launchdJobRunning(out) {
				info.Active = AnswerYes()
				info.MainPIDPresent = Undetermined(errors.New("launchd reports the job running but did not report its pid"))
				info.Detail = "loaded and running in " + info.Domain
			} else {
				info.Active = AnswerNo()
				info.MainPIDPresent = AnswerNo()
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

// launchdEnabled asks launchd whether our agent is DISABLED, rather than
// assuming that a plist on disk means "starts at login".
//
// This is "exit 0 is transport, the word is the answer" one platform over: the
// FILE is transport — it says what would run — and launchd's own state is the
// answer to whether it will. A plist can sit in ~/Library/LaunchAgents while
// `launchctl disable` has overridden it, and nothing about the file says so.
// Assuming Yes here was a fabricated positive on the platform that REPORTED the
// bug this PR serves (#1947): a mac user with a disabled agent would have been
// told autostart was healthy, and their daemon would not come back after a
// reboot.
//
// The domain matters and is deliberate. InstallAutostart still uses the legacy
// `launchctl load <plist>`, while RestartAutostartUnit uses the modern
// `kickstart -k gui/<uid>/<label>`. This asks about gui/<uid> — the SAME domain
// the restart path targets — because that is the domain whose answer predicts
// whether af's own restarts and logins reach the agent. Asking a third domain
// would answer about nothing.
func launchdEnabled() ProbeAnswer {
	res := autostartProbeCommand("launchctl", "print-disabled", launchdUserDomain())
	out, ok := res.Output()
	if !ok {
		return Undetermined(launchdProbeErr("print-disabled "+launchdUserDomain(), res))
	}
	if !res.Succeeded() {
		return Undetermined(launchdProbeErr("print-disabled "+launchdUserDomain(), res))
	}
	disabled, listed := launchdDisabledFor(out, autostartLaunchdLabel)
	switch {
	case listed && disabled:
		// launchd holds an explicit override: this agent will not start.
		return AnswerNo()
	case listed:
		// Explicitly enabled.
		return AnswerYes()
	default:
		// Not in the disabled store at all. That store is an OVERRIDE list, so
		// absence means "no override" — the plist's own RunAtLoad governs, and
		// InstallAutostart writes RunAtLoad=true. This is the one inference
		// here, and it is launchd's documented default rather than a guess about
		// a file we did not read.
		return AnswerYes()
	}
}

// launchdDisabledFor reads `launchctl print-disabled` output, which prints a
// block of `"label" => true|false` entries. Reports whether the label is listed
// and, if so, whether it is disabled.
func launchdDisabledFor(out, label string) (disabled, listed bool) {
	quoted := `"` + label + `"`
	for _, line := range strings.Split(out, "\n") {
		field := strings.TrimSpace(line)
		if !strings.HasPrefix(field, quoted) {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(field[len(quoted):]))
		value = strings.TrimSpace(strings.TrimPrefix(value, "=>"))
		switch {
		case strings.HasPrefix(value, "true"):
			return true, true
		case strings.HasPrefix(value, "false"):
			return false, true
		}
	}
	return false, false
}

// launchdUserDomain is the gui/<uid> domain target — the same domain
// launchdDomainTarget's service target lives in, and the one af's restarts use.
func launchdUserDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
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
		return Undetermined(fmt.Errorf("could not query systemd (%s): %w: %w",
			verb, errSystemdUnreachable, res.Cause())), "unknown"
	}
	word := firstLine(strings.TrimSpace(out))

	// systemd's exit code is a TRANSPORT fact — "the command ran, and roughly
	// how it feels about it". The ANSWER is the word. Trusting exit 0 to mean
	// "yes" is the same mistake as trusting an error to mean "no", one layer up:
	// `systemctl is-enabled` exits 0 for static, indirect, alias, generated,
	// transient and enabled-runtime, none of which mean "this starts at login".
	// Reading exit 0 as enabled reported BROKEN autostart as healthy — and an
	// unwarned user whose daemon never comes back after a reboot is the exact
	// failure this whole feature exists to prevent.
	//
	// So the word decides, per verb (the vocabularies are disjoint: "static" is
	// meaningless for is-active, "active" for is-enabled). The exit code gets
	// exactly one job, below: systemd's not-found signal.
	if res.ExitCode() == systemdNoSuchUnitExit {
		return AnswerNotFound(), "not-found"
	}
	answers := systemdEnabledAnswers
	if verb == systemdIsActive {
		answers = systemdActiveAnswers
	}
	if answer, known := answers[word]; known {
		return answer, word
	}
	// A word systemd never documents for this verb, or a diagnostic where a
	// state belongs ("Failed to connect to bus..."). Either way we do not know
	// what it means, and guessing in EITHER direction is how this bug family
	// works.
	return Undetermined(systemdProbeErr(verb, out)), "unknown"
}

// systemdMainPID asks the same manager which answered is-active for the exact
// process it owns. The command is separately bounded by autostartProbeCommand;
// a timeout or malformed answer stays unknown and can never become pid 0.
func systemdMainPID() (int, ProbeAnswer) {
	res := autostartProbeCommand("systemctl", "--user", "show", autostartUnitName, "--property=MainPID", "--value")
	out, ok := res.Output()
	if !ok {
		return 0, Undetermined(fmt.Errorf("could not query systemd (show MainPID): %w", res.Cause()))
	}
	if !res.Succeeded() {
		return 0, Undetermined(systemdProbeErr("show MainPID", out))
	}
	word := strings.TrimSpace(firstLine(out))
	word = strings.TrimSpace(strings.TrimPrefix(word, "MainPID="))
	pid, err := strconv.Atoi(word)
	if err != nil || pid < 0 {
		return 0, Undetermined(fmt.Errorf("systemd returned an invalid MainPID %q", word))
	}
	if pid == 0 {
		return 0, AnswerNo()
	}
	return pid, AnswerYes()
}

const (
	systemdIsEnabled = "is-enabled"
	systemdIsActive  = "is-active"
)

// errSystemdAlias is why "alias" is not an answer to our question: it describes
// the NAME we asked about, not whether af starts at login. Whether the unit it
// aliases is enabled is a different query we did not make.
var errSystemdAlias = errors.New("the unit name is an alias for another unit, so its enablement is that unit's, not this one's")

// errSystemdUnreachable marks the causes where systemd itself could not be
// asked — no binary, no bus, a timeout — as opposed to an answer we simply
// cannot map to our question.
//
// The distinction is load-bearing: only the first justifies skipping the second
// probe. Treating every Undetermined as "unreachable" would let an ALIAS answer
// (systemd replying perfectly well) suppress the is-active query, which is the
// same over-reading of a signal this file keeps being about.
var errSystemdUnreachable = errors.New("systemd could not be queried")

// systemdEnabledAnswers maps every word `systemctl is-enabled` documents
// (systemctl(1)) to the answer it gives AF's actual question: WILL THE DAEMON
// START AT LOGIN?
//
// That question is narrower than "did systemctl exit 0", and the difference is
// the bug: six of these words exit 0 while meaning "no".
var systemdEnabledAnswers = map[string]ProbeAnswer{
	// The one word that means what we are asking. Exits 0.
	"enabled": AnswerYes(),

	// Exit 0, and NOT what we are asking:
	"enabled-runtime": AnswerNo(), // enabled only in /run — gone after a reboot
	"static":          AnswerNo(), // no [Install] section; cannot be enabled
	"indirect":        AnswerNo(), // not enabled itself; only triggered by another
	"generated":       AnswerNo(), // conjured by a generator, not a persistent install
	"transient":       AnswerNo(), // created at runtime; will not survive a reboot
	"alias":           Undetermined(errSystemdAlias),

	// Exit non-zero, and definite:
	"linked":         AnswerNo(), // symlinked into place but not enabled
	"linked-runtime": AnswerNo(),
	"masked":         AnswerNo(), // disabled entirely
	"masked-runtime": AnswerNo(),
	"disabled":       AnswerNo(),
	"bad":            AnswerNo(), // the unit file is invalid, so nothing starts it

	"not-found": AnswerNotFound(),
}

// systemdActiveAnswers maps every word `systemctl is-active` documents to the
// answer for: IS THE DAEMON RUNNING NOW?
var systemdActiveAnswers = map[string]ProbeAnswer{
	// Running. Both exit 0.
	"active":    AnswerYes(),
	"reloading": AnswerYes(), // running; re-reading its config

	// Not running, definitively. Exit non-zero.
	"inactive":     AnswerNo(),
	"failed":       AnswerNo(),
	"activating":   AnswerNo(), // not up YET — a real answer, and not "up"
	"deactivating": AnswerNo(),
	"maintenance":  AnswerNo(),

	// systemd's word for "I have no record of this unit".
	"unknown": AnswerNotFound(),
}

func systemdProbeErr(verb, out string) error {
	if text := strings.TrimSpace(out); text != "" {
		return fmt.Errorf("could not query systemd (%s): %s", verb, firstLine(text))
	}
	return fmt.Errorf("could not query systemd (%s): it exited non-zero without a state", verb)
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

// launchdJobPID extracts the live pid from the same `launchctl print` block
// used for Active, so ownership never depends on a second racy manager query.
func launchdJobPID(out string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		field := strings.TrimSpace(line)
		raw, ok := strings.CutPrefix(field, "pid = ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(raw))
		if err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}
