package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// This file holds the skew family of checks (#1044): the ways a client and the
// daemon it talks to can drift apart. They share one failure mode — the daemon
// keeps answering, so nothing looks broken, while requests from a newer client
// are rejected field-by-field ("unknown field <name>", because the HTTP handler
// decodes with DisallowUnknownFields) and the UI hangs waiting for a reply that
// will never come. Every check here answers the same question from a different
// angle: is the daemon you are talking to the one your binary expects?

// devVersion is the version string of an unreleased build. Two dev builds can
// legitimately differ in content while reporting the same string, and a dev
// client against a released daemon is a routine state on a developer's box, so
// skew involving one is reported but never failed.
const devVersion = "dev"

// binaryProbeTimeout bounds each `af version` subprocess so a wedged binary on
// some stale path cannot hang the whole doctor run.
const binaryProbeTimeout = 5 * time.Second

// checkDaemonVersionSkew compares the responding daemon's version against this
// client's. This is the check that catches the live bug: an old daemon left
// running across an upgrade answers pings perfectly while rejecting every new
// field the upgraded client sends.
func checkDaemonVersionSkew(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if h.PingErr != nil {
		// Nothing answered, so there is no version to compare — whether that
		// is fine (starts on demand) or broken (stale socket) is
		// checkDaemonHealth's call, not ours.
		return
	}
	client := strings.TrimSpace(ctx.opts.Version)
	served := strings.TrimSpace(h.DaemonVersion)

	switch {
	case client == "" || client == devVersion:
		report.Warn(sectionDaemon, "daemon version",
			fmt.Sprintf("daemon reports %s; this client is an unreleased build, so skew cannot be judged",
				describeVersion(served)),
			"compare against a released af build if the UI misbehaves", false)
	case served == "":
		// A daemon that answers but reports no version predates version
		// reporting itself, which places it strictly older than this client.
		report.Fail(sectionDaemon, "daemon version",
			fmt.Sprintf("the running daemon predates version reporting but your af client is %s; "+
				"a version-skewed daemon rejects newer requests (e.g. \"unknown field\") and can hang the UI", client),
			"restart it: af daemon restart")
	case served != client:
		report.Fail(sectionDaemon, "daemon version",
			fmt.Sprintf("daemon is running %s but your af client is %s; "+
				"a version-skewed daemon rejects newer requests (e.g. \"unknown field\") and can hang the UI", served, client),
			"restart it: af daemon restart")
	default:
		report.Pass(sectionDaemon, "daemon version", "matches this client ("+client+")")
	}
}

func describeVersion(v string) string {
	if v == "" {
		return "no version (a build older than version reporting)"
	}
	return v
}

// checkDuplicateDaemons finds more than one daemon serving the active home.
// The singleton invariant is enforced by a startup lock, so a second daemon
// here means one escaped it — typically an old binary's daemon that kept the
// socket while a new one was installed. Scoped to this home: daemons serving
// other homes are checkForeignDaemons' business, and processes belonging to
// other users never appear in our snapshot.
func checkDuplicateDaemons(ctx *scanContext, report *Report) {
	if ctx.snap == nil {
		return
	}
	procs := activeHomeDaemons(ctx)
	if len(procs) <= 1 {
		// 0 daemons is not this check's problem (the daemon starts on demand);
		// 1 is the invariant holding. Neither is worth a row of its own.
		return
	}
	descs := make([]string, 0, len(procs))
	for _, p := range procs {
		descs = append(descs, daemonProcSummary(p))
	}
	report.Fail(sectionDaemon, "daemon instances",
		fmt.Sprintf("%d daemons are serving this agent-factory home (%s); "+
			"a second or stale daemon is a common cause of version skew",
			len(procs), strings.Join(descs, ", ")),
		"stop the extras — `af reset` now stops all af daemons — then let one restart")
}

// daemonProc pairs a daemon process with the agent-factory home it serves.
type daemonProc struct {
	proc proctree.Process
	home string
}

// scanDaemons returns every agent-factory daemon in the snapshot paired with
// the home it serves, in PID order.
//
// One scan, shared by both daemon checks: classifying a process means reading
// its argv and environ, which is a /proc read per process, and "is this a
// daemon, and whose?" must have exactly one answer — checkDuplicateDaemons
// counting a process that checkForeignDaemons would call foreign is a
// contradiction, not two opinions.
func scanDaemons(ctx *scanContext) []daemonProc {
	defaultHome := defaultAFHome()
	var out []daemonProc
	for pid, p := range ctx.snap {
		if ctx.selfAncestors[pid] {
			continue
		}
		args := daemonProcessArgv(pid)
		if len(args) == 0 || !daemon.LooksLikeDaemonArgv(args) {
			continue
		}
		out = append(out, daemonProc{proc: p, home: daemonProcessHome(pid, defaultHome)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].proc.PID < out[j].proc.PID })
	return out
}

// daemonProcs returns the run's daemon scan, computing it at most once. The
// doctor run is sequential, so a plain memo is enough.
func (c *scanContext) daemonProcs() []daemonProc {
	if !c.daemonsScanned {
		c.daemons = scanDaemons(c)
		c.daemonsScanned = true
	}
	return c.daemons
}

// activeHomeDaemons returns the daemon processes serving the active home.
func activeHomeDaemons(ctx *scanContext) []proctree.Process {
	activeHome := filepath.Clean(ctx.opts.ConfigDir)
	var out []proctree.Process
	for _, d := range ctx.daemonProcs() {
		if d.home == activeHome {
			out = append(out, d.proc)
		}
	}
	return out
}

// defaultAFHome is the home a daemon serves when it carries no explicit
// AGENT_FACTORY_HOME.
func defaultAFHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agent-factory")
}

// daemonProcessHome resolves which agent-factory home pid serves.
func daemonProcessHome(pid int, defaultHome string) string {
	home, ok := proctree.EnvValue(pid, "AGENT_FACTORY_HOME")
	if !ok || home == "" {
		home = defaultHome
	}
	return filepath.Clean(home)
}

// daemonProcSummary identifies a daemon process by pid and age, which is what
// distinguishes the stale daemon from the fresh one at a glance.
func daemonProcSummary(p proctree.Process) string {
	if _, age, err := proctree.CPUFraction(p); err == nil {
		return fmt.Sprintf("pid %d started %s ago", p.PID, formatAge(age))
	}
	return fmt.Sprintf("pid %d", p.PID)
}

// checkAutostartPath compares the binary the autostart unit launches against
// the binary running this command. When they differ, every upgrade that lands
// on the client's path leaves the supervised daemon respawning the old one —
// so the skew survives restarts and reboots, and `af daemon restart` never
// fixes it.
func checkAutostartPath(ctx *scanContext, report *Report) {
	info := ctx.opts.autostartUnit()
	if !info.Supported || !info.Exists {
		// checkDaemonHealth already warns when no unit is installed.
		return
	}
	if info.Err != nil {
		report.Warn(sectionDaemon, "autostart path",
			fmt.Sprintf("cannot read the autostart unit: %v", info.Err),
			"reinstall autostart: af daemon install", false)
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
	report.Fail(sectionDaemon, "autostart path",
		fmt.Sprintf("your autostart daemon runs %s but your af is %s; upgrades won't reach the supervised daemon",
			unitPath, selfPath),
		"reinstall autostart: af daemon install")
}

// checkSplitBrainBinaries finds af binaries at different versions in the usual
// install locations. This is the upstream cause of the whole family: with an
// old af on one PATH entry and a new one on another, whichever launches the
// daemon strands the other's clients — and the version skew reappears every
// time the wrong one wins.
func checkSplitBrainBinaries(ctx *scanContext, report *Report) {
	type found struct {
		path    string
		version string
	}
	var installs []found
	versions := map[string]bool{}

	for _, path := range ctx.opts.binaryCandidates() {
		v, err := ctx.opts.binaryVersion(path)
		if err != nil || v == "" {
			// Unreadable or not an af binary: silence is right here. This check
			// exists to compare the installs it can identify, not to audit
			// every path a binary might sit on.
			continue
		}
		installs = append(installs, found{path: path, version: v})
		versions[v] = true
	}

	if len(installs) <= 1 || len(versions) <= 1 {
		// One install, or several that agree, cannot strand a daemon.
		return
	}
	descs := make([]string, 0, len(installs))
	for _, in := range installs {
		descs = append(descs, fmt.Sprintf("%s@%s", in.path, in.version))
	}
	report.Fail(sectionDaemon, "af binaries",
		fmt.Sprintf("%d af installs at different versions (%s); whichever starts the daemon strands the other's clients",
			len(installs), strings.Join(descs, ", ")),
		"remove the stale install, or make sure PATH prefers the one you upgrade")
}

// checkStaleSockets reports daemon sockets left in the home with no daemon
// answering. The socket is what makes the failure quiet: clients find it,
// connect, and wait, instead of starting a daemon that would work.
func checkStaleSockets(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if h.PingErr == nil {
		return // a daemon is answering; the sockets are live by definition
	}
	var stale []string
	for _, name := range daemon.DaemonSocketNames() {
		path := filepath.Join(ctx.opts.ConfigDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		// The name is ours by convention, but only the mode proves the entry
		// is actually a socket rather than a file that borrowed the name.
		if info.Mode()&os.ModeSocket == 0 {
			continue
		}
		stale = append(stale, name)
	}
	if len(stale) == 0 {
		return
	}
	report.Warn(sectionDaemon, "stale sockets",
		fmt.Sprintf("%s present in %s but no daemon is answering",
			plural(len(stale), "daemon socket", "daemon sockets")+" ("+strings.Join(stale, ", ")+")",
			ctx.opts.ConfigDir),
		"run `af reset` to clear them, or `af daemon restart` to start a daemon on them", true)
}

// checkAutostartSupervision reports an autostart unit that exists but is not
// actually supervising the daemon. On macOS that includes an agent loaded in a
// domain other than the gui/<uid> the restart path targets: restarts silently
// miss it, so an old daemon keeps serving and skew never clears.
func checkAutostartSupervision(ctx *scanContext, report *Report) {
	info := ctx.opts.autostartSupervision()
	if !info.Supported || !info.UnitPresent {
		return
	}
	switch {
	case info.LoadedElsewhere:
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("the launchd agent is loaded outside %s, where af's restarts are sent (%s)",
				info.Domain, info.Detail),
			"reload it in the right domain: af daemon install", true)
	case !info.Active:
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("a unit file is installed but the service manager is not running it (%s)", info.Detail),
			"reinstall autostart: af daemon install", true)
	case !info.Enabled:
		report.Warn(sectionDaemon, "autostart supervision",
			fmt.Sprintf("the unit is running but not enabled, so it won't start at login (%s)", info.Detail),
			"enable it: af daemon install", true)
	default:
		report.Pass(sectionDaemon, "autostart supervision", "unit is enabled and running")
	}
}

// resolvePath canonicalizes p for comparison, resolving symlinks so that a
// symlinked install (~/.local/bin/af -> /nix/store/…) is not mistaken for a
// different binary. Falls back to a lexical absolute path when the target
// cannot be resolved — a path that no longer exists still compares usefully.
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// resolvedSelfBinary returns the path of the binary running this command.
func resolvedSelfBinary() (string, error) {
	return os.Executable()
}

// defaultBinaryCandidates lists the paths an af install plausibly occupies:
// whatever PATH resolves, this binary itself, and the fixed locations the
// installer and the common package managers use. Deduped by resolved path so a
// symlink chain to one binary is one install, not several.
func defaultBinaryCandidates() []string {
	var candidates []string
	if p, err := exec.LookPath("af"); err == nil {
		candidates = append(candidates, p)
	}
	if p, err := os.Executable(); err == nil {
		candidates = append(candidates, p)
	}
	fixed := []string{
		"/usr/local/bin/af",
		"/usr/bin/af",
		"/opt/homebrew/bin/af",
		"/home/linuxbrew/.linuxbrew/bin/af",
	}
	if home, err := os.UserHomeDir(); err == nil {
		fixed = append(fixed, filepath.Join(home, ".local", "bin", "af"))
	}
	candidates = append(candidates, fixed...)

	seen := map[string]bool{}
	var out []string
	for _, p := range candidates {
		resolved := resolvePath(p)
		if resolved == "" || seen[resolved] {
			continue
		}
		if info, err := os.Stat(resolved); err != nil || info.IsDir() {
			continue
		}
		seen[resolved] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// execBinaryVersion asks the af binary at path for its version. Bounded by
// binaryProbeTimeout, and `version` is a pure print — it neither reads the
// home nor contacts, let alone starts, a daemon.
func execBinaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), binaryProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", err
	}
	return parseAFVersion(string(out)), nil
}

// parseAFVersion pulls the version out of `af version` output
// ("agent-factory version 1.0.192").
func parseAFVersion(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if last == "version" {
		return "" // "agent-factory version" with nothing after it
	}
	return last
}
