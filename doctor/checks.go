package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	daemonProcessArgv   = daemon.ProcessArgv
	daemonPIDLooksAlive = daemon.PIDLooksAlive
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
// (restarting the daemon is a user decision).
func checkDaemonHealth(ctx *scanContext, report *Report) {
	h := daemon.Health()
	if h.SocketErr != nil {
		report.Findings = append(report.Findings, Finding{
			Check:  "daemon-health",
			Detail: fmt.Sprintf("cannot resolve daemon socket path: %v", h.SocketErr),
		})
		return
	}
	unit := "ad-hoc (no autostart unit; `af daemon install` sets one up)"
	if h.AutostartUnit {
		unit = "autostart unit installed"
	}
	switch {
	case h.PingErr == nil:
		report.OK = append(report.OK, fmt.Sprintf("daemon: responding on %s; %s", h.SocketPath, unit))
	case !h.SocketExists:
		report.OK = append(report.OK, "daemon: not running (starts on demand); "+unit)
	default:
		report.Findings = append(report.Findings, Finding{
			Check: "daemon-health",
			Detail: fmt.Sprintf("socket %s exists but the daemon is not responding (%v) — "+
				"likely a stale socket from a crashed daemon", h.SocketPath, h.PingErr),
		})
	}
	if h.PIDFilePID > 0 && !h.PIDVerified && h.PingErr != nil {
		report.Findings = append(report.Findings, Finding{
			Check: "daemon-health",
			Detail: fmt.Sprintf("daemon.pid records pid %d but no agent-factory daemon is running under it "+
				"(stale pid file)", h.PIDFilePID),
		})
	}
	if h.BinaryDeleted {
		report.Findings = append(report.Findings, Finding{
			Check: "daemon-health",
			Detail: fmt.Sprintf("daemon pid %d is running a binary that was since replaced on disk — "+
				"it will keep running the old version until restarted", h.PIDFilePID),
		})
	}
}

// checkOrphanedProcesses finds processes carrying the AF_SESSION ancestry
// marker. Marker + dead session = verified orphan (killable with --fix);
// marker + live session but outside its pane trees = escaped (report-only);
// no marker but a TMUX env var pointing at a dead server = possible orphan
// (report-only — could belong to any tmux, not necessarily agent-factory).
func checkOrphanedProcesses(ctx *scanContext, report *Report) {
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
		if name, ok := proctree.EnvValue(pid, tmux.EnvMarkerSession); ok && name != "" {
			marked[name] = append(marked[name], p)
			continue
		}
		if tmuxEnv, ok := proctree.EnvValue(pid, "TMUX"); ok && tmuxServerDead(ctx, tmuxEnv) {
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
			home, hasHome := proctree.EnvValue(p.PID, tmux.EnvMarkerHome)
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
	if ctx.snap == nil {
		return
	}
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
			if home, ok := proctree.EnvValue(procs[0].PID, tmux.EnvMarkerHome); ok {
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
				"kill it with: tmux kill-session -t '=%s:'", name, ctx.opts.ConfigDir, origin, name),
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
	homesInUse := processReferencedHomes(ctx.snap)
	tmuxHomesInUse := liveTmuxHomes(ctx)

	for _, dir := range candidateTempHomes(tempDir) {
		dir = filepath.Clean(dir)
		if dir == activeHome || !isAFHome(dir) {
			continue
		}
		if reason := tempHomeInUseReason(dir, homesInUse, tmuxHomesInUse); reason != "" {
			report.OK = append(report.OK, fmt.Sprintf("temp home %s is in use (%s)", dir, reason))
			continue
		}
		age := timeSince(newestMtime(dir))
		if age < ctx.opts.MinTempHomeAge {
			continue
		}
		// Containment re-check before offering an rm -rf.
		if !pathInside(tempDir, dir) {
			continue
		}
		removeDir := dir
		report.Findings = append(report.Findings, Finding{
			Check: "stale-temp-home",
			Detail: fmt.Sprintf("abandoned agent-factory home %s (untouched for %s)",
				dir, formatAge(age.Seconds())),
			FixAction: "remove " + dir,
			fix:       staleTempHomeRemoveFix(ctx, removeDir),
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

func processReferencedHomes(snap map[int]proctree.Process) map[string]bool {
	homes := map[string]bool{}
	if snap == nil {
		return homes
	}
	for pid := range snap {
		if home, ok := proctree.EnvValue(pid, "AGENT_FACTORY_HOME"); ok && home != "" {
			homes[filepath.Clean(home)] = true
		}
	}
	return homes
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

func tempHomeInUseReason(dir string, processHomes, tmuxHomes map[string]bool) string {
	dir = filepath.Clean(dir)
	switch {
	case processHomes[dir]:
		return "live process references it"
	case tmuxHomes[dir]:
		return "live tmux session references it"
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

func staleTempHomeRemoveFix(ctx *scanContext, dir string) func() error {
	return func() error {
		dir := filepath.Clean(dir)
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("cannot stat %s before removal: %w", dir, err)
		}
		if !pathInside(filepath.Clean(ctx.opts.TempDir), dir) {
			return fmt.Errorf("refusing to remove %s outside temp dir %s", dir, ctx.opts.TempDir)
		}
		if filepath.Clean(ctx.opts.ConfigDir) == dir {
			return fmt.Errorf("refusing to remove active agent-factory home %s", dir)
		}
		if !isAFHome(dir) {
			return fmt.Errorf("refusing to remove %s: it no longer looks like an agent-factory home", dir)
		}

		snap := ctx.snap
		if ctx.opts.snapshot != nil {
			if fresh, err := ctx.opts.snapshot(); err == nil {
				snap = fresh
			}
		}
		if reason := tempHomeInUseReason(dir, processReferencedHomes(snap), liveTmuxHomes(ctx)); reason != "" {
			return fmt.Errorf("refusing to remove %s: %s", dir, reason)
		}
		return os.RemoveAll(dir)
	}
}

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
	defaultHome := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultHome = filepath.Join(home, ".agent-factory")
	}
	activeHome := filepath.Clean(ctx.opts.ConfigDir)

	pids := make([]int, 0, len(ctx.snap))
	for pid := range ctx.snap {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if ctx.selfAncestors[pid] {
			continue
		}
		p := ctx.snap[pid]
		args := daemonProcessArgv(pid)
		if len(args) == 0 || !daemon.LooksLikeDaemonArgv(args) {
			continue
		}
		home, ok := proctree.EnvValue(pid, "AGENT_FACTORY_HOME")
		if !ok || home == "" {
			home = defaultHome
		}
		home = filepath.Clean(home)
		if home == activeHome || home == "" {
			continue // this install's daemon; covered by checkDaemonHealth
		}
		if _, err := os.Stat(home); err != nil {
			report.Findings = append(report.Findings, Finding{
				Check: "foreign-daemon",
				Detail: fmt.Sprintf("%s serves agent-factory home %s which no longer exists "+
					"(abandoned daemon will run its cron tasks forever)", describeProc(p), home),
				FixAction: fmt.Sprintf("kill pid %d", pid),
				fix:       killFix(ctx, p),
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
