package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
)

// This file backs the daemon half of `af reset` (#1913 follow-up): a factory
// reset that leaves a daemon running has not reset anything the daemon holds in
// memory, and the daemon a reset misses is precisely the one that causes the
// failure reset is meant to cure — an OLD binary left over from an upgrade,
// still bound to the control socket, rejecting a NEW client's requests with
// "unknown field tab_id".
//
// StopDaemon alone cannot do this. It follows daemon.pid, so it finds exactly
// one daemon: the managed one. A daemon started before the PID file existed
// (pre-1.0.69), a second daemon that lost the singleton race, or a source-built
// `agent-factory --daemon` all leave no PID file to follow and survive.

// scopedDaemonScanFn is the candidate-scan entry point used by
// StopOrphanDaemons. It is a function var for the same reason
// scanDaemonCandidatesFn is (#793): the underlying scan is HOST-WIDE, so on any
// machine running the supervised daemon a test would find that real daemon
// among its candidates. The uid+home filter below is what makes this safe in
// production, and the seam is what makes it safe to test the signalling path
// without depending on that filter being perfect.
var scopedDaemonScanFn = scanDaemonCandidatesFn

// StopOrphanDaemons stops every af daemon that belongs to THIS user and THIS AF
// home, and returns the PIDs it actually stopped, ascending.
//
// It is the second half of the reset teardown, run AFTER StopDaemon: StopDaemon
// handles the PID-file daemon, this handles every daemon that never wrote one.
// By the time it runs the managed daemon is normally already gone, so it
// usually finds nothing; the case it exists for is the leftover that reset
// previously only printed a `pkill` hint about.
//
// SCOPE IS THE WHOLE DESIGN HERE. This function sends SIGTERM/SIGKILL to
// processes it discovered by scanning the process table, so every filter is
// load-bearing and each one narrows a way to hit the wrong process:
//
//   - an af/agent-factory binary carrying a discrete --daemon token
//     (argsAreDaemonBinary + argsHaveDaemonFlag, reused from the PID-file path
//     so both agree — #1004), never a substring match on some other project's
//     `--daemonize`;
//   - owned by OUR uid, so a shared box never has one user's reset touch
//     another's daemon (load-bearing only when running as root, where a foreign
//     environ is readable — see verifyScopedDaemon);
//   - serving OUR AF home, so a reset of one AGENT_FACTORY_HOME never stops the
//     daemon of another. Running several homes side by side is a supported,
//     documented setup; a reset that killed all of them would be a far worse
//     bug than the stale daemon it was cleaning up.
//
// A candidate whose home CANNOT be established is left alone and reported to
// the caller rather than signaled. "I could not tell" must never resolve to
// "kill it": the cost of skipping is a leftover daemon and a printed warning,
// while the cost of guessing wrong is killing a working daemon that reset was
// never asked to touch.
//
// Errors stopping individual daemons are joined and returned alongside whatever
// was stopped — a reset reports how far it got rather than aborting.
func StopOrphanDaemons(configDir string) (stopped []int, unverified []int, err error) {
	ours, unverified, err := ScanScopedDaemons(configDir)
	if err != nil {
		return nil, nil, err
	}

	uid := os.Getuid()
	wantHome, homeErr := canonicalDir(configDir)
	if homeErr != nil {
		return nil, nil, fmt.Errorf("failed to resolve the af home %q: %w", configDir, homeErr)
	}

	var errs []error
	for _, pid := range ours {
		// Re-verify at signal time. A PID is a reusable kernel handle, so a
		// candidate can exit and its number be recycled between the scan and
		// this line; re-reading argv is the same TOCTOU narrowing StopDaemon
		// applies to a stale PID file.
		if verifyScopedDaemon(pid, uid, wantHome) != daemonOurs {
			continue
		}
		// signalAndWait is SIGTERM, poll, then SIGKILL only if the grace expires
		// — the same escalation StopDaemon uses, so a daemon stopped here still
		// gets to run its SaveInstances() handler (#571). It returns only once
		// the process is GONE, which is the property the reset depends on: a
		// daemon still running its shutdown flush would write instances.json
		// back out after the wipe deleted it.
		if err := signalAndWait(pid); err != nil {
			errs = append(errs, fmt.Errorf("stop daemon pid %d: %w", pid, err))
			continue
		}
		stopped = append(stopped, pid)
	}
	sort.Ints(stopped)
	return stopped, unverified, errors.Join(errs...)
}

// ScanScopedDaemons returns the live af daemons that belong to this user and
// this AF home (ours), and the af daemon processes whose home could NOT be
// established (unverified). It is the shared basis for both stopping orphans
// and asserting that nothing is left running.
func ScanScopedDaemons(configDir string) (ours []int, unverified []int, err error) {
	wantHome, err := canonicalDir(configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve the af home %q: %w", configDir, err)
	}

	candidates, err := scopedDaemonScanFn()
	if err != nil {
		// A missing pgrep is not a reset failure by itself, but it DOES mean we
		// cannot prove the field is clear. Report it as a scan error and let the
		// caller decide — reset treats "cannot verify" as a reason to abort
		// rather than wipe under a daemon it cannot see.
		if errors.Is(err, errPgrepUnavailable) {
			return nil, nil, fmt.Errorf("cannot scan for running daemons: %w", err)
		}
		return nil, nil, fmt.Errorf("failed to scan for running daemons: %w", err)
	}

	uid := os.Getuid()
	self := os.Getpid()
	for _, pid := range candidates {
		if pid <= 1 || pid == self {
			continue
		}
		switch verifyScopedDaemon(pid, uid, wantHome) {
		case daemonOurs:
			ours = append(ours, pid)
		case daemonUnverifiable:
			unverified = append(unverified, pid)
		case daemonForeign:
		}
	}
	sort.Ints(ours)
	sort.Ints(unverified)
	return ours, unverified, nil
}

// AssertNoLiveDaemon returns an error naming what is still running if ANY af
// daemon for this home is alive, or if anything still answers on the control
// socket.
//
// This is the barrier `af reset` must clear before it deletes a single byte of
// state, and it is the reason the reset is safe rather than merely careful. The
// daemon is the SINGLE WRITER of session state (#960): it holds every instance
// in memory and persists them on a tick and again on its shutdown path
// (RunDaemon's final SaveInstances). A wipe that races a live daemon does not
// half-work — it loses. The daemon writes instances.json back out from memory
// after the delete, and the "factory reset" hands the user their old sessions
// back, which is precisely the resurrection this whole change exists to stop.
//
// So the rule is: prove the field is clear, or do not wipe. A reset that aborts
// with an explanation leaves the user exactly where they started and costs them
// one command; a reset that guesses wrong leaves them with a half-deleted home
// and a daemon writing into it.
func AssertNoLiveDaemon(configDir string) error {
	if err := pingDaemon(); err == nil {
		return errors.New("a daemon is still answering on the control socket")
	}
	ours, unverified, err := ScanScopedDaemons(configDir)
	if err != nil {
		return err
	}
	if len(ours) > 0 {
		return fmt.Errorf("af daemon(s) still running for this AF home: %s", formatPIDList(ours))
	}
	if len(unverified) > 0 {
		return fmt.Errorf("af daemon process(es) whose AF home could not be verified are still running: %s; "+
			"if one of these serves this AF home, stop it before resetting", formatPIDList(unverified))
	}
	return nil
}

// daemonScope is the outcome of deciding whether a scanned PID is a daemon this
// reset owns.
type daemonScope int

const (
	// daemonOurs is an af daemon owned by this uid and serving this AF home.
	daemonOurs daemonScope = iota
	// daemonForeign provably is not ours — a different uid, a different AF
	// home, or no longer an af daemon at all.
	daemonForeign
	// daemonUnverifiable is an af daemon whose home could not be established,
	// so it is neither ours nor provably foreign. Never signaled.
	daemonUnverifiable
)

// verifyScopedDaemon decides whether pid is a daemon this reset may stop. It is
// called immediately before signalling as well as during the scan: a PID is a
// reusable kernel handle, so a candidate can exit and its number be recycled by
// an unrelated process in the window between the two. Re-reading argv at signal
// time is the same TOCTOU narrowing StopDaemon applies to a stale PID file.
func verifyScopedDaemon(pid, uid int, wantHome string) daemonScope {
	// Still an af daemon? Re-read argv rather than trusting the scan: this is
	// the check that stops a recycled PID from being signaled.
	if !isAgentFactoryDaemon(pid) {
		return daemonForeign
	}
	if isTestBinaryArgs(daemonArgs(pid)) {
		return daemonForeign
	}

	owner, ok := processUID(pid)
	if !ok {
		return daemonUnverifiable
	}
	if owner != uid {
		return daemonForeign
	}

	env, readable := daemonHomeEnv(pid)
	if !readable {
		return daemonUnverifiable
	}
	home, err := config.ConfigDirFor(env)
	if err != nil {
		// The daemon holds an AGENT_FACTORY_HOME we cannot resolve. Unresolvable
		// is not "not ours" — say so instead of guessing.
		log.WarningLog.Printf("reset: cannot resolve AGENT_FACTORY_HOME=%q for daemon pid %d: %v", env, pid, err)
		return daemonUnverifiable
	}
	got, err := canonicalDir(home)
	if err != nil {
		return daemonUnverifiable
	}
	if got != wantHome {
		return daemonForeign
	}
	return daemonOurs
}

// canonicalDir renders a directory path in a form two independently-resolved
// paths can be compared with. AF homes reach us from different places — ours
// from GetConfigDir, a daemon's from its environ — and may differ textually
// while naming the same directory: relative vs absolute (GetConfigDir does not
// absolutize — #1873), or through a symlink (/tmp vs /private/tmp on macOS).
//
// EvalSymlinks is best-effort: it fails on a path that does not exist, which is
// legitimate here (an AF home that has not been created yet), so a failure
// falls back to the lexical form rather than failing the comparison.
func canonicalDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return filepath.Clean(abs), nil
}

// processUID returns the uid owning pid. The second return is false when
// ownership cannot be determined (the process exited, or the platform will not
// say), which callers must treat as "unknown", never as "ours".
//
// Through proctree rather than /proc directly: this read had no darwin path, so
// on macOS it answered "unknown" for every process, and classifyDaemon returned
// daemonUnverifiable for every daemon — meaning `af reset` could never verify,
// and therefore never stop, anything. That is the same user-facing breakage as
// #1942, from the same root (#1939): a Linux-only inspection primitive with no
// backend for the platform we ship.
func processUID(pid int) (int, bool) {
	return proctree.UID(pid)
}

// daemonHomeEnv reports the AGENT_FACTORY_HOME a running process was started
// with. The second return distinguishes the two cases that a plain lookup
// conflates, and getting that distinction wrong breaks the reset in one
// direction or the other:
//
//   - (value, true): the environ was READ. An empty value means the variable is
//     genuinely absent, so the daemon resolved the DEFAULT home — this is the
//     COMMON case, since almost nobody sets AGENT_FACTORY_HOME, and treating it
//     as "unknown" would skip exactly the everyday stale daemon reset exists to
//     kill.
//   - ("", false): the environ was UNREADABLE — a foreign uid, or the process
//     exited. We cannot tell which home it serves, and must not guess it is the
//     default.
//
// The environ is fixed at exec and cannot be shed, so what it says about the
// home a daemon resolved at startup stays true for the life of the process.
//
// Read through proctree.Environ rather than /proc directly, which is what makes
// the first case reachable on darwin at all: before #1939 this was a bare /proc
// read, so on macOS EVERY daemon landed in the second case and `af reset` left
// all of them alone. Environ (not EnvValue) precisely because EnvValue folds
// "absent" and "unreadable" into one false, and the whole point of this
// function is that those two must not be confused.
func daemonHomeEnv(pid int) (string, bool) {
	home, status := proctree.LookupEnv(pid, "AGENT_FACTORY_HOME")
	switch status {
	case proctree.EnvFound:
		return home, true
	case proctree.EnvAbsent:
		// READ, and the variable genuinely is not set — so this daemon
		// resolved the DEFAULT home. This is the common case and must keep
		// working: almost nobody sets AGENT_FACTORY_HOME, and treating it as
		// unknown would skip exactly the everyday stale daemon reset exists
		// to kill.
		return "", true
	default:
		// EnvUnknown. NOT "unset": we were not allowed to look. Returning
		// ("", true) here would resolve the DEFAULT home, and if that happens
		// to be ours we would SIGTERM a daemon whose home we never read. The
		// caller turns this into daemonUnverifiable and leaves it alone.
		return "", false
	}
}

// RemoveRuntimeSockets unlinks the Unix sockets under the AF home that a client
// could otherwise dial into a dead endpoint: the gob control socket
// (daemon.sock), the HTTP/JSON socket (daemon-http.sock), and the per-session
// VS Code editor sockets (vscode/*.sock). It returns the paths it removed.
//
// It MUST run only after every daemon has been stopped, and it enforces that
// itself rather than trusting the caller: if anything still ANSWERS on the
// control socket, it removes nothing and says so. Unlinking a LIVE daemon's
// socket is the #767 failure — the daemon keeps serving an inode with no name,
// every dial against the path fails, and the next EnsureDaemon spawns a second
// daemon while the first leaks forever. That is strictly worse than the stale
// socket this function exists to clear, so "a daemon answered" is a reason to
// stop, not a race to win.
func RemoveRuntimeSockets(dir string) ([]string, error) {
	if err := pingDaemon(); err == nil {
		return nil, errors.New("a daemon is still answering on the control socket; " +
			"refusing to remove the sockets it is serving (removing a live daemon's socket " +
			"strands it and leaks a second one — #767)")
	}

	var removed []string
	var errs []error
	remove := func(path string) {
		err := os.Remove(path)
		switch {
		case err == nil:
			removed = append(removed, path)
		case os.IsNotExist(err):
			// Already gone: the daemon's own teardown unlinks its socket, so the
			// happy path lands here. Nothing to report.
		default:
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}

	remove(filepath.Join(dir, daemonSocketFileName))
	remove(filepath.Join(dir, daemonHTTPSocketFileName))

	// Editor sockets are swept with the daemon's OWN recognizer rather than a
	// "*.sock" glob: an operator can point AGENT_FACTORY_HOME at a directory
	// already in use, and reset must not delete a file it did not mint. See
	// isAbandonedVSCodeSocket for why name AND socket-ness are both checked.
	sockDir := filepath.Join(dir, vscodeSocketDirName)
	entries, err := os.ReadDir(sockDir)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("read %s: %w", sockDir, err))
	}
	for _, e := range entries {
		if isAbandonedVSCodeSocket(sockDir, e) {
			remove(filepath.Join(sockDir, e.Name()))
		}
	}

	sort.Strings(removed)
	return removed, errors.Join(errs...)
}
