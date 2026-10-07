package doctor

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/daemon"
)

// The autostart half of the skew checks (#1044): whose unit is it, what binary
// does it launch, and is anything actually supervising the daemon. Split out of
// skew.go to stay under the file-length limit (#1145), mirroring the existing
// skew_autostart_test.go split; the shared helpers live there and are visible
// here because it is one package.

// autostartScope answers "is the installed autostart unit this home's at all?",
// once per run.
//
// There is ONE autostart unit per user, and it bakes its AGENT_FACTORY_HOME at
// install time — so a unit installed for the developer's real home is still the
// only unit on the box when doctor runs under AGENT_FACTORY_HOME=/tmp/sandbox.
// Treating "a unit file exists" as "this home's unit" is the whose-home defect
// that #1916 and #1950 are, and it makes every autostart row here an assertion
// about somebody else's daemon. daemon.AutostartUnitServesHome (#1919) is the
// established answer, including the subtlety that a unit with no baked
// AGENT_FACTORY_HOME serves the DEFAULT home — absent is a value, not an unknown.
func (c *scanContext) autostartScope() (serves, installed bool, err error) {
	if !c.autostartScoped {
		c.autostartServes, c.autostartInstalled, c.autostartScopeErr =
			c.opts.autostartServesHome(c.opts.ConfigDir)
		c.autostartScoped = true
	}
	return c.autostartServes, c.autostartInstalled, c.autostartScopeErr
}

// autostartUnitIsOurs reports whether the autostart checks may speak about the
// installed unit. A false return means the caller must stay silent: the unit is
// absent, someone else's, or unestablished — and doctor does not guess.
//
// It reports nothing itself, deliberately. Both autostart checks call it, so a
// row emitted here would appear twice for one condition; and checkDaemonHealth
// already renders every one of these states on its "autostart" row — absent,
// serving another home, or unreadable. One condition, one row, one place that
// owns it.
func autostartUnitIsOurs(ctx *scanContext) bool {
	serves, installed, err := ctx.autostartScope()
	return err == nil && installed && serves
}

// checkAutostartPath compares the binary the autostart unit launches against
// the binary running this command. When they differ, every upgrade that lands
// on the client's path leaves the supervised daemon respawning the old one —
// so the skew survives restarts and reboots, and `af daemon restart` never
// fixes it.
func checkAutostartPath(ctx *scanContext, report *Report) {
	if !autostartUnitIsOurs(ctx) {
		return
	}
	info := ctx.opts.autostartUnit()
	if !info.Supported || !info.Exists {
		// checkDaemonHealth already warns when no unit is installed.
		return
	}
	if info.Err != nil {
		// A unit is installed but unreadable. Saying so is the whole job: the
		// alternative is doctor printing nothing about a unit the user can see
		// on disk. Counts as a problem — an unreadable unit is not a working one.
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("an autostart unit is installed at %s but cannot be read: %v", info.Path, info.Err),
			"fix the unit file's permissions, or reinstall it: af daemon install", true)
		return
	}
	self, err := ctx.opts.selfBinary()
	if err != nil {
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("cannot resolve this af binary's path: %v", err),
			"reinstall autostart if the daemon runs an unexpected binary: af daemon install", false)
		return
	}
	unitPath := resolvePath(info.ExecPath)
	selfPath := resolvePath(self)
	if unitPath == selfPath {
		report.Pass(sectionDaemon, "autostart path", "launches this af binary ("+selfPath+")")
		return
	}

	// A differing path alone does not prove a problem, and treating it as one
	// cries wolf on an ordinary dev box: anyone running a binary they just
	// built has a unit pointing at their installed af, which is correct and
	// intended. Nor can the two be told apart by version — a `go build` of this
	// tree reports the same number as the release.
	//
	// What actually strands a daemon is the two binaries being different
	// VERSIONS: then whatever the unit respawns is not what you are running,
	// and no restart fixes it. Same version at two paths is worth a note, not a
	// verdict.
	client := strings.TrimSpace(ctx.opts.Version)
	unitVersion, err := ctx.opts.binaryVersion(info.ExecPath)
	switch {
	case err != nil || unitVersion == "":
		// The unit launches something we cannot identify as an af binary —
		// including a path that no longer exists, in which case the unit cannot
		// start a daemon at all. That is worth acting on.
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("your autostart daemon runs %s, which is not a readable af binary; your af is %s",
				unitPath, selfPath),
			"reinstall autostart so it launches your af: af daemon install", true)
	case client == "" || client == devVersion:
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("your autostart daemon runs %s (%s) and your af is %s; this client is an unreleased build, so skew cannot be judged",
				unitPath, unitVersion, selfPath),
			"if the UI misbehaves, reinstall autostart: af daemon install", false)
	case unitVersion != client:
		report.Fail(sectionDaemon, "autostart path",
			fmt.Sprintf("your autostart daemon runs %s (%s) but your af is %s (%s); "+
				"upgrades won't reach the supervised daemon, and restarting it respawns the old one",
				unitPath, unitVersion, selfPath, client),
			"reinstall autostart: af daemon install")
	default:
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("your autostart daemon runs %s and your af is %s — same version (%s), so nothing is skewed today",
				unitPath, selfPath, unitVersion),
			"if you upgrade one path, reinstall autostart so both stay in step: af daemon install", false)
	}
}

// checkAutostartSupervision reports an autostart unit that exists but is not
// actually supervising the daemon. On macOS that includes an agent loaded in a
// domain other than the gui/<uid> the restart path targets: restarts silently
// miss it, so an old daemon keeps serving and skew never clears.
func checkAutostartSupervision(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if !autostartUnitIsOurs(ctx) {
		return
	}
	info := ctx.opts.autostartSupervision()
	if !info.Supported || !info.UnitPresent {
		return
	}

	// The launchd domain mismatch is its own finding, and only when launchd
	// actually ANSWERED that the job is elsewhere.
	elsewhere := false
	info.LoadedElsewhere.Match(
		func() {
			elsewhere = true
			report.Warn(sectionDaemon, "autostart supervision",
				fmt.Sprintf("the launchd agent is loaded outside %s, where af's restarts are sent (%s)",
					info.Domain, info.Detail),
				"reload it in the right domain: af daemon install", true)
		},
		func() {}, func() {}, func(error) {},
	)
	if elsewhere {
		return
	}

	// Match has no default branch, so the "we could not ask" case cannot be
	// forgotten here or in any future edit: the compiler demands it.
	info.Active.Match(
		func() { supervisionActive(ctx, report, info, h) },
		func() { supervisionNotRunning(report, info, h) },
		func() { supervisionUnitUnknownToManager(report, info, h) },
		func(cause error) {
			detail := fmt.Sprintf("could not query the service manager, so supervision is unknown: %s", oneLine(cause))
			if h.PingErr == nil {
				detail = fmt.Sprintf("a daemon is responding, but the service manager could not be queried, so whether it is supervised is unknown: %s", oneLine(cause))
			}
			report.Warn(sectionDaemon, "autostart supervision",
				detail,
				"check that the service manager is reachable (`systemctl --user status` / `launchctl print`), then rerun `af doctor`", false)
		},
	)
}

// supervisionActive renders the manager's "it is running" answer, which still
// leaves the is-enabled question to report.
func supervisionActive(ctx *scanContext, report *Report, info daemon.SupervisionInfo, h daemon.HealthStatus) {
	_ = ctx
	enabledYes := false
	enablement := ""
	enablementProblem := false
	info.Enabled.Match(
		func() { enabledYes, enablement = true, "unit is enabled" },
		func() { enablement, enablementProblem = "unit is not enabled", true },
		func() {
			enablement, enablementProblem = "the service manager has no enablement record for the unit", true
		},
		func(cause error) { enablement = "whether the unit starts at login is unknown: " + oneLine(cause) },
	)

	if h.PingErr == nil {
		ownership := daemon.ServingDaemonSupervised(h, info)
		handled := false
		ownership.Match(
			func() {
				if enabledYes {
					report.Pass(sectionDaemon, "autostart supervision",
						fmt.Sprintf("unit is enabled and running; it owns the responding daemon pid %d", h.ServingPID))
				} else {
					report.Warn(sectionDaemon, "autostart supervision",
						fmt.Sprintf("the unit is running and owns responding daemon pid %d; %s", h.ServingPID, enablement),
						"run `af daemon install` if the unit is not enabled, or check the service manager directly", enablementProblem)
				}
				handled = true
			},
			func() {
				detail := fmt.Sprintf("the installed unit owns pid %d, but responding daemon pid %d is not supervised by it",
					info.MainPID, h.ServingPID)
				if info.MainPID == 0 {
					detail = "the installed unit is active but owns no daemon process; the responding daemon is not supervised by it"
				}
				report.Warn(sectionDaemon, "autostart supervision", detail+"; "+enablement,
					"run `af daemon adopt` to hand the daemon back to the installed unit", true)
				handled = true
			},
			func() {
				report.Warn(sectionDaemon, "autostart supervision",
					"the service manager has no record of the installed unit, so the responding daemon is not supervised by it; "+enablement,
					"reload the unit: `systemctl --user daemon-reload`, or reinstall it: af daemon install", true)
				handled = true
			},
			func(cause error) {
				if enabledYes {
					report.Warn(sectionDaemon, "autostart supervision",
						"the unit is enabled and active, but whether it owns the responding daemon is unknown: "+oneLine(cause),
						"upgrade or restart the daemon and rerun `af doctor`", false)
				} else {
					report.Warn(sectionDaemon, "autostart supervision",
						fmt.Sprintf("the unit is active, but whether it owns responding daemon pid %d is unknown: %s; %s",
							h.ServingPID, oneLine(cause), enablement),
						"run `af daemon install` if the unit is not enabled, or upgrade/restart the daemon and rerun `af doctor`", enablementProblem)
				}
				handled = true
			},
		)
		if handled {
			return
		}
	}
	info.Enabled.Match(
		func() {
			report.Pass(sectionDaemon, "autostart supervision", "unit is enabled and running")
		},
		func() {
			report.Warn(sectionDaemon, "autostart supervision",
				fmt.Sprintf("the unit is running but not enabled, so it won't start at login (%s)", info.Detail),
				"enable it: af daemon install", true)
		},
		func() {
			report.Warn(sectionDaemon, "autostart supervision",
				"the daemon is running but the service manager has no record of the unit, so it won't start at login",
				"reload the unit: `systemctl --user daemon-reload`, or reinstall it: af daemon install", true)
		},
		func(cause error) {
			report.Warn(sectionDaemon, "autostart supervision",
				fmt.Sprintf("the daemon is running, but whether it starts at login is unknown: %s", oneLine(cause)),
				"check the service manager directly, then rerun `af doctor`", false)
		},
	)
}

// supervisionNotRunning renders an answered "it is not running".
func supervisionNotRunning(report *Report, info daemon.SupervisionInfo, h daemon.HealthStatus) {
	if info.Err != nil {
		// The unit is installed but unreadable AND it is not running: the
		// unreadable file is the likely reason nothing is supervising.
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("a unit file is installed but cannot be read (%v) and the service manager is not running it", info.Err),
			"fix the unit file's permissions, or reinstall it: af daemon install", true)
		return
	}
	if h.PingErr == nil {
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("a daemon is responding, but the installed unit is not running it (%s); the responder is not supervised", info.Detail),
			"run `af daemon adopt` to hand the daemon back to the installed unit", true)
		return
	}
	loadedButDead := false
	info.Loaded.Match(
		func() {
			// Everything looks configured — launchd knows the agent — while no
			// daemon is actually running.
			loadedButDead = true
			report.Warn(sectionDaemon, "autostart supervision",
				fmt.Sprintf("the launchd agent is loaded in %s but no daemon process is running (%s)",
					info.Domain, info.Detail),
				"start it: af daemon restart", true)
		},
		func() {}, func() {}, func(error) {},
	)
	if loadedButDead {
		return
	}
	report.Warn(sectionDaemon, "autostart supervision",
		fmt.Sprintf("a unit file is installed but the service manager is not running it (%s)", info.Detail),
		"reinstall autostart: af daemon install", true)
}

// supervisionUnitUnknownToManager renders the NOT-FOUND answer: the manager
// replied that it has no such unit.
//
// This is the state a two-valued probe threw away, and it is the most actionable
// of the lot. The unit FILE is installed — we only probe when it is — so the
// manager not knowing it means it was never loaded: `systemctl --user
// daemon-reload` fixes it. Reported as "inactive" it sent users to reinstall
// something that was already there; reported as "unknown" it told them nothing.
func supervisionUnitUnknownToManager(report *Report, info daemon.SupervisionInfo, h daemon.HealthStatus) {
	if h.PingErr == nil {
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("a daemon is responding, but the service manager has no record of the installed unit (%s); the responder is not supervised", info.Detail),
			"load it: `systemctl --user daemon-reload`, or reinstall it: af daemon install", true)
		return
	}
	report.Warn(sectionDaemon, "autostart supervision",
		fmt.Sprintf("a unit file is installed but the service manager has no record of it (%s), so nothing starts af at login", info.Detail),
		"load it: `systemctl --user daemon-reload`, or reinstall it: af daemon install", true)
}
