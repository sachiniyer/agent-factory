package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// sigtermFallback locates a running pre-#501 daemon and sends it SIGTERM,
// escalating to SIGKILL after sigtermFallbackGrace. Used when the Shutdown
// RPC returned method-not-found (the daemon is listening on the control
// socket but the binary predates #501).
//
// Strategy:
//  1. If ~/.agent-factory/daemon.pid exists, parse it. Verify the PID is
//     alive, its command line contains "--daemon" as a discrete token,
//     AND it belongs to THIS caller's AGENT_FACTORY_HOME. The cmdline check
//     alone cannot tell an `af --daemon` of this home from one of another
//     home, so a stale PID file whose PID was recycled by another home's
//     `af --daemon` would pass it while serving someone else's control
//     socket; the home binding (pidBelongsToThisHome) is what keeps the
//     fallback from SIGTERM-ing the wrong daemon.
//  2. Otherwise (or if the PID file is missing/stale/foreign), scan with
//     `pgrep -f -- '--daemon'`, keep only processes whose binary is `af`
//     or `agent-factory` (#937 — source builds run under the latter name),
//     filter out /tmp/Test* paths (Go test binaries) and the current
//     process, and require exactly one candidate.
//
// Returns ShutdownViaSIGTERM and the signalled target when a signal was
// delivered (the process is already dead — signalAndWait waited), or ShutdownFailed
// with an actionable error when the daemon (which is provably running — the
// caller only invokes us after the Shutdown RPC returned method-not-found,
// not ECONNREFUSED) could not be located or signaled. Returning
// ShutdownNoDaemon here would contradict the established state and silently
// leave the stale daemon running (#553).
func sigtermFallback() (ShutdownResult, ShutdownTarget, error) {
	pid, proc, source, scanned, err := locateDaemonPID()
	if err != nil {
		if scanned > 0 {
			// An ambiguity (multiple same-home `--daemon` candidates) or
			// another scan failure that left foreign/unverifiable daemons in
			// the scan (counted in `scanned`). The error already names the
			// same-home PIDs to kill by hand; do NOT append the blanket
			// `pkill -f -- '--daemon'` here — that command has no home or PID
			// constraint, so following it would kill exactly the foreign-home
			// daemons the home filter refused to touch, plus any unrelated
			// process carrying "--daemon".
			return ShutdownFailed, ShutdownTarget{}, err
		}
		return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
			"sigterm fallback failed: %w; run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			err,
		)
	}
	if pid == 0 {
		if scanned > 0 {
			// Every `--daemon` candidate the host scan found serves ANOTHER
			// home or could not be bound, and this filter deliberately refused
			// to signal any of them. Do NOT recommend a blanket
			// `pkill -f -- '--daemon'` here: that command has no home or PID
			// constraint, so following it would kill exactly the foreign-home
			// daemons this filtering refused to touch, plus any unrelated
			// process carrying "--daemon". The daemon serving THIS home must
			// be stopped by its PID, not a host-wide pattern.
			return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
				"sigterm fallback: daemon is running on the control socket but no PID candidate was found for this home (%s); "+
					"%d `--daemon` process(es) were left untouched because they serve another home or could not be bound — "+
					"stop the daemon serving this home by its PID, then retry `af upgrade`",
				source, scanned,
			)
		}
		return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
			"sigterm fallback: daemon is running on the control socket but no PID candidate was found (%s); "+
				"run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			source,
		)
	}

	// Pin the incarnation before signalling, while pid is certainly the daemon:
	// sampled after it dies, a recycled PID would yield the replacement's token.
	target := ShutdownTarget{PID: pid, StartToken: processStartTokenFn(pid)}
	log.InfoLog.Printf("sigterm fallback: signaling pre-#501 daemon (pid=%d source=%s)", pid, source)
	if err := signalClassifiedDaemon(pid, proc); err != nil {
		if errors.Is(err, errSignalTargetChanged) {
			// The PID locateDaemonPID proved serves this home exited and its
			// number was recycled onto a live process we did not classify
			// (likely another home's daemon) before the signal landed; the
			// identity-checked signal refused to signal the replacement, so a
			// recycled PID is not killed. Do NOT recommend the blanket
			// `pkill -f -- '--daemon'` — that command has no home or PID
			// constraint, so following it would kill exactly the recycled PID
			// the identity check just refused to touch, plus any unrelated
			// process carrying "--daemon". The caller must rediscover the
			// daemon serving this home by its PID (#4793 review).
			return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
				"sigterm fallback: the daemon pid %d proven to serve this home exited before SIGTERM and its PID was recycled onto another process; not signalling the replacement — "+
					"stop the daemon serving this home by its PID, then retry `af upgrade`",
				pid,
			)
		}
		if scanned > 1 {
			// locateDaemonPID returned this proven-ours PID alongside one or
			// more foreign/unverifiable `--daemon` candidates (counted in
			// `scanned`). The blanket `pkill -f -- '--daemon'` the host-wide
			// hint below recommends matches on the full command line with no
			// home or PID constraint, so following it would signal exactly
			// those foreign daemons the home filter refused to touch, plus any
			// unrelated process carrying "--daemon". Carry the scoped recovery
			// the other branches already use: stop the daemon serving THIS
			// home by its PID, not a host-wide pattern.
			return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
				"sigterm fallback for daemon pid %d: %w; %d other `--daemon` process(es) were left untouched because they serve another home or could not be bound — stop the daemon serving this home by its PID, then retry `af upgrade`",
				pid, err, scanned-1,
			)
		}
		return ShutdownFailed, ShutdownTarget{}, fmt.Errorf(
			"sigterm fallback for daemon pid %d: %w; run \"pkill -f -- '--daemon'\" to stop the old daemon manually before retrying `af upgrade`",
			pid, err,
		)
	}

	// Best-effort PID file cleanup so the next `af` invocation does not see
	// a stale file. StopDaemon does this on its happy path too; doing it
	// here keeps state tidy when the daemon binary never wrote one itself.
	removeDaemonPIDFile()
	return ShutdownViaSIGTERM, target, nil
}

// locateDaemonPID returns the PID of the running daemon to signal, a
// proctree.Process snapshot captured AT the classification decision (the
// instance whose home classifyDaemonHome proved ours, not a later Lookup that
// could observe a recycled PID), the source it was found in ("pid-file" or
// "pgrep"), and the count of `--daemon` candidates the host scan surfaced (0
// when no scan ran, e.g. no PID file and pgrep unavailable). On failure to
// locate a PID, returns (0, zero Process, source, scanned, nil) where source
// describes the suspected PID source for diagnostics (e.g. "pid-file pid=N
// foreign, pgrep: no matches for this home" or "no pid-file, pgrep
// unavailable") and scanned is the number of foreign/unverifiable candidates
// that were found and deliberately left untouched — including a PID-file entry
// the home binding rejected as a live foreign or unverifiable daemon, counted
// even when the scan finds nothing or does not run, so sigtermFallback does
// not recommend a blanket pkill that would kill it. An error is returned only
// for hard failures (ambiguous pgrep results, pgrep itself failing to
// execute). The returned Process binds the signal to the instance the home
// binding proved ours: a PID the kernel recycled between this classification
// and the signal has a different StartID, so proctree.Signal refuses it
// (ErrIdentityChanged) rather than terminating the replacement (#4793).
func locateDaemonPID() (int, proctree.Process, string, int, error) {
	pidFileSource := "no pid-file"
	// rejectedPIDFilePID is the PID a daemon.pid entry named when it was a
	// LIVE `af --daemon` the home binding PROVED serves another home
	// (daemonForeign) or could not bind (daemonUnverifiable). It is a
	// `--daemon` process this filter deliberately refused to signal, and a
	// blanket `pkill -f -- '--daemon'` (no home or PID constraint) would kill
	// it; counting it toward `scanned` even when the subsequent pgrep scan
	// finds nothing — or does not run, because pgrep is unavailable or errors
	// — keeps sigtermFallback from recommending a blanket pkill against
	// the very process the PID-file binding just refused to touch. A dead
	// or non-daemon PID-file entry is plain stale (no live foreign daemon
	// for a blanket pkill to hit), so it counts as 0.
	rejectedPIDFilePID := 0
	if pid, ok := readPIDFromFile(); ok {
		switch {
		case !pidLooksAlive(pid) || !isAgentFactoryDaemon(pid):
			log.InfoLog.Printf("sigterm fallback: PID file pid=%d is dead or not a daemon; falling back to pgrep", pid)
			pidFileSource = fmt.Sprintf("pid-file pid=%d stale", pid)
		default:
			switch classifyDaemonHome(pid) {
			case daemonOurs:
				return pid, captureDaemonIdentity(pid), "pid-file", 0, nil
			case daemonForeign:
				log.InfoLog.Printf("sigterm fallback: PID file pid=%d is a live daemon serving ANOTHER home; not signalling; falling back to pgrep", pid)
				pidFileSource = fmt.Sprintf("pid-file pid=%d foreign", pid)
				rejectedPIDFilePID = pid
			default: // daemonUnverifiable
				log.InfoLog.Printf("sigterm fallback: PID file pid=%d is a live daemon whose home could not be bound; not signalling; falling back to pgrep", pid)
				pidFileSource = fmt.Sprintf("pid-file pid=%d unverifiable", pid)
				rejectedPIDFilePID = pid
			}
		}
	}

	pids, err := scanDaemonCandidatesFn()
	if err != nil {
		// Preserve a rejected PID-file candidate as an untouched count even
		// when no scan ran, so sigtermFallback does not fall back to a
		// blanket pkill that would kill it.
		scanned := 0
		if rejectedPIDFilePID != 0 {
			scanned = 1
		}
		if errors.Is(err, errPgrepUnavailable) {
			return 0, proctree.Process{}, fmt.Sprintf("%s, pgrep unavailable", pidFileSource), scanned, nil
		}
		return 0, proctree.Process{}, "", scanned, fmt.Errorf("%s, pgrep: %w", pidFileSource, err)
	}
	// Reclassify every scanned candidate by uid and AGENT_FACTORY_HOME before
	// selecting a signal target. The pgrep scan returns every `--daemon`
	// process on the host, so a single result that serves ANOTHER home (or
	// whose home is unverifiable) would otherwise be signalled — exactly the
	// unrelated process the PID-file binding just rejected, whenever it was
	// the only scan match. Filtering to this home's proven daemons also turns
	// the old false "ambiguous" refusal into the correct single target when
	// other homes' daemons are alive alongside ours (#4793). The PID-file fast
	// path above and this scan path now both require the home binding, so the
	// two agree on what is signalled (#1004).
	scoped := make([]int, 0, len(pids))
	for _, pid := range pids {
		if pidBelongsToThisHome(pid) {
			scoped = append(scoped, pid)
		}
	}
	switch len(scoped) {
	case 0:
		// Every scanned `--daemon` serves another home or is unverifiable, and
		// was deliberately NOT signalled. A rejected PID-file candidate is one
		// more such process even when pgrep found nothing, so it is folded into
		// `scanned` (without double-counting when pgrep also surfaced it) and
		// surfaced to the caller, which can then stop short of recommending a
		// blanket pkill that would kill exactly these foreign daemons.
		scanned := len(pids)
		if rejectedPIDFilePID != 0 && !slices.Contains(pids, rejectedPIDFilePID) {
			scanned++
		}
		return 0, proctree.Process{}, fmt.Sprintf("%s, pgrep: no matches for this home (%d scanned)", pidFileSource, scanned), scanned, nil
	case 1:
		// A rejected PID-file candidate the host scan did not surface is one
		// more `--daemon` process this filter deliberately did not signal, the
		// same way it is in the zero-match branch above: pgrepDaemonCandidates
		// filters a /tmp/go-build* binary the PID-file classifier intentionally
		// accepts, so a foreign/unverifiable daemon the PID file named can be
		// absent from `pids` even when the scan found this home's own daemon. If
		// signaling the one proven-ours PID later fails, sigtermFallback recommends
		// the blanket `pkill -f -- '--daemon'` whenever `scanned <= 1`, which would
		// kill exactly that rejected foreign daemon the PID-file binding refused to
		// touch. Fold `rejectedPIDFilePID` into the count here when it was absent
		// from `pids`, so the signaling-error branch sees `scanned > 1` and carries
		// the scoped recovery the zero-match branch already does.
		scanned := len(pids)
		if rejectedPIDFilePID != 0 && !slices.Contains(pids, rejectedPIDFilePID) {
			scanned++
		}
		return scoped[0], captureDaemonIdentity(scoped[0]), "pgrep", scanned, nil
	default:
		return 0, proctree.Process{}, "", len(pids), fmt.Errorf(
			"sigterm fallback: ambiguous, found %d `--daemon` processes for this home (%s) — "+
				"kill the right one manually then re-run `af upgrade`",
			len(scoped), formatPIDList(scoped),
		)
	}
}

// reclaimDeadUnverifiablePIDFile handles the one TOCTOU the inconclusive
// binding leaves in stopDaemonUntil. classifyDaemonHome returns daemonUnverifiable
// when it cannot establish the candidate's uid or AGENT_FACTORY_HOME — a state
// stopDaemonUntil treats as "neither signal nor orphan": do not SIGTERM a PID
// that may be another home's daemon (#4793), and do not delete the PID file over
// it. But "unverifiable" can also be a TRANSIENT race rather than a real
// verdict: the recorded daemon passed the signal-0 and argv checks, then exited
// WHILE classifyDaemonHome was reading its uid or environ, which surfaces as
// unverifiable. If the PID is now dead there is nothing left to protect, so the
// stale PID file is reclaimed the way the earlier dead-PID checks do — removed
// only if it still names this PID (removePIDFileIfStillNames), so a same-home
// daemon that started in the interim is not orphaned — and the caller reports
// nothing to stop, letting a stop/restart or handoff proceed cleanly instead
// of failing against a process that no longer exists.
//
// Returns true when the PID is dead and the file was reclaimed (caller returns
// stopped=false, nil); false when the PID is still alive and the inconclusive
// verdict stands (caller surfaces the binding error). pidLooksAlive is the same
// liveness test the rest of stopDaemonUntil polls with; the exit it detects is
// the narrow window between the signal-0 probe upstream and this recheck. Lives
// in sigterm_fallback.go to keep daemon.go under the file-length lint limit
// (#1145), the same reason removePIDFileIfStillNames lives here. deadline is the
// caller's admission deadline, propagated to the PID-file lock the reclaim
// takes so a deadline-bounded stop does not block on a contended lock.
func reclaimDeadUnverifiablePIDFile(pidFile string, pid int, deadline time.Time) bool {
	if !pidLooksAlive(pid) {
		log.InfoLog.Printf("PID %d could not be bound to this home (unresolved) but has since exited; removing stale PID file", pid)
		removePIDFileIfStillNames(pidFile, pid, deadline)
		return true
	}
	return false
}

// pidBelongsToThisHome reports whether the process at pid is an agent-factory
// daemon serving THIS process's AGENT_FACTORY_HOME. It is the home-binding half
// of the trust decision a PID-file candidate must pass before it is signalled:
// cmdline alone (isAgentFactoryDaemon) cannot distinguish an `af --daemon` of
// THIS home from one of another home, and a stale daemon.pid whose PID the
// kernel recycled onto a DIFFERENT home's `af --daemon` passes the cmdline check
// while serving someone else's control socket. Signaling it would kill an
// unrelated daemon — possibly another user's on a shared host — so every path
// that reads daemon.pid and signals the PID it names must require this in
// addition to the cmdline match.
//
// It is a thin bool over classifyDaemonHome, the classifier StopDaemon and the
// pgrep scan path share so the two PID-validation paths agree on what "ours"
// means (#1004). "Unverifiable" (a uid, environ, or path that could not be
// established) is NOT ours: a PID that cannot be bound to this home is never
// trusted — guessing "ours" is exactly the bug. Callers that must act
// differently on a PROVEN-foreign PID and an INCONCLUSIVE one use
// classifyDaemonHome directly; see stopDaemonUntil.
func pidBelongsToThisHome(pid int) bool {
	return classifyDaemonHome(pid) == daemonOurs
}

// classifyDaemonHome decides whether the process at pid is a daemon serving
// THIS process's AGENT_FACTORY_HOME, surfacing the inconclusive case
// (daemonUnverifiable) the bool helper collapses. StopDaemon reads daemon.pid
// and then decides whether to signal the PID it names: proving the PID is
// another home's daemon (daemonForeign) lets it treat the file as stale, but a
// PID whose home binding is merely inconclusive (daemonUnverifiable — a uid
// that could not be read, a foreign or withheld environ, an unresolvable
// AGENT_FACTORY_HOME) is neither safe to signal (it may be another home's
// daemon — the #4793 hazard) nor safe to delete the PID file over (that
// orphans the live daemon the file names, and every later recovery loses its
// handle to it). StopDaemon therefore needs the scope, not just the trust
// decision; locateDaemonPID only needs the trust decision, so it keeps the bool
// helper and the two paths still agree on what "ours" means (#1004).
//
// It is the classifier for a PID an explicit caller RECORDED — daemon.pid for
// StopDaemon, or a single pgrep result for the SIGTERM fallback — so it is tuned
// for that, not for the host-wide scan af reset's verifyScopedDaemon serves. It
// differs from verifyScopedDaemon in the three ways following a recorded PID
// demands:
//
//   - It does NOT apply isTestBinaryArgs. That heuristic keeps `go test`-spawned
//     fakes out of a HOST-WIDE scan, where a test binary is indistinguishable
//     from a real daemon by argv alone. A PID FILE was written by a real daemon,
//     so a binary under /tmp/go-build* or /tmp/Test* — an uncached
//     `go run . --daemon` or a source build placed in a temp dir — is a
//     legitimate target here, not a test fake. Applying the heuristic would
//     classify it foreign, delete its live PID file, and leave it running —
//     failing open on an inconclusive binding. The host-wide scan keeps the
//     heuristic (pgrepDaemonCandidates); this recorded-PID path does not.
//
//   - It resolves a RELATIVE AGENT_FACTORY_HOME against the DAEMON's working
//     directory, not ours. Two daemons launched from different directories with
//     the same relative value serve different homes; resolving both against the
//     caller's cwd would label a foreign one "ours" and let a stale PID file
//     signal it — the cross-home hazard #4793 is about. This mirrors
//     daemonProcessHome in doctor/skew.go: a home is spelled in the frame of the
//     process that set it. A relative home whose cwd cannot be read is
//     unresolvable (daemonUnverifiable), never resolved against ours.
//
//   - It resolves the DEFAULT home (no AGENT_FACTORY_HOME) and a TILDE home
//     ("~" or "~/...") from the DAEMON's own $HOME, not ours. config.ConfigDirFor
//     resolves both the empty ("") and tilde ("~/state") forms against
//     os.UserHomeDir — the CALLER's $HOME — so a same-UID daemon launched under
//     a different HOME, or two daemons sharing a "~/state" spelling under
//     different HOME values, would both resolve to the caller's home, compare
//     equal, and be signalled on a stale PID file: the #4793 hazard via the
//     empty-env and tilde forms. Both are derived from the daemon's HOME read
//     out of its environ; an unreadable or absent HOME is daemonUnverifiable,
//     never resolved against ours; a "~user" form config.ConfigDirFor rejects
//     stays unverifiable. This too mirrors daemonProcessHome in doctor/skew.go.
//
// af reset's orphan scan deliberately keeps verifyScopedDaemon; a recorded PID
// and a host-wide discovery carry different evidence and warrant different
// policy.
func classifyDaemonHome(pid int) daemonScope {
	wantConfigDir, err := config.GetConfigDir()
	if err != nil {
		return daemonUnverifiable
	}
	wantHome, err := canonicalDir(wantConfigDir)
	if err != nil {
		return daemonUnverifiable
	}
	// Re-read argv rather than trusting the recorded PID: a PID is a reusable
	// kernel handle, so the process it names may have exited and its number
	// been recycled onto an unrelated process since the file was written.
	if !isAgentFactoryDaemon(pid) {
		return daemonForeign
	}
	owner, ok := processUID(pid)
	if !ok {
		return daemonUnverifiable
	}
	if owner != os.Getuid() {
		return daemonForeign
	}
	env, readable := daemonHomeEnv(pid)
	if !readable {
		return daemonUnverifiable
	}
	// Resolve a DEFAULT home (no AGENT_FACTORY_HOME) and a TILDE home ("~" or
	// "~/...") against the DAEMON's own $HOME, not ours. config.ConfigDirFor
	// expands both the empty ("") and tilde ("~/state") forms through
	// os.UserHomeDir — the CALLER's $HOME — so a same-UID daemon launched under
	// a different HOME, or two daemons sharing a "~/state" spelling under
	// different HOME values, would both resolve to the caller's home, compare
	// equal to wantHome, and be marked ours — letting a stale PID file or lone
	// pgrep result signal a daemon serving another home (the #4793 hazard via
	// the empty-env and tilde forms). Mirror daemonProcessHome in
	// doctor/skew.go: derive both from the daemon's HOME read out of its
	// environ, and treat an unreadable or absent HOME as unverifiable rather
	// than guessing ours.
	//
	// derivedFromDaemonHome records that `env` was rebuilt here from the
	// DAEMON's own $HOME. When the daemon's HOME is itself "~user"-prefixed
	// (HOME=~alice with no AGENT_FACTORY_HOME leaves env="~alice/.agent-factory";
	// or AGENT_FACTORY_HOME=~/state under HOME=~alice leaves "~alice/state"),
	// the rebuilt path carries a leading "~user" that came from the daemon's
	// valid HOME — a value the daemon's own file ops treated as a cwd-relative
	// path, not a home expansion. That prefix is NOT the raw
	// AGENT_FACTORY_HOME="~user" config.ConfigDirFor rejects; it is left for
	// resolveHomeInDaemonFrame to resolve against the daemon's cwd the way the
	// daemon did, and only a "~user" that came from a RAW AGENT_FACTORY_HOME
	// stays unverifiable (the rejection below gates on !derivedFromDaemonHome).
	derivedFromDaemonHome := false
	if env == "" || env == "~" || strings.HasPrefix(env, "~/") {
		daemonHome, status := proctree.LookupEnv(pid, "HOME")
		if status != proctree.EnvFound || daemonHome == "" {
			return daemonUnverifiable
		}
		switch {
		case env == "":
			env = filepath.Join(daemonHome, ".agent-factory")
		default: // "~" or "~/..."
			env = filepath.Join(daemonHome, strings.TrimPrefix(strings.TrimPrefix(env, "~"), "/"))
		}
		derivedFromDaemonHome = true
	}
	// env is now expanded in the DAEMON's frame. Resolve it here rather than
	// routing it through config.ConfigDirFor: when the daemon's own HOME is
	// itself "~"-prefixed (HOME=~/x, with no AGENT_FACTORY_HOME, the block
	// above leaves env="~/x/.agent-factory"), config.ConfigDirFor would expand
	// that leading "~" against the CALLER's $HOME — making a daemon that
	// actually serves <its cwd>/~/x/.agent-factory compare equal to the
	// caller's home and be marked ours on a stale PID file (the #4793 hazard
	// via a "~"-prefixed daemon HOME). A RAW "~user" form config.ConfigDirFor
	// rejects (AGENT_FACTORY_HOME="~user", NOT derived from the daemon's HOME)
	// stays unverifiable; a "~user" prefix derived from the daemon's HOME
	// above is already expanded in the daemon's frame and falls through.
	// Every other spelling (absolute, "~"-prefixed, or relative) is resolved
	// relative to the daemon's own cwd by resolveHomeInDaemonFrame the way the
	// daemon's own file ops resolved it.
	if !derivedFromDaemonHome && strings.HasPrefix(env, "~") && env != "~" && !strings.HasPrefix(env, "~/") {
		return daemonUnverifiable
	}
	gotHome, ok := resolveHomeInDaemonFrame(pid, env)
	if !ok {
		return daemonUnverifiable
	}
	// A process-relative procfs path such as /proc/self/cwd/state (or
	// /proc/self/root/...) names a different directory in the DAEMON's frame
	// than in the caller's: the kernel resolves /proc/self against the reading
	// process, so canonicalDir resolves /proc/self/cwd as the CALLER's cwd, not
	// the daemon's. A same-UID daemon launched from a different directory with
	// AGENT_FACTORY_HOME=/proc/self/cwd/state serves <its cwd>/state, but
	// resolveHomeInDaemonFrame returns the absolute spelling unchanged and
	// canonicalDir then resolves it in the caller's frame — comparing equal to a
	// caller whose own home resolves to that same <caller-cwd>/state and marking
	// the foreign daemon daemonOurs on a stale PID file or lone pgrep result
	// (#4793 via a /proc/self magic link). sameProcessRoot only compares root and
	// mount-namespace identity, not /proc/self resolution, so a same-namespace
	// daemon is not caught by it. Treat a process-relative procfs home as
	// unverifiable (the daemon's own file ops resolved it in its frame, and the
	// caller cannot) rather than guessing ours and signalling it.
	if isProcessRelativeProcfsHome(gotHome) {
		return daemonUnverifiable
	}
	// The spelling guard does not see through a symlink whose TARGET is a
	// process-relative procfs path: AGENT_FACTORY_HOME=link where link ->
	// /proc/self/cwd/state resolves the daemon's home in the DAEMON's frame
	// (<its cwd>/state) but canonicalDir follows the link in the CALLER's
	// frame, so a same-UID, same-namespace foreign daemon launched from a
	// different cwd compares equal to wantHome and is misclassified daemonOurs
	// (#4793 via a procfs-indirected symlink). sameProcessRoot does not catch
	// this (root and mount-namespace match). Walk the symlink chain and treat a
	// home whose resolved chain enters a process-relative procfs path as
	// unverifiable — see homeSymlinkEntersProcessRelativeProcfs.
	if homeSymlinkEntersProcessRelativeProcfs(gotHome) {
		return daemonUnverifiable
	}
	got, err := canonicalDir(gotHome)
	if err != nil {
		return daemonUnverifiable
	}
	// gotHome was resolved and canonicalDir above renders it in the CALLER's
	// filesystem frame. An ABSOLUTE home such as AGENT_FACTORY_HOME=/state is
	// interpreted in the CANDIDATE's frame (its root / mount namespace), not
	// the caller's: a same-UID daemon in a container, chroot, or different mount
	// namespace whose /state is a different directory than the caller's /state
	// would resolve to the caller's /state here, compare equal to wantHome, be
	// classified daemonOurs, and be signalled through a stale PID file or lone
	// pgrep result — the #4793 hazard via a path collision across namespaces.
	// When the candidate's frame is not the caller's, or cannot be established,
	// the textual comparison cannot be trusted, so fail closed
	// (daemonUnverifiable) rather than guessing ours.
	if !sameProcessRoot(pid) {
		return daemonUnverifiable
	}
	if got != wantHome {
		return daemonForeign
	}
	return daemonOurs
}

// resolveHomeInDaemonFrame absolutizes a daemon's (possibly relative) home
// against the DAEMON's own working directory rather than the caller's. An
// absolute home is returned unchanged; a relative one is joined to the cwd
// proctree.WorkingDir reports for pid; a relative one whose cwd cannot be read
// is reported unresolvable — resolving it against ours instead would make two
// daemons with the same relative value but different launch directories compare
// equal, the cross-home hazard #4793 turns on. See daemonProcessHome in
// doctor/skew.go for the same frame decision.
func resolveHomeInDaemonFrame(pid int, home string) (string, bool) {
	if filepath.IsAbs(home) {
		return home, true
	}
	cwd, ok := proctree.WorkingDir(pid)
	if !ok || cwd == "" {
		return "", false
	}
	return filepath.Join(cwd, home), true
}

// procRootFor returns the filesystem root the kernel exposes for pid on Linux
// (/proc/<pid>/root), resolved through the caller's frame so a symlinked root
// (a chroot reachable through /var/...) compares by its real target. The ok
// return is false when /proc is not present (not Linux — no mount-namespace
// hazard, sameProcessRoot keeps the existing behavior) or when the root symlink
// cannot be read (the process exited between the uid/environ probes above and
// this read, or the link is unavailable). A package var so a test can simulate
// a candidate whose root differs from the caller's — a container, chroot, or
// distinct mount namespace — which CI runners cannot create unprivileged.
var procRootFor = func(pid int) (string, bool) {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(link); err == nil {
		link = resolved
	}
	return link, true
}

// procMountNSFor returns the inode identifying pid's mount namespace on Linux
// (/proc/<pid>/ns/mnt), so a candidate that shares the caller's root string but
// lives in a distinct mount namespace — a bind-mount at the configured home
// path whose backing store is a different directory — is not read in the
// caller's frame. A distinct mount namespace can keep "/" as the same root
// string while bind-mounting a different directory at an absolute
// AGENT_FACTORY_HOME, so the root-string comparison procRootFor performs is
// not enough on its own: the same root with a different mount table can still
// name a different directory at the home path. The ok return is false when
// /proc is not present (not Linux — no mount-namespace hazard, the existing
// root comparison stands) or when the ns/mnt link cannot be read (the process
// exited between probes, or the link is unavailable). A package var so a test
// can simulate a candidate whose mount namespace differs from the caller's,
// which CI runners cannot create unprivileged.
var procMountNSFor = func(pid int) (uint64, bool) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if err != nil {
		return 0, false
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Ino, true
}

// sameProcessRoot reports whether the process at pid shares the caller's
// filesystem root, so an absolute path resolved and compared in the caller's
// frame names the same directory the candidate sees. classifyDaemonHome
// resolves the candidate's home through the CALLER's frame (canonicalDir); a
// candidate in a container, chroot, or different mount namespace has its own
// root (/proc/<pid>/root is a symlink there), so its absolute AGENT_FACTORY_HOME
// is interpreted in THAT frame and an equal textual spelling is not the same
// directory. When the roots differ the comparison cannot be trusted and
// classifyDaemonHome fails closed (daemonUnverifiable) rather than guessing
// ours and signalling a cross-namespace daemon.
//
// Linux-only: /proc is the kernel's per-process root surface. On platforms
// without /proc (macOS) there is no equivalent filesystem-namespace hazard and
// this returns true so the existing path resolution stands; a /proc that IS
// present but whose root link cannot be read is a frame we cannot establish,
// so it returns false and the candidate falls through unverifiable.
func sameProcessRoot(pid int) bool {
	if _, err := os.Stat("/proc"); err != nil {
		return true
	}
	cand, ok := procRootFor(pid)
	if !ok {
		return false
	}
	self, ok := procRootFor(os.Getpid())
	if !ok {
		return false
	}
	if cand != self {
		return false
	}
	// Equal root strings are not enough: a distinct mount namespace can keep
	// the same root path while bind-mounting a different directory at an
	// absolute AGENT_FACTORY_HOME, so the textual home comparison the root
	// check guards still resolves to a different directory. Compare mount-
	// namespace identity so such a candidate fails closed (#4793).
	return sameMountNamespace(pid)
}

// sameMountNamespace reports whether pid lives in the caller's mount namespace.
// /proc/<pid>/ns/mnt is a symlink whose inode names the namespace; equal inodes
// mean the same mount table, so an equal root and home path name the same
// directory. A distinct inode means the candidate sees a different mount table
// even with a matching root, so an equal textual AGENT_FACTORY_HOME is not the
// same directory — sameProcessRoot fails closed rather than reading the
// candidate's path in the caller's frame. On platforms without /proc (macOS)
// there is no mount-namespace hazard and this returns true so the root
// comparison stands; an unreadable ns/mnt (the process exited between probes)
// returns false so the candidate falls through unverifiable.
func sameMountNamespace(pid int) bool {
	if _, err := os.Stat("/proc"); err != nil {
		return true
	}
	cand, ok := procMountNSFor(pid)
	if !ok {
		return false
	}
	self, ok := procMountNSFor(os.Getpid())
	if !ok {
		return false
	}
	return cand == self
}

// scanDaemonCandidatesFn is the process-scan entry point used by
// locateDaemonPID. It is a function var so tests can substitute a controlled
// candidate list: the real pgrep scan is host-wide, so on any machine running
// the supervised daemon (`af daemon install`, the recommended setup since
// #791) it finds that unrelated process and the stale-PID / no-candidates
// branches become unreachable — and worse, a test exercising the fallback
// could SIGTERM the host's real daemon (#793).
var scanDaemonCandidatesFn = pgrepDaemonCandidates

// errPgrepUnavailable signals that `pgrep` is not on PATH, distinct from
// "pgrep ran and returned no matches". Surfaced by locateDaemonPID so the
// caller can build an actionable error rather than misclassifying the running
// daemon as absent (#553).
var errPgrepUnavailable = errors.New("pgrep not found in PATH")

// pgrepDaemonCandidates scans for processes whose full command line carries a
// "--daemon" token, then keeps only those whose binary is `af` or
// `agent-factory`. The `--` before the pattern stops pgrep from treating the
// leading-dash pattern as a flag. We match the bare `--daemon` token rather
// than "af --daemon" so source-built daemons running as `agent-factory
// --daemon` are found too (#937); argsAreDaemonBinary restores the
// binary-name specificity the old substring gave. Go test binaries living
// under /tmp/Test* and the current process are excluded. We rely on pgrep -f
// rather than parsing /proc directly so the path works on both Linux and macOS.
func pgrepDaemonCandidates() ([]int, error) {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		log.WarningLog.Printf("sigterm fallback: pgrep not found in PATH; cannot scan for daemons: %v", err)
		return nil, fmt.Errorf("%w: %v", errPgrepUnavailable, err)
	}
	out, err := exec.Command(pgrep, "-f", "--", "--daemon").Output()
	if err != nil {
		// pgrep exits 1 when there are no matches. Treat that as zero
		// candidates rather than an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep failed: %w", err)
	}

	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, parseErr := strconv.Atoi(line)
		if parseErr != nil {
			continue
		}
		if pid == self {
			continue
		}
		// Defensive: re-read the argv (boundaries preserved, so a spaced binary
		// path in argv[0] is classified correctly — #1214) and require it to (a)
		// contain "--daemon" as a discrete token (pgrep -f does substring
		// matching, not token matching), (b) belong to an `af`/`agent-factory`
		// binary so the broad `--daemon` pattern can't match an unrelated daemon
		// (#937), and (c) not be a Go test binary (these live under /tmp/Test...
		// when invoked from `go test`).
		args := daemonArgs(pid)
		if len(args) == 0 {
			continue
		}
		if !argsHaveDaemonFlag(args) {
			continue
		}
		if !argsAreDaemonBinary(args) {
			continue
		}
		if isTestBinaryArgs(args) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// isTestBinaryArgs filters out Go test binaries spawned during `go test`.
// They typically live under /tmp/Test<name>... (t.TempDir paths) or
// /tmp/go-build... (compiled-test cache). We don't want a test that spawns a
// fake "af --daemon" subprocess to accidentally claim a real daemon match
// when run in parallel with another test.
func isTestBinaryArgs(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "/tmp/Test") || strings.HasPrefix(a, "/tmp/go-build") {
			return true
		}
	}
	return false
}

// formatPIDList renders []int as a comma-separated list for user-facing
// error messages.
func formatPIDList(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}
