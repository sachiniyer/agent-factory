package daemon

import (
	"fmt"
	"html"
	"os"
	"strings"
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
	// Enabled is systemd's is-enabled; always true on macOS, where RunAtLoad
	// in the plist plays that role.
	Enabled bool
	// Active reports whether the service manager currently runs the unit —
	// systemd is-active, or a launchd agent loaded in the expected domain.
	Active bool
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
	LoadedElsewhere bool
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
		enabled, enabledOut := autostartUnitStateOK("is-enabled")
		active, activeOut := autostartUnitStateOK("is-active")
		info.Enabled, info.Active = enabled, active
		info.Detail = fmt.Sprintf("is-enabled=%s is-active=%s", enabledOut, activeOut)
	case "darwin":
		info.Enabled = true // RunAtLoad; launchd has no separate enable state
		info.Domain = launchdDomainTarget()
		if _, err := autostartUnitCommand("launchctl", "print", info.Domain); err == nil {
			info.Active = true
			info.Detail = "loaded in " + info.Domain
			return info
		}
		// Not in the domain the restart path targets. A legacy-domain load
		// still answers the domain-agnostic `launchctl list`, which is what
		// distinguishes "loaded somewhere else" from "not loaded at all".
		if _, err := autostartUnitCommand("launchctl", "list", autostartLaunchdLabel); err == nil {
			info.LoadedElsewhere = true
			info.Detail = "loaded outside " + info.Domain
			return info
		}
		info.Detail = "not loaded"
	}
	return info
}

// launchdDomainTarget is the gui/<uid>/<label> target the restart path uses.
// Kept here so doctor reports the same domain the restart actually kickstarts.
func launchdDomainTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), autostartLaunchdLabel)
}

// autostartUnitStateOK runs `systemctl --user <verb>` and reports whether it
// exited 0 along with its trimmed output. systemctl prints the state on both
// success and failure ("enabled" / "disabled"), so the output is worth keeping
// either way.
func autostartUnitStateOK(verb string) (bool, string) {
	out, err := autostartUnitCommand("systemctl", "--user", verb, autostartUnitName)
	text := strings.TrimSpace(string(out))
	if text == "" {
		text = "unknown"
	}
	return err == nil, text
}
