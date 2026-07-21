package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
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
	case served == "":
		// Judged BEFORE the dev-client case on purpose. An empty version is not
		// an unjudgeable mismatch: it means the daemon predates the Ping version
		// field entirely, so it is older than ANY client that can ask — a
		// source-built client included, since it carries the field it is asking
		// with. Letting the dev catch-all swallow this would hide real skew from
		// exactly the people running a dev build.
		report.Fail(sectionDaemon, "daemon version",
			fmt.Sprintf("the running daemon predates version reporting but your af client is %s; "+
				"a version-skewed daemon rejects newer requests (e.g. \"unknown field\") and can hang the UI",
				describeClient(client)),
			"restart it: af daemon restart")
	case client == "" || client == devVersion:
		report.Warn(sectionDaemon, "daemon version",
			fmt.Sprintf("daemon reports %s; this client is an unreleased build, so skew cannot be judged",
				describeVersion(served)),
			"compare against a released af build if the UI misbehaves", false)
	case served == devVersion:
		// A dev daemon's version identifies no release, so it is neither equal
		// nor unequal to ours in any way that predicts compatibility. Failing
		// here would tell everyone running a daemon they built from source to
		// restart it, forever — the exact cry-wolf that teaches users to ignore
		// doctor.
		report.Warn(sectionDaemon, "daemon version",
			fmt.Sprintf("the daemon is an unreleased build and your af client is %s; "+
				"an unreleased version identifies no release, so skew cannot be judged", client),
			"if the UI misbehaves, restart the daemon onto a released build: af daemon restart", false)
	case served != client:
		report.Fail(sectionDaemon, "daemon version",
			fmt.Sprintf("daemon is running %s but your af client is %s; "+
				"a version-skewed daemon rejects newer requests (e.g. \"unknown field\") and can hang the UI", served, client),
			"restart it: af daemon restart")
	default:
		report.Pass(sectionDaemon, "daemon version", "matches this client ("+client+")")
	}
}

// describeClient names the client in a verdict, including the unreleased case:
// "your af client is dev" is still the fact the user needs.
func describeClient(v string) string {
	if v == "" {
		return "an unversioned build"
	}
	return v
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
// socket while a new one was installed.
//
// Scoped to this user AND this home. Neither half is free: /proc shows every
// user's processes, and an `af --daemon` run by someone else on a shared box
// has a world-readable cmdline that matches ours perfectly. Only its owning uid
// and its (unreadable) environ tell it apart from a genuine duplicate — see
// activeHomeDaemons. Daemons serving other homes are checkForeignDaemons'
// business.
func checkDuplicateDaemons(ctx *scanContext, report *Report) {
	if ctx.snap == nil {
		// No process snapshot — proctree is /proc-only, so this is every run on
		// macOS (#1939). Staying silent would render as a clean bill of health
		// for a check that never executed: a diagnostic that cannot see must
		// say so, not PASS. Advisory, because the user cannot fix the platform.
		report.Warn(sectionDaemon, "daemon instances",
			"cannot scan processes on this platform, so duplicate daemons were not checked",
			"on macOS this check is unavailable (#1939); check for extra `af --daemon` processes by hand", false)
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
	// The remedy must cost no more than the problem. A second daemon is a
	// daemon-lifecycle problem, and stopping one loses nothing: sessions live
	// in tmux and on disk, and a daemon is restartable at will. Anything that
	// touches the user's sessions, tasks or worktrees is out of all proportion
	// to what this detects.
	report.Fail(sectionDaemon, "daemon instances",
		fmt.Sprintf("%d daemons are serving this agent-factory home (%s); "+
			"a second or stale daemon is a common cause of version skew",
			len(procs), strings.Join(descs, ", ")),
		"stop the extra daemon by pid (listed above), then run `af daemon restart`")
}

// daemonProc pairs a daemon process with the agent-factory home it serves.
type daemonProc struct {
	proc proctree.Process
	// home is the agent-factory home this daemon serves; only meaningful when
	// homeKnown, and never guessed.
	home string
	// homeKnown reports whether home was positively established. False means
	// the process's environ could not be read, so which home it serves is
	// genuinely unknown — not "the default".
	homeKnown bool
	// ownedByUs reports whether this process runs under the uid running doctor.
	ownedByUs bool
	// isSelfAncestor reports whether this process is us or one of our
	// ancestors. Counting it is fine — killing it is not.
	isSelfAncestor bool
}

// scanDaemons returns every agent-factory daemon in the snapshot paired with
// the home it serves, in PID order.
//
// One scan, shared by both daemon checks: classifying a process means reading
// its argv and environ, which is a /proc read per process, and "is this a
// daemon, and whose?" must have exactly one answer — checkDuplicateDaemons
// counting a process that checkForeignDaemons would call foreign is a
// contradiction, not two opinions.
//
// Attribution is deliberately conservative, because every consumer of this scan
// either accuses the user of a problem or (with --fix) kills something. A
// process is attributed to a home only when we positively read that home out of
// its environ; anything unreadable stays unattributed. See daemonProcessHome.
//
// Our own ancestors are INCLUDED, and flagged rather than dropped. Dropping them
// is what the kill paths want, but counting is not killing: `af doctor` run from
// a daemon-spawned child (a watch task's shell) has the real serving daemon as
// an ancestor, so excluding it leaves a stale second daemon looking like the one
// legitimate daemon — doctor reporting PASS on a split-brained box, which is the
// same lie as a false alarm, told the other way round (#1044).
func scanDaemons(ctx *scanContext) []daemonProc {
	self := selfUID()
	var out []daemonProc
	for pid, p := range ctx.snap {
		args := daemonProcessArgv(pid)
		if len(args) == 0 || !daemon.LooksLikeDaemonArgv(args) {
			continue
		}
		home, known := daemonProcessHome(pid)
		uid, uidKnown := daemonProcessOwnerUID(pid)
		out = append(out, daemonProc{
			proc:           p,
			home:           home,
			homeKnown:      known,
			ownedByUs:      uidKnown && uid == self,
			isSelfAncestor: ctx.selfAncestors[pid],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].proc.PID < out[j].proc.PID })
	return out
}

// selfUID is the uid doctor runs as; a var so tests can pin it.
var selfUID = os.Getuid

// daemonProcs returns the run's daemon scan, computing it at most once. The
// doctor run is sequential, so a plain memo is enough.
func (c *scanContext) daemonProcs() []daemonProc {
	if !c.daemonsScanned {
		c.daemons = scanDaemons(c)
		c.daemonsScanned = true
	}
	return c.daemons
}

// activeHomeDaemons returns the daemon processes positively established as
// serving the active home, under our own uid. Both conditions are required:
// anything we cannot confirm is somebody else's business, and a diagnostic that
// accuses the user based on a guess is worse than one that stays quiet.
func activeHomeDaemons(ctx *scanContext) []proctree.Process {
	activeHome := normalizeHome(ctx.opts.ConfigDir)
	var out []proctree.Process
	for _, d := range ctx.daemonProcs() {
		if d.ownedByUs && d.homeKnown && d.home == activeHome {
			out = append(out, d.proc)
		}
	}
	return out
}

// daemonProcessHome resolves which agent-factory home pid serves, reporting
// whether the answer is known at all.
//
// Everything here is read in the DAEMON's frame of reference, never ours. The
// daemon resolved its config dir from its own environment and its own working
// directory, so that is the only frame in which its AGENT_FACTORY_HOME means
// what it meant:
//
//   - no AGENT_FACTORY_HOME → its home is <its HOME>/.agent-factory. Using our
//     $HOME instead attributes a daemon started under a different HOME (a
//     sandbox or debug run) to the home we happen to be inspecting.
//   - a tilde ("~/af-home") expands against ITS $HOME.
//   - a relative path ("af-home", which config.GetConfigDir accepts as-is) is
//     relative to ITS cwd. Absolutizing against ours makes the same daemon
//     resolve differently depending on where doctor was invoked from.
//
// Every home comparison in this file is built on this answer, so getting the
// frame wrong inverts the verdicts (a real duplicate missed, a sibling called
// foreign) rather than merely making them noisy. When the daemon's frame cannot
// be read, the home is unknown — never guessed from ours (#1044).
func daemonProcessHome(pid int) (string, bool) {
	raw, found, err := daemonProcessEnvLookup(pid, "AGENT_FACTORY_HOME")
	if err != nil {
		return "", false // unreadable: unknown, not "the default"
	}
	// The daemon's own HOME, needed for both the default and any tilde.
	daemonHome, _, err := daemonProcessEnvLookup(pid, "HOME")
	if err != nil {
		return "", false
	}
	if !found || raw == "" {
		if daemonHome == "" {
			return "", false // no explicit home and no HOME to derive one from
		}
		raw = filepath.Join(daemonHome, ".agent-factory")
	}
	cwd, _ := daemonProcessCwd(pid)
	return resolveHomeIn(raw, daemonHome, cwd)
}

// resolveHomeIn canonicalizes a home spelled in the frame of a process whose
// HOME is homeDir and whose working directory is cwd, so that two spellings of
// one directory compare equal. Reports false when the spelling cannot be
// resolved in that frame.
//
// The spellings that actually occur: a raw tilde ("~/af-home", which a daemon's
// environ preserves while config.GetConfigDir has already expanded the active
// home); a relative path from a hand-written unit; a trailing slash; and a
// symlinked home — including macOS /var vs /private/var, where one directory has
// two absolute spellings (#1918).
func resolveHomeIn(raw, homeDir, cwd string) (string, bool) {
	if raw == "" {
		return "", false
	}
	switch {
	case raw == "~" || strings.HasPrefix(raw, "~/"):
		if homeDir == "" {
			return "", false // a tilde we cannot expand in that frame
		}
		raw = filepath.Join(homeDir, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	case strings.HasPrefix(raw, "~"):
		// "~user" — GetConfigDir rejects these outright rather than treating
		// them as a literal directory, so there is no home to compare.
		return "", false
	}
	if !filepath.IsAbs(raw) {
		if cwd == "" {
			return "", false // relative to a working directory we cannot read
		}
		raw = filepath.Join(cwd, raw)
	}
	if resolved, err := filepath.EvalSymlinks(raw); err == nil {
		raw = resolved
	}
	return filepath.Clean(raw), true
}

// normalizeHome canonicalizes OUR active home for comparison. Our own frame is
// the right one here and only here: ConfigDir came from config.GetConfigDir,
// which resolved it from this process's environment and cwd.
func normalizeHome(home string) string {
	resolved, _ := resolveHomeIn(config.ExpandTilde(home), userHomeDir(), workingDir())
	return resolved
}

// userHomeDir and workingDir are this process's frame, kept as vars so tests can
// pin them.
var (
	userHomeDir = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return home
	}
	workingDir = func() string {
		dir, err := os.Getwd()
		if err != nil {
			return ""
		}
		return dir
	}
)

// daemonProcSummary identifies a daemon process by pid and age, which is what
// distinguishes the stale daemon from the fresh one at a glance.
func daemonProcSummary(p proctree.Process) string {
	if _, age, err := proctree.CPUFraction(p); err == nil {
		return fmt.Sprintf("pid %d started %s ago", p.PID, formatAge(age))
	}
	return fmt.Sprintf("pid %d", p.PID)
}

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

// checkStaleSockets reports daemon sockets left in the home that nothing is
// listening on. The socket is what makes the failure quiet: clients find it,
// connect, and wait, instead of starting a daemon that would work.
//
// Two things it deliberately does NOT do.
//
// It does not report the CONTROL socket: checkDaemonHealth pings it, and a
// present-but-silent control socket is already its FAIL with the same remedy.
// Billing one condition to two actionable rows inflates the issue count and
// hands scripts two rows for one fix.
//
// And it does not call a socket stale on the strength of the control socket's
// silence. "The control socket did not answer" is not evidence about a
// DIFFERENT listener — they are separate binds, and RunDaemon keeps one when the
// other fails. Telling a user to restart over a live listener is the same
// mistake as reading a probe failure as a negative answer: a claim we have not
// earned. A socket is stale here only when its own dial failed (#1044).
func checkStaleSockets(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if h.PingErr == nil {
		return // a daemon is answering; checkHTTPSocket owns the HTTP probe
	}
	var stale []string
	for _, name := range daemon.DaemonSocketNames() {
		if name == daemon.ControlSocketName() {
			continue // checkDaemonHealth owns this one
		}
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
		if !socketProvenDead(name, h) {
			// Either something is accepting on it, or nobody dialed it. Neither
			// is grounds for calling it debris.
			continue
		}
		stale = append(stale, name)
	}
	if len(stale) == 0 {
		return
	}
	report.Warn(sectionDaemon, "stale sockets",
		fmt.Sprintf("%s present in %s but nothing is listening on %s",
			plural(len(stale), "daemon socket", "daemon sockets")+" ("+strings.Join(stale, ", ")+")",
			ctx.opts.ConfigDir, plural(len(stale), "it", "them")),
		// A restart rebinds these; nothing here justifies touching the user's
		// sessions or state.
		"run `af daemon restart` to rebind them", true)
}

// socketProvenDead reports whether we have POSITIVE evidence that nothing is
// accepting on the named socket — a dial of that socket that failed.
//
// Absence of evidence is not evidence here: a socket nobody probed stays
// unreported rather than being assumed dead. Today Health dials exactly one
// non-control socket (the HTTP one); a future socket added to DaemonSocketNames
// without a matching probe will simply not be claimed, which is the safe way to
// be incomplete.
func socketProvenDead(name string, h daemon.HealthStatus) bool {
	if filepath.Base(h.HTTPSocketPath) != name {
		return false
	}
	// Only a dial that ANSWERED "nothing is there" counts. Undetermined — the
	// probe never ran — is not evidence of death, which is the whole point of
	// the answer type.
	dead := false
	h.HTTPListening.Match(
		func() {}, func() { dead = h.HTTPSocketExists }, func() { dead = false }, func(error) {},
	)
	return dead
}

// checkHTTPSocket reports the HTTP/JSON listener's health independently of the
// control socket.
//
// They are two listeners, and RunDaemon keeps serving the control socket when
// startHTTPServer fails. So "the daemon answers" — the thing checkDaemonHealth
// establishes — is no evidence at all about the socket the TUI, the HTTP API,
// and the web UI actually dial. Gating this probe on a healthy Ping let doctor
// report all-clear over a dead web surface (#1044).
func checkHTTPSocket(ctx *scanContext, report *Report, h daemon.HealthStatus) {
	if h.PingErr != nil {
		// No daemon: its HTTP socket is expected to be gone too, and any
		// leftover file is checkStaleSockets' story.
		return
	}
	// Match has no default, so "healthy" cannot be reported without an answer
	// that says so. A nil error used to mean both "the dial succeeded" and
	// "nobody dialed", and PASS fell out of the second one.
	h.HTTPListening.Match(
		func() {
			report.Pass(sectionDaemon, "http socket", "accepting connections on "+h.HTTPSocketPath)
		},
		func() {
			if !h.HTTPSocketExists {
				report.Warn(sectionDaemon, "http socket",
					fmt.Sprintf("the daemon is running but its HTTP socket (%s) is not there, "+
						"so the web UI and HTTP clients have nothing to dial", h.HTTPSocketPath),
					"run `af daemon restart` and check the daemon log for an HTTP listener error", true)
				return
			}
			report.Warn(sectionDaemon, "http socket",
				fmt.Sprintf("the daemon is running but nothing answers on its HTTP socket %s; "+
					"the web UI and HTTP clients will hang or fail", h.HTTPSocketPath),
				"run `af daemon restart` to rebind it", true)
		},
		func() {
			report.Warn(sectionDaemon, "http socket",
				fmt.Sprintf("the daemon is running but its HTTP socket (%s) does not exist", h.HTTPSocketPath),
				"run `af daemon restart` and check the daemon log for an HTTP listener error", true)
		},
		func(cause error) {
			report.Warn(sectionDaemon, "http socket",
				fmt.Sprintf("could not probe the daemon's HTTP socket, so the web/API surface is unknown: %s", oneLine(cause)),
				"check the daemon log, then rerun `af doctor`", false)
		},
	)
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
					"run `af daemon install` to hand the daemon back to the installed unit", true)
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
			"run `af daemon install` to hand the daemon back to the installed unit", true)
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

// oneLine flattens a multi-line error (errors.Join separates with newlines) so a
// row stays one line — the report's columns are the whole point of its
// scannability.
func oneLine(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(strings.Fields(err.Error()), " ")
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

// defaultBinaryCandidates lists the paths an af INSTALL plausibly occupies:
// whatever PATH resolves, plus the fixed locations the installer and the common
// package managers use. Deduped by resolved path so a symlink chain to one
// binary is one install, not several.
//
// os.Executable is deliberately absent. A binary someone just built and ran out
// of a temp dir or a worktree is not an install, and including it made a routine
// dev-box workflow — run a scratch build whose version differs from the one on
// disk — report a split-brain install and advise removing it. The real
// split brain this check exists for (an old /usr/local/bin/af beside a new
// ~/.local/bin/af) lives entirely in the paths below.
func defaultBinaryCandidates() []string {
	var candidates []string
	if p, err := exec.LookPath("af"); err == nil {
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

// binaryProbeWaitDelay bounds how long Wait blocks after a version probe is
// killed on its deadline, before the inherited pipes are force-closed. Same
// reason as autostartProbeWaitDelay: without it the deadline is a comment, not
// a bound (#1967).
const binaryProbeWaitDelay = 2 * time.Second

// execBinaryVersion asks the af binary at path for its version. `version` is a
// pure print — it neither reads the home nor contacts, let alone starts, a
// daemon.
//
// Really bounded, not decoratively. This runs a binary doctor found lying on a
// PATH entry: it may be a wrapper script, a stale build, or something that is
// not af at all, and if it forks a child that inherits stdout and wedges, then
// exec.CommandContext kills only the direct process and Output() waits on pipe
// EOF regardless of the deadline — hanging the one command a user runs BECAUSE
// things are wedged. The autostart probe got this treatment; this one asserted
// it in a comment and did not have it (#1967).
func execBinaryVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), binaryProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "version")
	// Its own process group, so the deadline tears down the whole tree rather
	// than orphaning a child that still holds the pipe (#610/#769/#856).
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
	cmd.WaitDelay = binaryProbeWaitDelay

	out, err := cmd.Output()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // reap stragglers
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s version timed out after %s: %w", path, binaryProbeTimeout, ctx.Err())
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		// ErrWaitDelay alone means the binary answered and only a straggler held
		// the pipe — the answer is in `out`, and the reap above killed the
		// straggler. A real failure surfaces as an ExitError instead.
		return "", err
	}
	return parseAFVersion(string(out)), nil
}

// afVersionPattern is the exact shape `af version` prints. Requiring the full
// "agent-factory version <semver>" line — rather than "the last word of line 1"
// — is what keeps an unrelated program that happens to be named `af` from being
// read as an agent-factory install. Plenty of binaries answer `version` with a
// zero exit and print something; only ours prints this (#1044).
//
// The suffix is permissive after the semver (a `-rc1`/`+dirty` build tag is
// still our version), but the prefix and the numeric core are not. "dev" is
// accepted alongside semver because an unreleased build IS one of ours, and a
// dev binary sitting next to a release is precisely the split brain a developer
// needs told about — rejecting it would make the check blind to its most likely
// real case.
var afVersionPattern = regexp.MustCompile(`^agent-factory version (\d+\.\d+\.\d+\S*|dev)$`)

// parseAFVersion pulls the version out of `af version` output
// ("agent-factory version 1.0.192"), returning "" for anything that is not an
// agent-factory binary announcing itself in the expected form.
func parseAFVersion(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	m := afVersionPattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return ""
	}
	return m[1]
}
