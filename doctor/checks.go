package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
)

type rootAgentProgramInspection struct {
	resolveProfile func(context.Context, *config.RepoContext) (config.ResolvedValue, error)
	resolveConfig  func(context.Context, *config.RepoContext) (*config.ResolvedConfig, error)
}

var (
	rootAgentProgramProbeTimeout  = binaryProbeTimeout
	resolveRootAgentForInspection = func(ctx context.Context, global *config.Config, projectSelector string, strictProjectLookup bool) (rootAgentProgramInspection, error) {
		snapshot, err := config.ResolveRootAgentInspectionSnapshotWithConfigContext(ctx, global, projectSelector, strictProjectLookup)
		if err != nil {
			return rootAgentProgramInspection{}, err
		}
		return rootAgentProgramInspection{
			resolveProfile: snapshot.ResolveRootAgentForRepoContext,
			resolveConfig:  snapshot.ResolveConfigForRepoContext,
		}, nil
	}
	inspectRootAgentProgram = func(ctx context.Context, repo *config.RepoContext, profile config.RootAgent, inspection rootAgentProgramInspection) (string, error) {
		if !daemon.RootAgentProfileNeedsRepoConfig(profile) {
			return daemon.RootAgentProgramForProfileResolvedConfig(profile, nil)
		}
		resolved, err := inspection.resolveConfig(ctx, repo)
		if err != nil {
			return "", err
		}
		return daemon.RootAgentProgramForProfileResolvedConfig(profile, resolved)
	}
)

func selfPID() int { return os.Getpid() }

func tempDirDefault() string { return os.TempDir() }

var (
	daemonProcessArgv = daemon.ProcessArgv
	// withheldEnvCause names why the kernel served no environment for a
	// process (#3584). A package var so tests on Linux, which has no such
	// redaction, can stage the darwin answer.
	withheldEnvCause = proctree.WithheldEnvCause
	// tempHomeLockProbe answers "is a daemon running for this home?" through the
	// home's daemon.lock — the kernel-guaranteed fact that authorises removing an
	// abandoned temp home (#1989). A package var so tests can stage each of the
	// three outcomes (held / takeable / unprovable) deterministically, without a
	// real daemon or a real NFS mount.
	tempHomeLockProbe = daemon.ProbeHomeLock
	// The per-process facts that decide whether a daemon is OURS (#1044).
	// Injectable for the same reason as the two above: the states that matter —
	// another user's process, an environ we may not read — cannot be staged by
	// a test without root, and whether they hold for any given pid depends on
	// the machine (in a container pid 1 is often the test user itself).
	daemonProcessEnvLookup = proctree.EnvLookup
	daemonProcessOwnerUID  = proctree.OwnerUID
	daemonProcessCwd       = proctree.WorkingDir
	// cpuFraction reads a process's lifetime-average CPU. A package var so
	// tests can stage the possible-orphan overflow tail's measured CPU
	// deterministically: the per-process fraction is, like the daemon
	// identities above, a machine-dependent fact no test can stage without
	// root, and whether a busy loop reads 0.3 or 1.0 of a core depends on
	// the host's free-core count at scan time. The possible-orphan summary
	// describes the hidden tail by its measured max, so the one row the
	// operator sees about those processes reflects their CPU rather than
	// a hardcoded "idle" guess.
	cpuFraction = proctree.CPUFraction
)

// processLeakMinAge separates durable escaped/orphaned processes from ordinary
// session teardown. A minute is far longer than one doctor scan but negligible
// beside the 8-hour-to-10-day leaks this check found in production (#2627).
//
// runawayCPUFraction and runawayMinAge define "pegging a core for an extended
// period": lifetime-average CPU ≥ 80% of a core for a process at least 30
// minutes old. A legitimate build rarely sustains that average; the leaked
// `yes` processes from the outage sat at ~100% for 15 days.
const (
	processLeakMinAge  = time.Minute
	runawayCPUFraction = 0.8
	runawayMinAge      = 30 * time.Minute
)

// describeProc renders "pid 123 (yes, 99% CPU over 15d2h): yes" for findings.
func describeProc(p proctree.Process) string {
	desc := fmt.Sprintf("pid %d (%s", p.PID, p.Comm)
	if frac, age, err := cpuFraction(p); err == nil {
		desc += fmt.Sprintf(", %.0f%% CPU over %s", frac*100, formatAge(age))
	}
	desc += ")"
	if cmdline := proctree.Cmdline(p.PID); cmdline != "" {
		if len(cmdline) > 120 {
			cmdline = cmdline[:120] + "…"
		}
		desc += ": " + cmdline
	}
	return desc
}

func formatAge(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// killFix builds a --fix action that terminates one verified process with
// TERM→KILL escalation. Fails if the process survives SIGKILL.
func killFix(ctx *scanContext, p proctree.Process) func() error {
	return func() error {
		remaining := proctree.KillEscalating(
			[]proctree.Process{p}, ctx.opts.killGrace, ctx.opts.killTermWait, nil)
		if len(remaining) > 0 {
			return fmt.Errorf("process %d survived SIGKILL", p.PID)
		}
		return nil
	}
}

// checkDaemonHealth reports on the active install's daemon: socket, ping,
// autostart unit, pid file, and binary freshness. Read-only; never fixable
// (restarting the daemon is a user decision). Takes the run's shared health
// probe so every daemon check reasons about one consistent observation.
func checkDaemonHealth(ctx *scanContext, report *Report, h daemon.HealthStatus, cfg *config.Config) {
	if h.SocketErr != nil {
		report.Fail(sectionDaemon, "daemon", fmt.Sprintf("cannot resolve daemon socket path: %v", h.SocketErr),
			"fix AGENT_FACTORY_HOME and rerun `af doctor`")
		return
	}
	switch {
	case h.PingErr == nil:
		switch h.Phase {
		case "", daemon.DaemonPhaseReady:
			// Empty is a responding daemon from before the additive phase field.
			report.Pass(sectionDaemon, "daemon", "responding on "+h.SocketPath)
		case daemon.DaemonPhaseWarming:
			report.Warn(sectionDaemon, "daemon",
				"responding on "+h.SocketPath+" but startup is still in progress (phase warming)",
				"wait for daemon startup to finish; if it remains warming, inspect the daemon log", false)
		case daemon.DaemonPhaseUpgradeProbation:
			report.Warn(sectionDaemon, "daemon",
				fmt.Sprintf("responding on %s while upgrade transaction %s is in validation probation", h.SocketPath, h.TransactionID),
				"wait for upgrade validation to commit or roll back; inspect the daemon log if the phase does not advance", false)
		default:
			report.Warn(sectionDaemon, "daemon",
				fmt.Sprintf("responding on %s with unrecognized lifecycle phase %q", h.SocketPath, h.Phase),
				"upgrade this af client before diagnosing daemon readiness", false)
		}
	case !h.SocketExists:
		report.Pass(sectionDaemon, "daemon", "not running; starts on demand")
	default:
		// The socket exists but the ping failed — classify WHY. A dial timeout
		// is not proof the daemon is dead: a live daemon with a saturated
		// control-socket accept backlog times out identically, so reading that
		// as a Fail would send the user to `af daemon restart` over a working
		// daemon and drive a spurious nonzero exit (#2040). Only a completed
		// negative (a refusal) is the definite Fail; a timeout is advisory.
		notResponding := func() {
			report.Fail(sectionDaemon, "daemon", fmt.Sprintf("socket %s exists but the daemon is not responding (%v)", h.SocketPath, h.PingErr),
				"run `af daemon restart`; if it still fails, remove the stale socket after verifying no daemon is running")
		}
		daemon.ClassifyPingFailure(h.PingErr).Match(
			// Yes and NotFound are unreachable: ClassifyPingFailure returns them
			// only for a nil error, and this arm runs only when the ping FAILED.
			// They route to the definite Fail rather than to a fabricated pass, so
			// a future fifth outcome cannot quietly turn a broken daemon healthy.
			notResponding,
			notResponding, // No: a refusal — nobody is behind the socket.
			notResponding,
			func(cause error) {
				report.Warn(sectionDaemon, "daemon",
					fmt.Sprintf("control socket %s did not answer within the dial timeout, so daemon liveness is "+
						"unknown — it may be alive but busy (%v)", h.SocketPath, h.PingErr),
					"if the daemon seems wedged run `af daemon restart`; a momentary timeout under heavy RPC load is "+
						"harmless and needs no action", false)
			},
		)
	}
	daemonConfig := cfg
	if h.PingErr == nil && daemonConfig == nil && ctx.globalConfigMissing {
		// A deleted or never-materialized file has a known next-start posture,
		// but it does not make every default a user-configured requirement. Apply
		// defaults only to diagnostics about the daemon that is demonstrably still
		// running with an older snapshot; other checks retain nil/unconfigured.
		daemonConfig = config.DefaultConfig()
	}
	if h.PingErr == nil && daemonConfig != nil {
		checkRunningDaemonConfig(report, h, daemonConfig)
		checkRootAgentPrograms(ctx, report, daemonConfig)
	}
	// The listener/auth disk-posture row (#5137 refusal, opted-in exposure)
	// lives in listener_posture.go.
	checkListenerPosture(report, cfg)
	if h.PingErr == nil {
		checkRefusedHosts(report, h.RefusedHosts, cfg)
	}
	// "A unit file exists" is not "this home has autostart". There is one unit
	// per user and it bakes its AGENT_FACTORY_HOME at install time, so under a
	// non-default AGENT_FACTORY_HOME the installed unit is somebody else's
	// (#1916/#1919/#1950). h.AutostartUnit only answers the file question, so
	// the scope gate answers the ownership one.
	serves, installed, scopeErr := ctx.autostartScope()
	switch {
	case scopeErr != nil:
		report.Warn(sectionDaemon, "autostart", fmt.Sprintf("cannot read the installed autostart unit: %v", scopeErr),
			"fix the unit file, or reinstall it: af daemon install", true)
	case !installed:
		report.Warn(sectionDaemon, "autostart", "not installed",
			"run `af daemon install` to keep scheduled tasks running across reboots", false)
	case !serves:
		// Advisory: running under a non-default AGENT_FACTORY_HOME is a
		// deliberate act (a sandbox, a second home), and the user is not
		// obliged to give it autostart.
		report.Warn(sectionDaemon, "autostart",
			fmt.Sprintf("the installed autostart unit serves a different agent-factory home, so %s has no supervised daemon",
				ctx.opts.ConfigDir),
			"if this home should start at login, install autostart while it is active: af daemon install", false)
	default:
		report.Pass(sectionDaemon, "autostart", "installed")
	}
	// An unverifiable pid is NEITHER verified nor stale: the file names a
	// live af daemon whose home binding could not be proven — possibly this
	// home's own daemon with an unreadable process frame (the normal shape
	// on macOS), possibly another home's. It gets an advisory rather than
	// the stale-file row, because suggesting removal over a pid that might
	// name this home's live daemon would orphan the very handle recovery
	// needs (#5188).
	if h.PIDUnverifiable && h.PingErr != nil {
		report.Warn(sectionDaemon, "daemon.pid",
			fmt.Sprintf("records pid %d, a live af daemon whose home could not be verified", h.PIDFilePID),
			"it may be this home's daemon or another's — check `af daemon status` in the intended home before removing the file or signalling the pid", false)
	}
	if h.PIDFilePID > 0 && !h.PIDVerified && !h.PIDUnverifiable && h.PingErr != nil {
		report.Warn(sectionDaemon, "daemon.pid", fmt.Sprintf("records pid %d but no agent-factory daemon is running under it", h.PIDFilePID),
			"remove the stale daemon.pid after verifying the pid is not an af daemon", true)
	}
	if h.BinaryDeleted {
		report.Warn(sectionDaemon, "daemon binary", fmt.Sprintf("pid %d is running a binary that was replaced on disk", h.PIDFilePID),
			"run `af daemon restart` to pick up the current binary", true)
	}
}

// checkRootAgentPrograms compares every adopted live root with the profile the
// current on-disk config resolves for its repository. The daemon-side warning
// compares against its frozen startup snapshot; doctor's disk view is
// deliberately complementary, so a post-start edit is visible here even before
// a restarted daemon can enforce it.
func checkRootAgentPrograms(ctx *scanContext, report *Report, cfg *config.Config) {
	instances, err := ctx.opts.sessionInventory()
	if err != nil {
		report.Warn(sectionDaemon, "root agent program",
			"could not compare live root sessions with the configured profiles: "+oneLine(err),
			"rerun `af doctor` when the daemon session inventory is available", false)
		report.markIncomplete("root agent program")
		return
	}
	compared, drifted, unresolved, unrecorded := 0, 0, 0, 0
	for _, inst := range instances {
		if !session.IsReservedTitle(inst.Title) || rootSessionIsInert(inst) {
			continue
		}
		if inst.InFlightOp != session.OpNone || inst.Status == session.Loading || inst.Status == session.Deleting {
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not compare the root agent program for %s because a lifecycle operation is in-flight", rootSessionDisplayPath(inst)),
				"wait for the operation to settle, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		if inst.StartupStateUnknown {
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not compare the root agent program for %s because its startup state is unknown", rootSessionDisplayPath(inst)),
				"kill the startup-unknown root, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		identityPath := inst.Worktree.RepoPath
		if identityPath == "" {
			identityPath = inst.Worktree.WorktreePath
		}
		if identityPath == "" {
			identityPath = inst.Path
		}
		if identityPath == "" {
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not inspect the root agent program for %s because its repository path is missing", rootSessionDisplayPath(inst)),
				"repair or remove the retained root session record, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		// Reuse doctor's binaryProbeTimeout independently for each root: one stale
		// mount is an unknown answer about that root, not permission to spend the
		// probe budget of every healthy root that follows it in the inventory.
		probeCtx, cancel := context.WithTimeout(context.Background(), rootAgentProgramProbeTimeout)
		inspection, resolveErr := resolveRootAgentForInspection(probeCtx, cfg, identityPath, false)
		if resolveErr != nil {
			cancel()
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not resolve the configured profile for live root at %s: %s", identityPath, oneLine(resolveErr)),
				"restore the checkout or mount, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		commandPath := inst.Worktree.WorktreePath
		if commandPath == "" {
			commandPath = inst.Path
		}
		if commandPath == "" {
			commandPath = identityPath
		}
		commandRepo, resolveErr := config.RepoFromPathContext(probeCtx, commandPath)
		if resolveErr != nil {
			cancel()
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not revalidate the configured profile for live root at %s: %s", commandPath, oneLine(resolveErr)),
				"restore the checkout or mount, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		resolved, resolveErr := inspection.resolveProfile(probeCtx, commandRepo)
		if resolveErr != nil {
			cancel()
			unresolved++
			remediation := "repair the named config source, then rerun `af doctor`"
			if errors.Is(resolveErr, config.ErrRootAgentInspectionIdentityChanged) {
				remediation = "rerun `af doctor`; if the checkout is being replaced, wait for that operation to finish first"
			}
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not revalidate the configured profile for live root at %s: %s", commandPath, oneLine(resolveErr)),
				remediation, false)
			report.markIncomplete("root agent program")
			continue
		}
		if config.RootAgentValueFailsClosed(resolved) {
			cancel()
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not inspect the configured profile for live root at %s: %s", identityPath, config.RootAgentFailClosedReason(resolved)),
				"repair the named config source, then rerun `af doctor`", false)
			report.markIncomplete("root agent program")
			continue
		}
		profile, ok := resolved.Value.(config.RootAgent)
		if !ok {
			cancel()
			unresolved++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not inspect the configured profile for live root at %s: unexpected root_agent resolution type %T", identityPath, resolved.Value),
				"rerun `af doctor`; if this persists, report the incompatible config result", false)
			report.markIncomplete("root agent program")
			continue
		}
		if !profile.Enabled {
			cancel()
			drifted++
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("live root at %s remains running although its configured profile is disabled · the live root was adopted as-is", identityPath),
				"restart the daemon to load the disabled profile, then kill the root; restarting alone does not stop an adopted live root", true)
			continue
		}
		runningProgram := inst.RuntimeProgram
		if strings.TrimSpace(runningProgram) == "" {
			cancel()
			// Unknown, not uninspectable: adoption and name-only reattach never
			// record a launch command, so an empty runtime_program is the designed
			// steady state for a root this daemon did not spawn. Nothing here is
			// broken and nothing the user can run fills the field — only the
			// root's next real launch does. That is a note, not a warning.
			unrecorded++
			report.Info(sectionDaemon, "root agent program",
				fmt.Sprintf("the resolved runtime command is not recorded for %s — the running process was adopted, or launched before runtime commands were recorded; it will be recorded on the root's next launch", rootSessionDisplayPath(inst)))
			continue
		}
		configuredProgram, programErr := inspectRootAgentProgram(probeCtx, commandRepo, profile, inspection)
		cancel()
		if programErr != nil {
			unresolved++
			remediation := "repair the named config source, then rerun `af doctor`"
			if errors.Is(programErr, config.ErrRootAgentInspectionIdentityChanged) {
				remediation = "rerun `af doctor`; if the checkout is being replaced, wait for that operation to finish first"
			}
			report.Warn(sectionDaemon, "root agent program",
				fmt.Sprintf("could not inspect the configured command for live root at %s: %s", commandPath, oneLine(programErr)),
				remediation, false)
			report.markIncomplete("root agent program")
			continue
		}
		compared++
		// Same byte-or-literal-argv rule as the daemon's drift latch
		// (#5504): quoting-only spellings of the same argv are not drift,
		// and a non-literal side falls back to the byte verdict.
		if configuredProgram == runningProgram ||
			sessionenv.LiteralCommandsEqual(configuredProgram, runningProgram) {
			continue
		}
		drifted++
		report.Warn(sectionDaemon, "root agent program",
			fmt.Sprintf("root agent program drift for %s: configured command %q · running command %q · the live root was adopted as-is", commandPath, configuredProgram, runningProgram),
			"restart the daemon, then kill the root", true)
	}
	if drifted == 0 && unresolved == 0 {
		detail := "no enabled live root sessions to compare"
		switch {
		case compared > 0:
			detail = fmt.Sprintf("%d live root session(s) match the configured command", compared)
			if unrecorded > 0 {
				detail += fmt.Sprintf("; %d adopted live root(s) have no recorded runtime command", unrecorded)
			}
		case unrecorded > 0:
			detail = "every live root was adopted without a recorded runtime command to compare"
		}
		report.Pass(sectionDaemon, "root agent program", detail)
	}
}

func rootSessionDisplayPath(inst session.InstanceData) string {
	for _, path := range []string{inst.Worktree.WorktreePath, inst.Path, inst.Worktree.RepoPath} {
		if path != "" {
			return path
		}
	}
	return inst.Title
}

func rootSessionIsInert(inst session.InstanceData) bool {
	if inst.UserKilled {
		return true
	}
	switch session.EffectiveLiveness(inst) {
	case session.LiveLost, session.LiveDead, session.LiveArchived:
		return true
	default:
		return false
	}
}

// checkRunningDaemonConfig compares the file on disk with the immutable
// listener/auth posture returned by the process which answered Ping. An older
// responder is reported as unknown, not silently treated as current.
func checkRunningDaemonConfig(report *Report, h daemon.HealthStatus, cfg *config.Config) {
	matches := daemon.RunningConfigMatches(h, cfg)
	matches.Match(
		func() {
			report.Pass(sectionDaemon, "daemon config", "on-disk listener/auth config matches the running daemon")
		},
		func() {
			report.Warn(sectionDaemon, "daemon config",
				"config on disk differs from the running daemon ("+daemon.RunningConfigDifference(h.BootConfig, cfg)+")",
				"run `af daemon restart` to apply the config on disk", true)
		},
		func() {
			report.Warn(sectionDaemon, "daemon config",
				"the running daemon could not be compared with the config on disk because the responder was not identified",
				"restart the daemon, then rerun `af doctor`", false)
		},
		func(cause error) {
			report.Warn(sectionDaemon, "daemon config",
				"whether the running daemon matches the config on disk is unknown: "+oneLine(cause),
				"restart the daemon to apply the current config, or rerun `af doctor` after upgrading the daemon", false)
		},
	)
}

// checkForeignDaemons finds agent-factory daemon processes serving a home
// other than the active one. A daemon whose home directory no longer exists
// is unambiguously broken (it can only burn CPU and run stale cron tasks —
// #1093) and is killable with --fix; one whose home still exists might be an
// intentional second install, so it is reported only.
func checkForeignDaemons(ctx *scanContext, report *Report) {
	if ctx.snap == nil {
		return
	}
	// Normalized on both sides (see normalizeHome): a daemon that spells the
	// active home differently is ours, and calling it foreign would offer a
	// --fix kill of the live daemon serving this very run.
	activeHome := normalizeHome(ctx.opts.ConfigDir)

	// Shares the run's one daemon scan with checkDuplicateDaemons, which
	// classifies this home's daemons — the two must agree on which process
	// serves which home.
	for _, d := range ctx.daemonProcs() {
		p := d.proc
		pid := p.PID
		home := d.home
		if d.isSelfAncestor {
			// Our own ancestor daemon (doctor launched from a watch task, say).
			// checkDuplicateDaemons still counts it, but it must never be
			// offered for a kill: that is the daemon running this command.
			continue
		}
		if !d.ownedByUs {
			// Another user's daemon is not ours to report or reap: --fix would
			// offer a kill that can only fail with EPERM, and on a shared box
			// their daemon is none of this user's business (#1044).
			continue
		}
		if !d.homeKnown {
			// The environ was unreadable, so which home this daemon serves is
			// genuinely unknown. Calling it foreign would be a guess, and --fix
			// would then offer to kill a process on the strength of that guess.
			//
			// homeKnown is resolved upstream by daemonProcessHome, which reads
			// the environ through proctree.EnvLookup — now backed by the
			// three-valued LookupEnv (this PR), so a redacted or empty darwin
			// environment lands here as "unknown → skip" rather than a
			// fabricated default. The inline EnvUnknown/EnvAbsent switch this PR
			// originally added here is therefore redundant with #1920's refactor
			// and dropped in its favour.
			continue
		}
		if home == activeHome || home == "" {
			continue // this install's daemon; covered by checkDaemonHealth
		}
		if _, err := os.Stat(home); os.IsNotExist(err) {
			report.addActionableFinding(Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves agent-factory home %s which no longer exists "+
					"(abandoned daemon will run its cron tasks forever)", describeProc(p), home),
				FixAction: fmt.Sprintf("kill pid %d", pid),
				fix:       killFix(ctx, p),
			})
		} else if err != nil {
			report.addAdvisoryFinding(Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves agent-factory home %s whose status cannot be verified: %v",
					describeProc(p), home, err),
			})
		} else {
			report.addAdvisoryFinding(Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves a different agent-factory home (%s) — "+
					"left alone in case it is intentional", describeProc(p), home),
			})
		}
	}
}
