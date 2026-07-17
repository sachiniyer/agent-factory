package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func selfPID() int { return os.Getpid() }

func tempDirDefault() string { return os.TempDir() }

var (
	daemonProcessArgv   = daemon.ProcessArgv
	daemonPIDLooksAlive = daemon.PIDLooksAlive
	// The per-process facts that decide whether a daemon is OURS (#1044).
	// Injectable for the same reason as the two above: the states that matter —
	// another user's process, an environ we may not read — cannot be staged by
	// a test without root, and whether they hold for any given pid depends on
	// the machine (in a container pid 1 is often the test user itself).
	daemonProcessEnvLookup = proctree.EnvLookup
	daemonProcessOwnerUID  = proctree.OwnerUID
	daemonProcessCwd       = proctree.WorkingDir
)

// runawayCPUFraction and runawayMinAge define "pegging a core for an
// extended period": lifetime-average CPU ≥ 80% of a core for a process at
// least 30 minutes old. A legitimate build rarely sustains that average; the
// leaked `yes` processes from the outage sat at ~100% for 15 days.
const (
	runawayCPUFraction = 0.8
	runawayMinAge      = 30 * time.Minute
)

// listTmuxSessions returns every session name on the current tmux server.
// A missing server (exit 1) is an empty list, mirroring CleanupSessions.
func listTmuxSessions(ctx *scanContext) []string {
	out, err := ctx.opts.Exec.Output(exec.Command("tmux", "ls", "-F", "#{session_name}"))
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

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
func checkDaemonHealth(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if h.SocketErr != nil {
		report.Fail(sectionDaemon, "daemon", fmt.Sprintf("cannot resolve daemon socket path: %v", h.SocketErr),
			"fix AGENT_FACTORY_HOME and rerun `af doctor`")
		return
	}
	switch {
	case h.PingErr == nil:
		report.Pass(sectionDaemon, "daemon", "responding on "+h.SocketPath)
	case !h.SocketExists:
		report.Pass(sectionDaemon, "daemon", "not running; starts on demand")
	default:
		report.Fail(sectionDaemon, "daemon", fmt.Sprintf("socket %s exists but the daemon is not responding (%v)", h.SocketPath, h.PingErr),
			"run `af daemon restart`; if it still fails, remove the stale socket after verifying no daemon is running")
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
	live := map[string]bool{}
	for _, name := range listTmuxSessions(ctx) {
		live[name] = true
	}

	marked := map[string][]proctree.Process{}
	var possibles []proctree.Process
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
			marked[name] = append(marked[name], p)
			continue
		}
		if tmuxEnv, st := proctree.LookupEnv(pid, "TMUX"); st == proctree.EnvFound && tmuxServerDead(ctx, tmuxEnv) {
			possibles = append(possibles, p)
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
				if inSession[p.PID] {
					continue
				}
				report.Findings = append(report.Findings, Finding{
					Check: "escaped-process",
					Detail: fmt.Sprintf("%s escaped the pane tree of live session %s "+
						"(left alone while the session is alive)", describeProc(p), name),
				})
			}
			continue
		}
		for _, p := range procs {
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
				report.Findings = append(report.Findings, Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s was spawned by dead session %s (home %s)",
						describeProc(p), name, home),
					FixAction: fmt.Sprintf("kill pid %d", p.PID),
					fix:       killFix(ctx, p),
				})
			case hasHome:
				report.Findings = append(report.Findings, Finding{
					Check: "orphaned-process",
					Detail: fmt.Sprintf("%s marks dead session %s but belongs to another "+
						"agent-factory home (%s) — its session may be live on that install's "+
						"private tmux server, so it is not fixed from here; run `af doctor` "+
						"with that home active", describeProc(p), name, home),
				})
			default:
				report.Findings = append(report.Findings, Finding{
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
			report.Findings = append(report.Findings, Finding{
				Check: "possible-orphan",
				Detail: fmt.Sprintf("… and %d more processes of dead tmux servers (all idle or near-idle; "+
					"none carry an agent-factory marker, so none are killed)", len(rankedPossibles)-maxPossibleOrphans),
			})
			break
		}
		report.Findings = append(report.Findings, Finding{
			Check: "possible-orphan",
			Detail: fmt.Sprintf("%s belongs to a dead tmux server (no agent-factory marker — "+
				"cannot verify ownership, so it is reported, not killed)", describeProc(r.p)),
		})
	}
}

// tmuxServerDead parses a TMUX env value ("socketPath,serverPID,sessionIdx")
// and reports whether the server it names is gone. Unparseable values are
// treated as alive (never accuse on garbage).
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
	// PID gone or recycled to a non-tmux process; confirm via the socket.
	if _, err := os.Stat(parts[0]); err == nil && alive {
		// Socket still present and some process holds the PID — too
		// ambiguous to call dead.
		return false
	}
	return true
}

// checkRunawayChildren reports (never kills) descendants of live af_
// sessions that have averaged a pegged core for an extended period.
func checkRunawayChildren(ctx *scanContext, report *Report) {
	// See checkOrphanedProcesses: blindness is reported once, by
	// checkProcessInspection, rather than swallowed here.
	if ctx.snap == nil {
		return
	}
	unmeasurable := 0
	for _, name := range listTmuxSessions(ctx) {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		procs := tmux.SessionProcessTrees(ctx.opts.Exec, name)
		sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
		for _, p := range procs {
			if ctx.selfAncestors[p.PID] {
				continue
			}
			frac, age, err := proctree.CPUFraction(p)
			if errors.Is(err, proctree.ErrCPUUnknown) {
				// Counted, not swallowed. A check that cannot answer must say
				// so: silently skipping every process would render "no runaway
				// processes" — the exact shape of the report this package
				// exists to stop printing. Reported once below rather than per
				// process, since the cause is usually systemic (a subset=pid
				// procfs hides /proc/uptime, so NO process has an age).
				unmeasurable++
				continue
			}
			if err != nil || frac < runawayCPUFraction || age < runawayMinAge.Seconds() {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Check: "runaway-cpu",
				Detail: fmt.Sprintf("%s in live session %s has averaged a pegged core — "+
					"check the session; doctor never kills children of live sessions", describeProc(p), name),
			})
		}
	}
	if unmeasurable > 0 {
		report.Warn(sectionProcesses, "runaway-cpu",
			fmt.Sprintf("could not measure CPU for %s in live sessions, so this check reports nothing "+
				"about them", plural(unmeasurable, "process", "processes")),
			"a process pegging a core would not be spotted here; inspect the sessions yourself", false)
	}
}

// checkLeakedTmuxSessions reports af_ tmux sessions with no backing record
// in this home's storage. Report-only even under --fix: a session with no
// record here may be owned by another agent-factory home on the same tmux
// server, and killing someone else's live session is worse than a leak.
func checkLeakedTmuxSessions(ctx *scanContext, report *Report) {
	recorded := recordedTmuxNames(ctx.opts.ConfigDir)
	var leaked []string
	for _, name := range listTmuxSessions(ctx) {
		if strings.HasPrefix(name, tmux.TmuxPrefix) && !recorded[name] {
			leaked = append(leaked, name)
		}
	}
	sort.Strings(leaked)
	for _, name := range leaked {
		origin := "no ancestry marker"
		if procs := tmux.SessionProcessTrees(ctx.opts.Exec, name); len(procs) > 0 {
			if home, st := proctree.LookupEnv(procs[0].PID, tmux.EnvMarkerHome); st == proctree.EnvFound {
				if filepath.Clean(home) == filepath.Clean(ctx.opts.ConfigDir) {
					origin = "created by this install"
				} else {
					origin = "created by another agent-factory home: " + home
				}
			}
		}
		report.Findings = append(report.Findings, Finding{
			Check: "leaked-tmux-session",
			Detail: fmt.Sprintf("tmux session %s has no backing record in %s (%s); "+
				"kill it with: %s", name, ctx.opts.ConfigDir, origin,
				// "=name:" is tmux's exact-match target syntax — one argument, so
				// it is one piece the seam quotes as a whole.
				shellsuggest.Command("tmux", "kill-session", "-t", "="+name+":")),
		})
	}
}

// recordedTmuxNames loads every persisted tmux session name (agent + tabs)
// from this home's storage, read-only. Legacy records without an explicit
// TmuxName fall back to the derived repo-scoped name.
func recordedTmuxNames(configDir string) map[string]bool {
	names := map[string]bool{}
	// Doctor may be pointed at a ConfigDir other than the ambient one only
	// in tests, which also set AGENT_FACTORY_HOME — LoadAllRepoInstances
	// always reads the ambient home.
	all, err := config.LoadAllRepoInstances()
	if err != nil {
		return names
	}
	type tabRec struct {
		TmuxName string `json:"tmux_name"`
	}
	type instRec struct {
		Title    string   `json:"title"`
		Path     string   `json:"path"`
		TmuxName string   `json:"tmux_name"`
		Tabs     []tabRec `json:"tabs"`
	}
	for _, raw := range all {
		var instances []instRec
		if err := json.Unmarshal(raw, &instances); err != nil {
			continue
		}
		for _, inst := range instances {
			if inst.TmuxName != "" {
				names[inst.TmuxName] = true
			} else if inst.Title != "" {
				names[tmux.NewTmuxSessionForRepo(inst.Title, inst.Path, "").SanitizedName()] = true
			}
			for _, tab := range inst.Tabs {
				if tab.TmuxName != "" {
					names[tab.TmuxName] = true
				}
			}
		}
	}
	return names
}

// afHomeMarkers are the files/dirs whose presence identifies a directory as
// an agent-factory home. Two or more must match before doctor will even
// report a directory, let alone remove it.
var afHomeMarkers = []string{
	config.TomlConfigFileName, config.ConfigFileName, "state.json", "instances", "daemon.sock", "daemon.pid", "agent-factory.log",
}

// checkStaleTempHomes finds abandoned agent-factory homes under the temp
// dir (leaked by tests/debug runs — the #1093 immortal-daemon fuel). A home
// is stale only when nothing references it: no live process has it as
// AGENT_FACTORY_HOME, no live tmux session marks it as AF_HOME, its
// daemon.pid (if any) is verified absent/dead/stale rather than merely
// unreadable, and it has not been touched for MinTempHomeAge.
func checkStaleTempHomes(ctx *scanContext, report *Report) {
	tempDir := filepath.Clean(ctx.opts.TempDir)
	activeHome := filepath.Clean(ctx.opts.ConfigDir)
	homesInUse, unreadableProcs := processReferencedHomes(ctx.snap)
	tmuxHomesInUse := liveTmuxHomes(ctx)

	for _, dir := range candidateTempHomes(tempDir) {
		dir = filepath.Clean(dir)
		if dir == activeHome || !isAFHome(dir) {
			continue
		}
		if reason := tempHomeInUseReason(dir, homesInUse, tmuxHomesInUse, unreadableProcs); reason != "" {
			report.Pass(sectionProcesses, "temp home", fmt.Sprintf("%s is in use (%s)", dir, reason))
			continue
		}
		age := timeSince(newestMtime(dir))
		if age < ctx.opts.MinTempHomeAge {
			continue
		}
		if !pathInside(tempDir, dir) {
			continue
		}
		// REPORT-ONLY. No FixAction, no fix closure: `af doctor --fix` will
		// not delete this, and that is deliberate.
		//
		// This finding used to carry an rm -rf, gated on "no process
		// references this home". FOUR consecutive P1 reviews found four
		// different ways to make that negative false, and every one of them
		// ended at this same delete:
		//
		//   1. darwin redacts a foreign process's environment silently, so an
		//      unread env read as "references no home".
		//   2. the permission gate written to fix (1) modelled uid but not
		//      CS_RESTRICT, which SIP makes ordinary — so a same-uid restricted
		//      process read as "references no home".
		//   3. (the gate was deleted; classify the answer instead.)
		//   4. the uid filter that survived assumed temp homes sit under a
		//      user-owned root. Linux's /tmp is 01777 and shared, so another
		//      user's home lives there and their processes read as
		//      "references no home".
		//
		// That is not four bugs. It is one unsound question: "can I prove
		// nobody is using this?" cannot be answered by inspecting processes.
		// Process inspection is inference over a surface that is adversarial by
		// construction — kernels redact, policies grow clauses we did not
		// model, /proc can be mounted subset=pid, /tmp is shared with users we
		// cannot see, PIDs recycle. Each of those turns "I saw no user" into
		// "there is no user", and that negative authorised an rm -rf.
		//
		// So the teeth come out until the predicate is a FACT rather than an
		// inference: a lockfile the owning process holds for its lifetime,
		// where "is this in use?" is answered by trying to take the lock and
		// the kernel is the authority (see the follow-up issue; af homes
		// already have exactly such a lock in daemon/singleton_lock.go).
		//
		// Reporting is honest; authorising is dangerous; they are not the same
		// code path. Doctor still names the directory and still says what it
		// could not verify — the operator decides.
		report.Findings = append(report.Findings, Finding{
			Check: "stale-temp-home",
			Detail: fmt.Sprintf("agent-factory home %s looks abandoned (untouched for %s), but nothing "+
				"here can PROVE it is unused — inspect it and remove it yourself if it is dead",
				dir, formatAge(age.Seconds())),
			Severity:    StatusWarn,
			Remediation: "verify nothing is using it, then `rm -rf " + dir + "`",
		})
	}
}

// timeSince is time.Since, indirected so tests can pin the clock if needed.
var timeSince = time.Since

// candidateTempHomes lists directories one and two levels below tempDir —
// Go tests produce /tmp/TestName123/001-style homes, manual runs
// /tmp/tmp.XXXX ones.
func candidateTempHomes(tempDir string) []string {
	var out []string
	level1, err := os.ReadDir(tempDir)
	if err != nil {
		return nil
	}
	for _, e := range level1 {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tempDir, e.Name())
		out = append(out, dir)
		level2, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e2 := range level2 {
			if e2.IsDir() {
				out = append(out, filepath.Join(dir, e2.Name()))
			}
		}
	}
	return out
}

func isAFHome(dir string) bool {
	found := 0
	for _, marker := range afHomeMarkers {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			found++
		}
	}
	return found >= 2
}

// processReferencedHomes returns the AF homes live processes name in their
// environment, AND the number of processes whose environment could not be read.
//
// The second return is the whole point, and dropping it would restore a
// DESTRUCTIVE bug. This feeds tempHomeInUseReason, whose negative — "no process
// references this home" — is the predicate for os.RemoveAll. A process whose
// environment we could not read might be using the home; if its unreadability
// is silently folded into "does not reference it", doctor --fix deletes a home
// that is in use, on the strength of a read that was denied.
//
// This is not hypothetical on darwin: the kernel redacts a foreign process's
// environment and the buffer looks identical to one with no variables at all
// (see internal/proctree's darwin readEnviron). Linux says EACCES honestly, but
// the caller cannot depend on the platform to be honest for it.
//
// Only OUR OWN processes count toward unreadable. A home under this user's
// 0700 temp dir cannot be in use by another user's process, so a foreign
// process being unreadable tells us nothing we needed to know — and counting it
// would make --fix refuse forever on any real machine, where most of the
// process table belongs to root.
func processReferencedHomes(snap map[int]proctree.Process) (homes map[string]bool, unreadable int) {
	homes = map[string]bool{}
	if snap == nil {
		return homes, 0
	}
	self := os.Getuid()
	for pid := range snap {
		home, status := proctree.LookupEnv(pid, "AGENT_FACTORY_HOME")
		switch status {
		case proctree.EnvFound:
			if home != "" {
				homes[filepath.Clean(home)] = true
			}
		case proctree.EnvAbsent:
			// Read, and it names no home: it cannot be using one.
		default:
			// EnvUnknown. Narrow it to the processes whose unreadability
			// actually costs us knowledge, or --fix would refuse forever:
			// on a real machine most of the table is root's, and darwin
			// redacts every one of those environments.
			uid, ok := proctree.UID(pid)
			switch {
			case !ok:
				// No credentials at all means the process is gone — it was
				// in the snapshot and has since exited. A dead process uses
				// nothing, so its silence costs us nothing. (Ownership comes
				// from the process table, which is readable for ANY process
				// on both platforms, so a failure here is absence of the
				// process rather than absence of permission.)
			case uid != self:
				// Another user's process. It cannot be using a home under
				// this user's 0700 temp dir, so whether we can read its
				// environment is irrelevant to the question being asked.
			default:
				// Ours, alive, and unreadable: precisely the case we cannot
				// rule out.
				unreadable++
			}
		}
	}
	return homes, unreadable
}

func liveTmuxHomes(ctx *scanContext) map[string]bool {
	homes := map[string]bool{}
	for _, name := range listTmuxSessions(ctx) {
		if !strings.HasPrefix(name, tmux.TmuxPrefix) {
			continue
		}
		if home, ok := tmuxSessionHomeMarker(ctx, name); ok && home != "" {
			homes[filepath.Clean(home)] = true
		}
	}
	return homes
}

func tmuxSessionHomeMarker(ctx *scanContext, name string) (string, bool) {
	out, err := ctx.opts.Exec.Output(exec.Command("tmux", "show-environment", "-t",
		fmt.Sprintf("=%s:", name), tmux.EnvMarkerHome))
	if err != nil {
		return "", false
	}
	return strings.CutPrefix(strings.TrimSpace(string(out)), tmux.EnvMarkerHome+"=")
}

type tempHomeDaemonStatus int

const (
	tempHomeDaemonAbsentOrDead tempHomeDaemonStatus = iota
	tempHomeDaemonAlive
	tempHomeDaemonUnknown
)

// tempHomeDaemonLiveness reports whether the home's daemon.pid names a live
// agent-factory daemon. A live PID with unreadable argv is unknown, not dead:
// doctor must not delete a home when daemon liveness cannot be verified.
func tempHomeDaemonLiveness(dir string) tempHomeDaemonStatus {
	data, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
	if err != nil {
		if os.IsNotExist(err) {
			return tempHomeDaemonAbsentOrDead
		}
		return tempHomeDaemonUnknown
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return tempHomeDaemonUnknown
	}
	if !daemonPIDLooksAlive(pid) {
		return tempHomeDaemonAbsentOrDead
	}
	args := daemonProcessArgv(pid)
	if len(args) == 0 {
		return tempHomeDaemonUnknown
	}
	if daemon.LooksLikeDaemonArgv(args) {
		return tempHomeDaemonAlive
	}
	return tempHomeDaemonAbsentOrDead
}

// tempHomeInUseReason returns why dir must not be removed, or "" when it is
// provably unused. unreadableProcs is how many of this user's processes have an
// environment we could not read: each one is a process that MIGHT reference
// dir, so a non-zero count means "unused" cannot be proven and the directory
// stays. That mirrors the tempHomeDaemonUnknown arm below — this function
// already knew that uncertainty is a reason to keep, not to delete.
func tempHomeInUseReason(dir string, processHomes, tmuxHomes map[string]bool, unreadableProcs int) string {
	dir = filepath.Clean(dir)
	switch {
	case processHomes[dir]:
		return "live process references it"
	case tmuxHomes[dir]:
		return "live tmux session references it"
	case unreadableProcs > 0:
		return fmt.Sprintf("%d of this user's processes have an unreadable environment, so nothing proves "+
			"they are not using it", unreadableProcs)
	}
	switch tempHomeDaemonLiveness(dir) {
	case tempHomeDaemonAlive:
		return "daemon pid is live"
	case tempHomeDaemonUnknown:
		return "daemon.pid liveness is uncertain"
	default:
		return ""
	}
}

// staleTempHomeRemoveFix is DELETED. Do not reintroduce it without the lock.
//
// It was the --fix closure that rm -rf'd an "abandoned" temp home, and it was
// careful: containment checks, an active-home check, an isAFHome check, and a
// fresh snapshot at fix time. None of that was the problem. Its PREDICATE was:
// it asked "did I see any process referencing this home?" and treated "no" as
// proof. Four consecutive P1 reviews found four different ways for that "no" to
// be false (see checkStaleTempHomes), each one ending here, in an rm -rf.
//
// The care was real and it did not help, because you cannot make an unsound
// question safe by validating its inputs harder. The delete returns when the
// predicate is a fact the kernel guarantees — try to take the home's lock —
// rather than an inference we assemble from a surface that redacts, restricts
// and shares. See the follow-up issue.

func pathInside(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// newestMtime returns the most recent mtime among the dir itself and its
// marker files — a fair "last touched" signal without a full tree walk.
func newestMtime(dir string) time.Time {
	newest := time.Time{}
	consider := func(path string) {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	consider(dir)
	for _, marker := range afHomeMarkers {
		consider(filepath.Join(dir, marker))
	}
	return newest
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
			report.Findings = append(report.Findings, Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves agent-factory home %s which no longer exists "+
					"(abandoned daemon will run its cron tasks forever)", describeProc(p), home),
				FixAction: fmt.Sprintf("kill pid %d", pid),
				fix:       killFix(ctx, p),
			})
		} else if err != nil {
			report.Findings = append(report.Findings, Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves agent-factory home %s whose status cannot be verified: %v",
					describeProc(p), home, err),
			})
		} else {
			report.Findings = append(report.Findings, Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves a different agent-factory home (%s) — "+
					"left alone in case it is intentional", describeProc(p), home),
			})
		}
	}
}
