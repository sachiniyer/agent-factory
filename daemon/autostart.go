package daemon

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// The daemon autostart unit is the single OS-level unit agent-factory
// installs: a user-level systemd service on Linux or a launchd agent on
// macOS that keeps the daemon (and therefore its task scheduler and watcher
// supervisor) running across logins and reboots. Per-task timer units no
// longer exist (#782).

const (
	autostartUnitName     = "agent-factory-daemon.service"
	autostartLaunchdLabel = "com.agent-factory.daemon"
)

// Every launchctl call af makes names the gui/<uid> domain explicitly, and
// they must all name the SAME one (#1947).
//
// The legacy verbs (`launchctl load`/`unload`) take no domain: they bootstrap
// into the domain of the CALLING session, so an install run over ssh lands the
// agent in user/<uid> while an install run from a Terminal window lands it in
// gui/<uid>. RestartAutostartUnit has always kicked gui/<uid> explicitly, so a
// legacy-loaded agent could sit in a domain our own restart never targets —
// `launchctl kickstart` then fails to find the job, the upgrade path logs a
// warning nobody reads, and the stale daemon keeps serving the old binary.
// That silent no-op is the macOS half of #1947 (#1920 found the same case).
//
// The modern verbs take the domain as an argument, so install, pause, resume,
// restart, and uninstall all target gui/<uid> no matter how af was invoked.
// Bootstrapping into gui/<uid> requires a GUI login session; without one
// `bootstrap` fails loudly at install time, which is strictly better than the
// legacy path's success followed by a restart that silently never lands.
func launchdGUIDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// launchdServiceTarget is the service-target form (`gui/<uid>/<label>`) that
// bootout and kickstart address the loaded job by.
func launchdServiceTarget() string {
	return launchdGUIDomain() + "/" + autostartLaunchdLabel
}

// quoteExecStartPath quotes a path for use in an ExecStart= line.
// systemd parses ExecStart= with shell-like quoting, so surrounding the path
// in double quotes allows spaces. Internal backslashes and double quotes are
// escaped so the value remains syntactically valid.
func quoteExecStartPath(p string) string {
	escaped := strings.ReplaceAll(p, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, `$`, `$$`)
	escaped = strings.ReplaceAll(escaped, `%`, `%%`)
	return `"` + escaped + `"`
}

// formatSystemdEnvLine renders one `Environment=NAME=value` line so systemd
// parses the whole value, spaces and all.
//
// systemd tokenizes Environment= with quote-aware, C-unescaping parsing
// (systemd.exec(5)): unquoted whitespace splits a single line into several
// independent assignments, and an unquoted backslash or quote is consumed as
// an escape. So when the value contains whitespace, a quote, or a backslash,
// the entire NAME=value is wrapped in double quotes with backslashes and
// double quotes escaped C-style inside. Values with no such characters are
// emitted bare to avoid needless churn.
//
// '%' is a systemd specifier and is always doubled. '$' is left literal:
// unlike ExecStart=, Environment= performs no variable expansion (verified
// with `systemctl show` / `systemd-analyze verify`), so doubling it would
// corrupt the value into a literal "$$". Newlines and carriage returns, which
// systemd rejects in unit files, are folded to spaces.
func formatSystemdEnvLine(name, value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, `%`, `%%`)
	assignment := name + "=" + value
	if !strings.ContainsAny(value, " \t\"'\\") {
		return "Environment=" + assignment
	}
	assignment = strings.ReplaceAll(assignment, `\`, `\\`)
	assignment = strings.ReplaceAll(assignment, `"`, `\"`)
	return `Environment="` + assignment + `"`
}

// systemdAutostartUnit renders the user-level systemd service that keeps the
// daemon running. PATH and SHELL are captured at install time because the
// systemd user manager starts services with a minimal environment, and the
// daemon needs the user's real PATH to find tmux and the agent programs.
// AGENT_FACTORY_HOME is captured for the same reason when the installing
// shell has it set: without it the unit's daemon would serve the default
// home instead of the custom one (#782).
func systemdAutostartUnit(execPath, pathEnv, shellEnv, agentFactoryHome string) string {
	envLines := formatSystemdEnvLine("PATH", pathEnv) + "\n" + formatSystemdEnvLine("SHELL", shellEnv)
	if agentFactoryHome != "" {
		envLines += "\n" + formatSystemdEnvLine("AGENT_FACTORY_HOME", agentFactoryHome)
	}
	return fmt.Sprintf(`[Unit]
Description=Agent Factory daemon (task scheduler + autoyes)

[Service]
ExecStart=%s --daemon
Restart=on-failure
RestartSec=5
%s

[Install]
WantedBy=default.target
`, quoteExecStartPath(execPath), envLines)
}

// launchdAutostartPlist renders the launchd agent that keeps the daemon
// running on macOS. KeepAlive on unsuccessful exit mirrors the systemd
// Restart=on-failure behavior; logPath captures crashes that happen before
// the daemon's own logging is up. AGENT_FACTORY_HOME is captured when set,
// matching the systemd unit (#782).
//
// Unlike the systemd Environment= line (#893), no extra quoting is needed for
// values with spaces: each value is its own XML <string> element, so spaces
// are ordinary text content. html.EscapeString only needs to neutralize the
// XML metacharacters (& < > " ') that would otherwise break the markup.
func launchdAutostartPlist(execPath, pathEnv, shellEnv, agentFactoryHome, logPath string) string {
	esc := html.EscapeString
	homeEntry := ""
	if agentFactoryHome != "" {
		homeEntry = fmt.Sprintf("        <key>AGENT_FACTORY_HOME</key>\n        <string>%s</string>\n", esc(agentFactoryHome))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>--daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>SHELL</key>
        <string>%s</string>
%s    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, autostartLaunchdLabel, esc(execPath), esc(pathEnv), esc(shellEnv), homeEntry, esc(logPath), esc(logPath))
}

// InstallAutostart registers the daemon for autostart at login: a systemd
// user service on Linux, a launchd agent on macOS. Any daemon started ad hoc
// (by EnsureDaemon) is handed over to the supervised unit. Returns the path of
// the installed unit file.
//
// The invariant this function guarantees (#974): on success the unit is
// ENABLED/LOADED, never merely written. Enabling (and `--now` starting) the
// unit is the thing that makes autostart real and supervises future runs, so
// it must not be skipped. Two failure modes used to break that:
//
//   - The ad-hoc→supervised handover stop is best-effort. A StopDaemon failure
//     no longer aborts before enabling: the unit's `--now` start takes over the
//     control socket per the #796/#798 handover, and the secondary ad-hoc
//     daemon is left at worst enabled-but-inactive until the next login — far
//     better than a present-but-not-enabled unit. (Previously a stop failure
//     returned early, leaving the file on disk but never enabled.)
//   - On a hard failure of the reload/enable/load step the just-written unit
//     file is removed, so AutostartInstalled — which reports on file existence —
//     can never misreport a not-enabled unit as installed and mislead the
//     upgrade respawn path into RestartAutostartUnit on a unit that was never
//     enabled.
func InstallAutostart() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}

	switch autostartGOOS {
	case "linux":
		dir, err := autostartSystemdUserDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve systemd user directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create systemd user directory: %w", err)
		}
		unitPath := filepath.Join(dir, autostartUnitName)
		content := systemdAutostartUnit(execPath, os.Getenv("PATH"), os.Getenv("SHELL"), os.Getenv("AGENT_FACTORY_HOME"))
		if err := config.AtomicWriteFile(unitPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write unit file: %w", err)
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "daemon-reload"); err != nil {
			removeAutostartUnitFile(unitPath)
			return "", fmt.Errorf("failed to reload systemd user daemon: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		// Hand any ad-hoc daemon over to the supervised one, but never let a
		// stop failure block enabling — see the function comment (#974).
		if _, err := autostartStopDaemon(); err != nil {
			log.WarningLog.Printf("failed to stop the running daemon before enabling the autostart unit; enabling anyway: %v", err)
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "enable", "--now", autostartUnitName); err != nil {
			removeAutostartUnitFile(unitPath)
			return "", fmt.Errorf("failed to enable daemon service: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return unitPath, nil

	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve LaunchAgents directory: %w", err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
		configDir, err := config.GetConfigDir()
		if err != nil {
			return "", fmt.Errorf("failed to get config directory: %w", err)
		}
		logPath := filepath.Join(configDir, "daemon-launchd.log")
		content := launchdAutostartPlist(execPath, os.Getenv("PATH"), os.Getenv("SHELL"), os.Getenv("AGENT_FACTORY_HOME"), logPath)
		// Boot out a previous agent first so bootstrap picks up the new file.
		// Best-effort: it fails when no agent is loaded, which is the norm.
		_, _ = autostartUnitCommand("launchctl", "bootout", launchdServiceTarget())
		if err := config.AtomicWriteFile(plistPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("failed to write plist file: %w", err)
		}
		// Hand any ad-hoc daemon over to the supervised one, but never let a
		// stop failure block loading — see the function comment (#974).
		if _, err := autostartStopDaemon(); err != nil {
			log.WarningLog.Printf("failed to stop the running daemon before loading the autostart agent; loading anyway: %v", err)
		}
		if out, err := autostartUnitCommand("launchctl", "bootstrap", launchdGUIDomain(), plistPath); err != nil {
			removeAutostartUnitFile(plistPath)
			return "", fmt.Errorf("failed to bootstrap launch agent into %s: %w\n%s", launchdGUIDomain(), err, strings.TrimSpace(string(out)))
		}
		return plistPath, nil

	default:
		return "", fmt.Errorf("daemon autostart is not supported on %s (the daemon still starts automatically whenever you run af)", autostartGOOS)
	}
}

// removeAutostartUnitFile deletes a just-written autostart unit/plist after a
// later install step fails, so a hard failure never leaves a present-but-not-
// enabled file that AutostartInstalled would misreport as installed (#974).
// Best-effort: a cleanup failure is logged, not surfaced, since the original
// install error is the one the caller must act on.
func removeAutostartUnitFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to clean up autostart unit %s after a failed install: %v", path, err)
	}
}

// Injection points for tests, mirroring legacy_units.go: the GOOS the install
// and restart helpers act on, the unit-directory resolvers, the external
// command runner, and the ad-hoc daemon stop, so the install and upgrade
// respawn paths can be exercised hermetically without touching the host's real
// autostart unit, invoking systemctl/launchctl, or signaling a real daemon.
var (
	autostartGOOS            = runtime.GOOS
	autostartSystemdUserDir  = defaultSystemdUserDir
	autostartLaunchAgentsDir = defaultLaunchAgentsDir
	autostartUnitCommand     = runAutostartUnitCommand
	autostartStopDaemon      = StopDaemon
)

func runAutostartUnitCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// autostartUnitFilePath returns the path of the unit file InstallAutostart
// writes for the current platform, or "" when autostart is unsupported here.
func autostartUnitFilePath() (string, error) {
	switch autostartGOOS {
	case "linux":
		dir, err := autostartSystemdUserDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, autostartUnitName), nil
	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, autostartLaunchdLabel+".plist"), nil
	default:
		return "", nil
	}
}

// AutostartInstalled reports whether the daemon autostart unit installed by
// InstallAutostart is present on this machine.
func AutostartInstalled() bool {
	path, err := autostartUnitFilePath()
	if err != nil || path == "" {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// AutostartUnitServesHome reports whether the INSTALLED autostart unit's daemon
// would serve configDir. installed is false when no unit file is present.
//
// This is the gate every unit operation a reset performs must pass, and it
// exists because "a unit file exists" and "that unit is the one I am resetting"
// are different questions (#1916 P2). The unit bakes its AGENT_FACTORY_HOME at
// install time (InstallAutostart captures os.Getenv), so a developer's unit
// serves their real home no matter what AGENT_FACTORY_HOME the CURRENT process
// happens to carry. Gating on file existence alone means `AGENT_FACTORY_HOME=/tmp/sandbox
// af reset` stops the maintainer's real daemon — the reset of a throwaway home
// reaching out and taking down a completely unrelated one.
//
// A unit installed with NO AGENT_FACTORY_HOME serves the DEFAULT home, which is
// exactly what ConfigDirFor("") resolves — so the absent case is not "unknown",
// it is the common case and must compare equal to a default-home reset.
func AutostartUnitServesHome(configDir string) (serves bool, installed bool, err error) {
	path, err := autostartUnitFilePath()
	if err != nil {
		return false, false, err
	}
	if path == "" {
		return false, false, nil // autostart unsupported on this platform
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to read the autostart unit %s: %w", path, err)
	}

	var baked string
	switch autostartGOOS {
	case "linux":
		baked, _ = systemdUnitEnvValue(string(data), "AGENT_FACTORY_HOME")
	case "darwin":
		baked, _ = launchdPlistEnvValue(string(data), "AGENT_FACTORY_HOME")
	default:
		return false, false, nil
	}

	// baked == "" means the unit captured no AGENT_FACTORY_HOME → default home.
	unitHome, err := config.ConfigDirFor(baked)
	if err != nil {
		return false, true, fmt.Errorf("the autostart unit's AGENT_FACTORY_HOME %q is unusable: %w", baked, err)
	}
	unitCanonical, err := canonicalDir(unitHome)
	if err != nil {
		return false, true, err
	}
	wantCanonical, err := canonicalDir(configDir)
	if err != nil {
		return false, true, err
	}
	return unitCanonical == wantCanonical, true, nil
}

// systemdUnitEnvValue extracts name's value from the unit's Environment= lines.
//
// It is the exact inverse of formatSystemdEnvLine and MUST stay in lockstep
// with it — a renderer change that this does not follow silently turns a
// same-home reset into a different-home one, which is the bug the gate exists
// to prevent. TestAutostartUnitHomeRoundTrip pins the pair together.
func systemdUnitEnvValue(content, name string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
		if !ok {
			continue
		}
		// formatSystemdEnvLine wraps the WHOLE assignment in quotes when the
		// value needs it, C-escaping \ and " inside.
		if len(rest) >= 2 && strings.HasPrefix(rest, `"`) && strings.HasSuffix(rest, `"`) {
			rest = unescapeSystemdValue(rest[1 : len(rest)-1])
		}
		key, value, found := strings.Cut(rest, "=")
		if !found || key != name {
			continue
		}
		// '%' is doubled unconditionally by the renderer (it is a systemd
		// specifier), and is not backslash-escaped, so it un-doubles last.
		return strings.ReplaceAll(value, "%%", "%"), true
	}
	return "", false
}

// unescapeSystemdValue reverses the C-style \\ and \" escaping applied inside a
// quoted Environment= assignment. It walks the string once rather than running
// two ReplaceAll passes, which would mis-handle a value containing a literal
// backslash followed by a quote.
func unescapeSystemdValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// launchdPlistEnvValue extracts name's value from the plist's
// EnvironmentVariables dict: the <string> immediately following <key>name</key>.
// The inverse of launchdAutostartPlist's html.EscapeString; kept in lockstep by
// the same round-trip test.
func launchdPlistEnvValue(content, name string) (string, bool) {
	idx := strings.Index(content, "<key>"+name+"</key>")
	if idx < 0 {
		return "", false
	}
	rest := content[idx:]
	start := strings.Index(rest, "<string>")
	if start < 0 {
		return "", false
	}
	rest = rest[start+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return "", false
	}
	return html.UnescapeString(rest[:end]), true
}

// AutostartUnitExecPath returns the binary path the INSTALLED autostart unit
// launches. installed is false when no unit file is present.
//
// The unit bakes its program path at install time (InstallAutostart captures
// os.Executable), exactly as it bakes AGENT_FACTORY_HOME — and for the upgrade
// path that is a trap. `af upgrade` overwrites the binary it is CURRENTLY
// running; restarting the unit then relaunches whatever path the unit was
// installed with. With two installs on one box (Homebrew /opt/homebrew/bin/af
// and ~/.local/bin/af), a restart faithfully brings the OLD image back up while
// the upgrade reports success — the stale daemon of #1947. Callers compare this
// against the binary they just wrote and say so when they disagree.
func AutostartUnitExecPath() (execPath string, installed bool, err error) {
	path, err := autostartUnitFilePath()
	if err != nil {
		return "", false, err
	}
	if path == "" {
		return "", false, nil // autostart unsupported on this platform
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to read the autostart unit %s: %w", path, err)
	}

	var (
		baked string
		found bool
	)
	switch autostartGOOS {
	case "linux":
		baked, found = systemdUnitExecStart(string(data))
	case "darwin":
		baked, found = launchdPlistProgramPath(string(data))
	default:
		return "", false, nil
	}
	if !found || baked == "" {
		return "", true, fmt.Errorf("the autostart unit %s does not name a program to launch", path)
	}
	return baked, true, nil
}

// systemdUnitExecStart extracts the program path from the unit's ExecStart=
// line. The exact inverse of quoteExecStartPath, and must stay in lockstep with
// it — TestAutostartUnitExecPathRoundTrip pins the pair together.
func systemdUnitExecStart(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		// The renderer always double-quotes the path, so scan to the matching
		// UNESCAPED quote: cutting at the first space would truncate a path
		// containing one, and cutting at the first quote would truncate a path
		// containing an escaped quote.
		if !strings.HasPrefix(rest, `"`) {
			// Defensive: a hand-edited unit may leave the path bare. Only then
			// is a space a field separator.
			field, _, _ := strings.Cut(rest, " ")
			return field, field != ""
		}
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				return unquoteExecStartPath(rest[1:i]), true
			}
		}
		return "", false
	}
	return "", false
}

// unquoteExecStartPath reverses quoteExecStartPath's escaping of an already
// unwrapped (quotes stripped) value: C-style \\ and \", plus systemd's doubled
// $$ and %% specifiers. One pass, so a literal backslash followed by a quote
// survives — two ReplaceAll passes would not.
func unquoteExecStartPath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		if (s[i] == '$' || s[i] == '%') && i+1 < len(s) && s[i+1] == s[i] {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// launchdPlistProgramPath extracts the program from the plist's
// ProgramArguments array: the first <string> after the key. The inverse of
// launchdAutostartPlist's html.EscapeString, kept in lockstep by the same
// round-trip test.
func launchdPlistProgramPath(content string) (string, bool) {
	idx := strings.Index(content, "<key>ProgramArguments</key>")
	if idx < 0 {
		return "", false
	}
	rest := content[idx:]
	start := strings.Index(rest, "<string>")
	if start < 0 {
		return "", false
	}
	rest = rest[start+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return "", false
	}
	return html.UnescapeString(rest[:end]), true
}

// RestartAutostartUnit restarts the daemon through the OS service manager —
// `systemctl --user restart` on Linux, `launchctl kickstart -k` on macOS — so
// the daemon stays supervised. Used by the upgrade/auto-update respawn path
// (#796): the Shutdown RPC is a clean exit that Restart=on-failure
// deliberately does not restart, so respawning via launchDaemonProcess would
// leave the unit inactive and demote the daemon to an unsupervised ad-hoc
// child until the next login.
func RestartAutostartUnit() error {
	switch autostartGOOS {
	case "linux":
		if out, err := autostartUnitCommand("systemctl", "--user", "restart", autostartUnitName); err != nil {
			return fmt.Errorf("systemctl --user restart %s failed: %w\n%s", autostartUnitName, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		// The domain here is the one InstallAutostart bootstraps into: they
		// are the same helper precisely so they cannot drift apart again
		// (#1947).
		target := launchdServiceTarget()
		if out, err := autostartUnitCommand("launchctl", "kickstart", "-k", target); err != nil {
			return fmt.Errorf("launchctl kickstart -k %s failed: %w\n%s", target, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

// PauseAutostartUnit stops the daemon autostart unit — `systemctl --user stop`
// on Linux, `launchctl unload` on macOS — WITHOUT uninstalling it, so the
// service manager cannot (re)launch the daemon until ResumeAutostartUnit.
// Stopping the unit also stops the supervised daemon itself. `af reset` wraps
// its wipe in this pair: the unit relaunches a daemon that exits uncleanly,
// and a daemon relaunched mid-wipe restores sessions from the very records
// the wipe is deleting, ending up holding ghost instances with no storage
// backing.
func PauseAutostartUnit() error {
	switch autostartGOOS {
	case "linux":
		if out, err := autostartUnitCommand("systemctl", "--user", "stop", autostartUnitName); err != nil {
			return fmt.Errorf("systemctl --user stop %s failed: %w\n%s", autostartUnitName, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		// Addressed by service target, not plist path, so the pause lands in
		// the same gui/<uid> domain the install bootstrapped into (#1947).
		// A bootout of a job that is not loaded fails — but a job that is not
		// loaded is already paused for our purposes, and the caller (`af
		// reset`) only warns, so the failure is cosmetic where it is possible
		// at all.
		if out, err := autostartUnitCommand("launchctl", "bootout", launchdServiceTarget()); err != nil {
			return fmt.Errorf("launchctl bootout %s failed: %w\n%s", launchdServiceTarget(), err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

// ResumeAutostartUnit re-arms the unit PauseAutostartUnit stopped — `systemctl
// --user start` on Linux, `launchctl load` on macOS — which also starts the
// daemon again (the launchd agent is RunAtLoad).
func ResumeAutostartUnit() error {
	switch autostartGOOS {
	case "linux":
		if out, err := autostartUnitCommand("systemctl", "--user", "start", autostartUnitName); err != nil {
			return fmt.Errorf("systemctl --user start %s failed: %w\n%s", autostartUnitName, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return fmt.Errorf("failed to resolve LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
		// bootstrap (not the legacy load) so the resumed agent lands back in
		// the same gui/<uid> domain the install used and the restart kicks.
		if out, err := autostartUnitCommand("launchctl", "bootstrap", launchdGUIDomain(), plistPath); err != nil {
			return fmt.Errorf("launchctl bootstrap %s %s failed: %w\n%s", launchdGUIDomain(), plistPath, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

// UninstallAutostart removes the daemon autostart unit installed by
// InstallAutostart. The daemon itself keeps running until it exits or is
// stopped; it just no longer restarts at login. Returns the path of the
// removed unit file, or "" if none was installed.
//
// The unit path comes from the same resolvers InstallAutostart uses, so both
// always target the same real directory — with XDG_CONFIG_HOME set, systemd
// units live under it, not under ~/.config (#1091).
func UninstallAutostart() (string, error) {
	switch autostartGOOS {
	case "linux":
		dir, err := autostartSystemdUserDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve systemd user directory: %w", err)
		}
		unitPath := filepath.Join(dir, autostartUnitName)
		if _, err := os.Stat(unitPath); os.IsNotExist(err) {
			return "", nil
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "disable", "--now", autostartUnitName); err != nil {
			return "", fmt.Errorf("failed to disable daemon service: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove unit file: %w", err)
		}
		if out, err := autostartUnitCommand("systemctl", "--user", "daemon-reload"); err != nil {
			return "", fmt.Errorf("failed to reload systemd user daemon: %w\n%s", err, strings.TrimSpace(string(out)))
		}
		return unitPath, nil

	case "darwin":
		dir, err := autostartLaunchAgentsDir()
		if err != nil {
			return "", fmt.Errorf("failed to resolve LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(dir, autostartLaunchdLabel+".plist")
		if _, err := os.Stat(plistPath); os.IsNotExist(err) {
			return "", nil
		}
		// Same gui/<uid> domain as the install's bootstrap (#1947). Best-effort:
		// removing the plist is what makes the uninstall stick across logins.
		_, _ = autostartUnitCommand("launchctl", "bootout", launchdServiceTarget())
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("failed to remove plist file: %w", err)
		}
		return plistPath, nil

	default:
		return "", fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}
