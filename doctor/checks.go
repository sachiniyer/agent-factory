package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func selfPID() int { return os.Getpid() }

func tempDirDefault() string { return os.TempDir() }

var (
	daemonProcessArgv = daemon.ProcessArgv
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
	if frac, age, err := proctree.CPUFraction(p); err == nil {
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
	if h.PingErr == nil && cfg != nil {
		checkRunningDaemonConfig(report, h, cfg)
	}
	// The #2090 exposure is INFORMATIONAL since #2168 Phase 0: a tokenless
	// network listener is an allowed, deliberate configuration, so this is a Warn
	// with problem=false — it never makes `af doctor` exit non-zero over a posture
	// the owner decided users may choose. It was a Fail ("not running, and it
	// cannot start") back when the daemon refused; there is no such dead end left
	// to report.
	//
	// Reported independently of daemon liveness, unlike the old row: the exposure
	// matters MOST when the daemon is up, because then it is actually being
	// served. The separate daemon-config row compares this disk value with the
	// posture returned by the running daemon itself (#2168 Phase 4).
	//
	// cfg is nil when the config could not be loaded at all (doctor.Run passes
	// what it got). Say nothing then rather than guessing a posture — the load
	// failure has its own row, and inventing either answer here would be worse
	// than the silence.
	if cfg != nil && config.ListenerServesUnauthenticatedNetwork(cfg.ListenAddr, cfg.RequireToken) {
		report.Warn(sectionDaemon, "listener",
			fmt.Sprintf("network.listen_addr %q is reachable from the network and network.require_token is false, so the control API "+
				"(including DeliverPrompt, which runs instructions through your agents) is served to anyone who can "+
				"reach that address, with no authentication", cfg.ListenAddr),
			"if that is not what you want, run `af config set network.require_token true` to require a bearer token (`af token "+
				"show` prints it), or `af config set network.listen_addr 127.0.0.1:8443` to serve this machine only",
			false)
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
	if h.PIDFilePID > 0 && !h.PIDVerified && h.PingErr != nil {
		report.Warn(sectionDaemon, "daemon.pid", fmt.Sprintf("records pid %d but no agent-factory daemon is running under it", h.PIDFilePID),
			"remove the stale daemon.pid after verifying the pid is not an af daemon", true)
	}
	if h.BinaryDeleted {
		report.Warn(sectionDaemon, "daemon binary", fmt.Sprintf("pid %d is running a binary that was replaced on disk", h.PIDFilePID),
			"run `af daemon restart` to pick up the current binary", true)
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

// checkOrphanedProcesses finds processes carrying the AF_SESSION ancestry
// marker. Marker + dead session = verified orphan (killable with --fix);
// marker + live session but outside its pane trees = escaped (report-only);
// no marker but a TMUX env var pointing at a dead server = possible orphan
// (report-only — could belong to any tmux, not necessarily agent-factory).
// checkProcessInspection reports whether doctor can see the process table at
// all, and it must run BEFORE every check that reads ctx.snap.
//
// This is the check that exists because of what its absence did (#1939).
// proctree was /proc-only with no darwin backend, so on macOS every snapshot
// failed, Run discarded the error, and the process checks below returned early
// on a nil snap. Nothing was reported — not a warning, not a note — so `af
// doctor` printed a clean report on a machine it had never managed to inspect.
// Every macOS user who ran doctor to ask "are there orphaned processes?" was
// told "no" by a program that could not have known either way.
//
// So blindness gets its own row, and that row is a FAIL: it contributes to the
// exit code, because "I could not look" is a broken doctor, not a healthy
// machine. Do not soften this to a Warn, and do not let the process checks
// below start reporting emptiness as health.
func checkProcessInspection(ctx *scanContext, report *Report) {
	if ctx.snap != nil {
		report.Pass(sectionProcesses, "process-inspection",
			fmt.Sprintf("read the process table (%d processes)", len(ctx.snap)))
		return
	}
	detail := fmt.Sprintf("cannot read the process table on %s, so the orphan, escaped-process and "+
		"runaway-CPU checks below could not run: %v", runtime.GOOS, ctx.snapErr)
	remediation := "the checks that depend on the process table are UNKNOWN, not clean — " +
		"do not read this report as evidence that no processes leaked"
	if errors.Is(ctx.snapErr, proctree.ErrUnsupportedPlatform) {
		remediation = fmt.Sprintf("af has no process-table backend for %s; the process checks cannot run here "+
			"and this report says nothing about leaked processes", runtime.GOOS)
	}
	report.Fail(sectionProcesses, "process-inspection", detail, remediation)
}

func checkOrphanedProcesses(ctx *scanContext, report *Report) {
	// Silent only because checkProcessInspection already reported the
	// blindness as a FAIL. Never make this branch the whole story again.
	if ctx.snap == nil {
		return
	}
	// A name missing from `live` is what arms the kill below, so an
	// unavailable listing cannot be allowed to empty this map: every live
	// session would read as dead and every one of its processes as a verified
	// orphan. Silent for the same reason as the branch above — checkTmuxInspection
	// already reported the blindness as a FAIL (#2874).
	names, listErr := ctx.tmuxSessions()
	if listErr != nil {
		return
	}
	live := map[string]bool{}
	for _, name := range names {
		live[name] = true
	}

	marked := map[string][]proctree.Process{}
	var possibles []proctree.Process
	var observations processLeakObservations
	for pid, p := range ctx.snap {
		if ctx.selfAncestors[pid] {
			continue
		}
		// EnvUnknown must NOT land in `marked`: that set is the killable
		// candidate pool, and a process we could not read is a process we
		// cannot claim is ours. It falls through to the report-only paths
		// below, which is the safe direction — we under-report an orphan
		// rather than act on one we never identified.
		name, nameStatus := proctree.LookupEnv(pid, tmux.EnvMarkerSession)
		if nameStatus == proctree.EnvFound && name != "" {
			if observations.oldEnough(ctx, p) {
				marked[name] = append(marked[name], p)
			}
			continue
		}
		if tmuxEnv, st := proctree.LookupEnv(pid, "TMUX"); st == proctree.EnvFound && tmuxServerDead(ctx, tmuxEnv) {
			if observations.oldEnough(ctx, p) {
				possibles = append(possibles, p)
			}
		}
	}

	for _, name := range sortedKeys(marked) {
		procs := marked[name]
		sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
		if live[name] {
			inSession := map[int]bool{}
			for _, p := range tmux.SessionProcessTrees(ctx.opts.Exec, name) {
				inSession[p.PID] = true
			}
			for _, p := range procs {
				if inSession[p.PID] || !observations.stillPresent(p) {
					continue
				}
				report.addAdvisoryFinding(Finding{
					Check: "escaped-process",
					Detail: fmt.Sprintf("%s escaped the pane tree of live session %s "+
						"(left alone while the session is alive)", describeProc(p), name),
				})
			}
			continue
		}
		for _, p := range procs {
			if !observations.stillPresent(p) {
				continue
			}
			// A kill requires a PROVEN home match, not just a dead-looking
			// session: another agent-factory home (e.g. a play-test sandbox
			// on a private `tmux -L` server) has sessions that are invisible
			// to this server's live list, so its perfectly healthy processes
			// would otherwise masquerade as verified orphans here. Foreign
			// or missing AF_HOME downgrades to report-only.
			// Only EnvFound may arm the kill. EnvUnknown (a denied or
			// redacted read) falls to the default arm — report-only —
			// because "I could not read its home" is not "its home is
			// mine".
			home, homeStatus := proctree.LookupEnv(p.PID, tmux.EnvMarkerHome)
			hasHome := homeStatus == proctree.EnvFound
			switch {
			case hasHome && filepath.Clean(home) == filepath.Clean(ctx.opts.ConfigDir):
				report.addActionableFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s was spawned by dead session %s (home %s)",
						describeProc(p), name, home),
					FixAction: fmt.Sprintf("kill pid %d", p.PID),
					fix:       killFix(ctx, p),
				})
			case hasHome:
				report.addAdvisoryFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks dead session %s but belongs to another "+
						"agent-factory home (%s) — its session may be live on that install's "+
						"private tmux server, so it is not fixed from here; run `af doctor` "+
						"with that home active", describeProc(p), name, home),
				})
			default:
				report.addAdvisoryFinding(Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks dead session %s but carries no readable "+
						"home marker — cannot prove which install owns it, so it is "+
						"reported, not killed", describeProc(p), name),
				})
			}
		}
	}

	// Sort by CPU so a core-burning `yes` outranks fifty idle shells, and
	// cap the listing — on a long-lived dev box this class is numerous and
	// mostly idle, and it is report-only by definition.
	type ranked struct {
		p    proctree.Process
		frac float64
	}
	rankedPossibles := make([]ranked, 0, len(possibles))
	for _, p := range possibles {
		if !observations.stillPresent(p) {
			continue
		}
		frac, _, _ := proctree.CPUFraction(p)
		rankedPossibles = append(rankedPossibles, ranked{p, frac})
	}
	sort.Slice(rankedPossibles, func(i, j int) bool {
		if rankedPossibles[i].frac != rankedPossibles[j].frac {
			return rankedPossibles[i].frac > rankedPossibles[j].frac
		}
		return rankedPossibles[i].p.PID < rankedPossibles[j].p.PID
	})
	const maxPossibleOrphans = 15
	for i, r := range rankedPossibles {
		if i == maxPossibleOrphans {
			report.addAdvisoryFinding(Finding{
				Check: "possible-orphan",
				Detail: fmt.Sprintf("… and %d more processes of dead tmux servers (all idle or near-idle; "+
					"none carry an agent-factory marker, so none are killed)", len(rankedPossibles)-maxPossibleOrphans),
			})
			break
		}
		report.addAdvisoryFinding(Finding{
			Check: "possible-orphan",
			Detail: fmt.Sprintf("%s belongs to a dead tmux server (no agent-factory marker — "+
				"cannot verify ownership, so it is reported, not killed)", describeProc(r.p)),
		})
	}
	observations.report(report)
}

// processLeakObservations keeps every unprovable state visible without mixing
// it into the leak counts. A vanished process is normal churn and needs no row;
// missing age or an identity-read failure means the check was blind.
type processLeakObservations struct {
	unknownAge      int
	unknownIdentity int
}

func (o *processLeakObservations) oldEnough(ctx *scanContext, p proctree.Process) bool {
	if p.StartedAt.IsZero() {
		o.unknownAge++
		return false
	}
	return ctx.snapAt.Sub(p.StartedAt) >= ctx.opts.minProcessLeakAge
}

func (o *processLeakObservations) stillPresent(p proctree.Process) bool {
	same, err := proctree.SameIdentity(p)
	if err != nil {
		o.unknownIdentity++
		return false
	}
	return same
}

func (o processLeakObservations) report(report *Report) {
	var blind []string
	if o.unknownAge > 0 {
		blind = append(blind, fmt.Sprintf("could not determine the age of %s",
			plural(o.unknownAge, "candidate process", "candidate processes")))
	}
	if o.unknownIdentity > 0 {
		blind = append(blind, fmt.Sprintf("could not revalidate the identity of %s",
			plural(o.unknownIdentity, "candidate process", "candidate processes")))
	}
	if len(blind) == 0 {
		return
	}
	report.Warn(sectionProcesses, "process-leak-inspection",
		strings.Join(blind, " and ")+"; they are omitted from the escaped, orphaned, and possible-orphan counts",
		"those candidates are UNKNOWN, not proven leaks; rerun doctor or inspect them manually", false)
}

// tmuxServerDead parses a TMUX env value ("socketPath,serverPID,sessionIdx")
// and reports whether the process identity it records still belongs to tmux.
// Unparseable values are treated as alive (never accuse on garbage). The socket
// path is deliberately not a liveness signal: a crashed server can leave it
// behind after the kernel recycles the recorded PID to an unrelated process.
func tmuxServerDead(ctx *scanContext, tmuxEnv string) bool {
	parts := strings.Split(tmuxEnv, ",")
	if len(parts) < 2 {
		return false
	}
	serverPID, err := strconv.Atoi(parts[1])
	if err != nil || serverPID <= 0 {
		return false
	}
	server, alive := ctx.snap[serverPID]
	if alive && strings.HasPrefix(server.Comm, "tmux") {
		return false
	}
	// PID gone or recycled to a non-tmux process: the server named by this
	// environment value is dead, regardless of any stale or reused socket path.
	return true
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
