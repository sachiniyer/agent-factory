package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
)

// ExistsOrUnknown reports session existence as a deliberately LOSSY bool: it
// returns true for "the session exists OR tmux could not be reached (a wedged /
// timed-out has-session)", and false ONLY for "definitively absent" — tmux
// answered and said no such session. A wedged has-session is laundered into true
// here (via sessionExists), the conservative lie a bool is forced to tell.
//
// The name states the lie so it is visible at every call site. Contract:
//
//   - A caller MAY act on !ExistsOrUnknown() ("definitively gone"): classify a
//     failed command as ErrSessionGone, or skip the idempotent teardown of an
//     already-absent session. A wedged server reads as "exists", so these callers
//     never falsely tear down or abandon a session that is merely slow — the safe
//     direction, and the reason this form is kept.
//   - A caller MUST NOT read a bare true as proof of life: "true" folds in "I
//     could not tell". Any site that treats existence as EVIDENCE or as a POSITIVE
//     gate must call ProbeSession() and handle !known explicitly (#1917/#1962).
func (t *TmuxSession) ExistsOrUnknown() bool {
	return sessionExists(t.cmdExec, t.sanitizedName)
}

// sessionExists reports whether a tmux session with the exact name `name`
// currently exists. Shared by ExistsOrUnknown and the receiver-less
// CleanupSessions path so both probe identically.
//
// Bounded by tmuxCommandTimeout (#1917): has-session against a wedged server
// parks forever, and this probe is the fallback on nearly every tmux error path
// in the package — including Close's, on the daemon's undeletable-session
// teardown.
//
// A tripped deadline reports TRUE (exists). The bool cannot express "unknown",
// so the answer has to be the one that is safe to be wrong about, and every
// caller that acts destructively acts on FALSE: Close reaps the session's
// process trees, and io.go/clientless.go raise ErrSessionGone, which the daemon
// reads as a confirmed death. A false "gone" against a server that is merely
// wedged would SIGKILL a live agent's process tree and tear down a session that
// is still running — the exact mistake tmuxTimeoutContext exists to prevent. A
// false "exists" only costs a best-effort skip, so it is the conservative lie.
//
// Callers that must not paper over the difference do not use this probe at all:
// they check ctx.Err() on their own bounded command and skip it entirely (see
// tmuxTimeoutContext, and Close's kill-session timeout branch).
//
// A non-timeout failure — the usual `has-session` exit 1 for "no such session",
// or any other tmux error — still reports false, preserving the pre-#1917
// conflation callers already relied on.
func sessionExists(cmdExec cmd.Executor, name string) bool {
	exists, known := probeSession(cmdExec, name)
	if !known {
		log.WarningLog.Printf("tmux has-session for %s timed out after %s; the server is wedged, so "+
			"reporting the session as still present rather than risk a false teardown", name, tmuxCommandTimeout)
		return true
	}
	return exists
}

// ProbeSession reports whether this session exists AND whether tmux actually
// ANSWERED — the tri-state a bool cannot express (#1917 round 8).
//
// ExistsOrUnknown has to pick yes or no, so a timed-out probe becomes "yes": the
// conservative lie, safe for the read-only callers that only ever act on "no". But
// it launders UNKNOWN into AFFIRMATIVE at the bottom of the stack, and every caller
// above is then downstream of a lie it cannot detect — which is how a wedged tmux
// server came to be reported as a live agent, and how a liveness counter built on
// affirmative evidence got fooled anyway. Callers that treat "alive" as EVIDENCE
// must take this form; callers that merely need a bool keep the lie, knowingly.
func (t *TmuxSession) ProbeSession() (exists bool, known bool) {
	return probeSession(t.cmdExec, t.sanitizedName)
}

// probeSession is sessionExists WITHOUT the lossy collapse: it reports whether
// the session exists AND whether tmux actually answered.
//
// The two-value form exists because the collapse above, while safe for the
// probe's many read-only callers, silently destroyed information for the one
// caller that tears sessions down: Close asked "does it still exist?", got back
// a `true` synthesized from a TIMEOUT, and reported an ordinary kill failure —
// so its caller deleted the workspace with the session's fate unknown (#1917).
// A caller that acts on the answer takes this form and handles !known; a caller
// that only reads takes the bool and gets the conservative lie.
func probeSession(cmdExec cmd.Executor, name string) (exists bool, known bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	// Using "-t name" does a prefix match, which is wrong. `-t=` does an exact match.
	err := runTmuxBoundedWith(ctx, cmdExec, "has-session", fmt.Sprintf("-t=%s", name))
	if err == nil {
		return true, true
	}
	if ctx.Err() != nil {
		return false, false
	}
	// tmux answered: the usual `has-session` exit 1 for "no such session", or any
	// other error, which this probe has always conflated with absence.
	return false, true
}

// probeSessionStrict is the non-lossy existence probe for CleanupSessions'
// ownership gate. Unlike probeSession, it does not treat every non-timeout
// execution failure as absence: only tmux's ordinary exit 1 paired with its
// exact "can't find session" diagnostic is a determinate answer. Any other
// failure remains unknown and cannot authorize reset to continue to worktree
// deletion.
func probeSessionStrict(cmdExec cmd.Executor, name string) (exists bool, known bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	_, err = outputTmuxBoundedWith(ctx, cmdExec, "has-session", fmt.Sprintf("-t=%s", name))
	if err == nil {
		return true, true, nil
	}
	if ctx.Err() != nil {
		return false, false, fmt.Errorf("%w: has-session %s after %s", ErrTmuxTimeout, name, tmuxCommandTimeout)
	}
	if missingTmuxSession(err, name) {
		return false, true, nil
	}
	return false, false, fmt.Errorf("has-session for %s did not return a usable answer: %w", name, err)
}

// missingTmuxSession recognizes tmux's explicit exact-target absence answer.
// Exit status 1 alone is ambiguous: wrapper, policy, and unclassified connection
// failures can return the same status while the named session remains unknown.
// A definitive no-server answer counts too: no session can remain on a socket
// that holds no server — routed through NoServerRunning so the has-session
// re-probes get the same ENOENT treatment as the listing. The finding on #2875
// named this path explicitly: `tmux ls` may find AF sessions, the socket may then
// be removed by a /tmp cleaner, and this fallback probe would answer ENOENT while
// the server and its panes are still running.
func missingTmuxSession(err error, name string) bool {
	diagnostic, ok := tmuxExitOneDiagnostic(err)
	if !ok {
		return false
	}
	return diagnostic == "can't find session: "+name || NoServerRunning(err)
}

// NoServerRunning reports whether a failed tmux command's error is tmux's
// DEFINITIVE "there is no server on this socket" answer, and therefore a
// determinate EMPTY rather than a read that did not happen.
//
// The invariant, stated here rather than delegated to a "mirrors X" comment
// somewhere else: A FAILED READ IS NOT AN EMPTY RESULT. Anything that turns a
// tmux error into "there are no sessions" has to come through this function or
// it is guessing — and both callers so far reached that guess on a path that
// then destroys something (#2870 deletes worktrees, #2874 kills processes).
//
// Exported for `af doctor`, which shells out to tmux itself rather than through
// this package. A second implementation is exactly how the first one spread.
func NoServerRunning(err error) bool {
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	if !exitOne {
		return false
	}
	switch classifyNoServerDiagnostic(diagnostic) {
	case serverRefusedConnection:
		// The socket EXISTS and refused us. Nothing is listening on it, which
		// is self-sufficient proof: no server, so no sessions.
		return true
	case socketAbsent:
		// ENOENT is NOT self-sufficient. tmux(1) documents that a socket
		// removed by accident can be recreated by signalling the server, so a
		// server whose socket a /tmp cleaner unlinked is still running, with
		// live sessions and live panes, while every client gets ENOENT.
		// Reproduced: a session and its `sleep 300` pane survived `rm -f` of
		// the socket, and `tmux ls` answered exactly this diagnostic (#2875).
		//
		// It is only definitive when no tmux server is alive to own an
		// unlinked socket — which is the ordinary case this branch exists for,
		// a machine where tmux never ran.
		return len(tmuxServerProcessPIDs()) == 0
	default:
		return false
	}
}

// noServerDiagnosticKind is why tmux says there is no server, because the two
// reasons carry different weight and collapsing them is what #2875 caught.
type noServerDiagnosticKind int

const (
	notANoServerDiagnostic noServerDiagnosticKind = iota
	// serverRefusedConnection: ECONNREFUSED. The socket is there; nobody is
	// behind it. Definitive on its own.
	serverRefusedConnection
	// socketAbsent: ENOENT. Either no server ever created one — or one did and
	// the socket was removed out from under it.
	socketAbsent
)

// tmuxServerProcessAlive reports whether any tmux SERVER process is running for
// this uid. A package var so tests can pin the answer: the real probe reads the
// host's process table, so a developer box with tmux running would otherwise
// give a different result from a container that has none.
var tmuxServerProcessPIDs = liveTmuxServerPIDs

// PinServerProbeForTest fixes the "is a tmux server alive for this uid?" answer
// and returns the restore. It exists for internal/testguard.IsolateTmux, which
// declares a private-socket world for a test; without it that isolation is
// incomplete, because the probe reads the HOST process table and would see the
// developer's own tmux servers inside a world that is supposed to have none.
//
// Test-only by contract, not by build tag: testguard lives in internal/ and is
// the only caller.
func PinServerProbeForTest(pids ...int) (restore func()) {
	prev := tmuxServerProcessPIDs
	tmuxServerProcessPIDs = func() []int { return pids }
	return func() { tmuxServerProcessPIDs = prev }
}

// liveTmuxServerProcess looks for a tmux server owned by this uid.
//
// It answers a deliberately BROAD question — "could any live server own an
// unlinked socket?" — rather than "is the server for THIS socket alive?", which
// would need the server's own socket path and is not worth the machinery. The
// asymmetry is chosen: a false "yes" costs a refused sweep the user can retry
// after checking; a false "no" lets reset delete worktrees out from under a live
// agent. Over-refusal is also rare in practice, because a server on the DEFAULT
// socket has a socket file, so it answers ECONNREFUSED rather than ENOENT.
//
// A process table we cannot read reports TRUE, for the same reason: an
// unreadable table is not evidence of absence.
func liveTmuxServerPIDs() []int {
	snap, err := proctree.Snapshot()
	if err != nil {
		// Not evidence of absence. A sentinel PID would be a lie, so report an
		// unnamed one: callers only test emptiness, and the message degrades to
		// "a server is running" without claiming to know which.
		return []int{0}
	}
	uid := os.Getuid()
	var pids []int
	for pid, p := range snap {
		if !isTmuxServerComm(p.Comm) {
			continue
		}
		if owner, ok := proctree.UID(pid); ok && owner != uid {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// ListSessionNames returns the name of every session on the tmux server, or an
// error when tmux could not tell us — the same three-valued contract
// CleanupSessions applies to its own listing, and for the same reason: a caller
// that reads a missing name as proof the session is DEAD will act on it.
//
// Exported for `af doctor` (#2874/#2910). Doctor shelled out to `tmux ls`
// itself, which put its listing outside two invariants this package maintains
// and states as obligations:
//
//   - the classification of tmux's ambiguous exit 1 (see NoServerRunning), and
//   - the BOUND. Every tmux command here runs under tmuxCommandTimeout through
//     boundedTmuxCommand, because a wedged server parks the client forever and a
//     bare context is not a bound — it has to carry the process-group kill and
//     WaitDelay too (#1787/#2099). Doctor's copy had neither, so `af doctor
//     --fix` could hang in the cleanup phase against a server that wedged
//     mid-run, instead of refusing the removal it could no longer justify.
//
// A tripped deadline is an error, never an empty list: a server that did not
// answer has told us nothing about what is running on it.
func ListSessionNames(cmdExec cmd.Executor) ([]string, error) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, "ls", "-F", "#{session_name}")
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		if timedOut {
			return nil, fmt.Errorf("%w: tmux ls after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if NoServerRunning(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("could not list tmux sessions%s: %w", tmuxDiagnosticSuffix(err), err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// CommandDiagnostic returns what tmux wrote to stderr for a failed command,
// whitespace-collapsed onto one line, or "" when there is nothing to report.
//
// It exists because an (*exec.ExitError).Error() is only "exit status N": tmux's
// actual reason — the socket it could not reach and why — lives in Stderr, and
// an error that omits it tells the user of a refused operation nothing about
// what to fix.
func CommandDiagnostic(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	return strings.Join(strings.Fields(string(exitErr.Stderr)), " ")
}

// tmuxExitOneDiagnostic returns the trimmed stderr of a tmux command that
// exited with status 1, and whether it did. Only status 1 qualifies: tmux
// reports both "the thing you asked about is absent" and "I could not reach the
// server" with it, so the diagnostic is the ONLY thing that separates them, and
// any other status is a failure mode tmux does not document a diagnostic for.
func tmuxExitOneDiagnostic(err error) (string, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return "", false
	}
	return strings.TrimSpace(string(exitErr.Stderr)), true
}

// noTmuxServerDiagnostic reports whether a tmux exit-1 diagnostic is tmux's
// DEFINITIVE "there is no server on this socket" answer — which, for a listing,
// is the difference between "there are no sessions" and "I could not find out".
//
// tmux's client prints the ECONNREFUSED case by name and routes every other
// connect(2) failure through strerror, so exactly two diagnostics are definitive
// (both measured against tmux 3.4):
//
//   - "no server running on <socket>" — the socket exists and refused us, so
//     nothing is listening on it. A server that exits leaves its socket file
//     behind, so this is what a server that has DIED says.
//   - "error connecting to <socket> (No such file or directory)" — ENOENT: the
//     socket does not exist, so no server ever created one there. This is the
//     ordinary answer on a machine with no tmux server, and it is why the set
//     cannot be narrowed to the line above: doing so would make `af reset`
//     refuse to run for most users (#2870).
//
// Everything else leaves the session set unknown — (Permission denied) and other
// connect failures, tmux's own socket-directory refusals, a wrapper's exit 1,
// and an empty diagnostic. A "no server" line that names no socket is not tmux's
// answer either, so it is not accepted.
//
// The strings are matched in tmux's own C locale: tmux calls setlocale only for
// LC_CTYPE, so strerror stays untranslated regardless of the user's LANG.
func classifyNoServerDiagnostic(diagnostic string) noServerDiagnosticKind {
	if socket, refused := strings.CutPrefix(diagnostic, "no server running on "); refused {
		if strings.TrimSpace(socket) != "" {
			return serverRefusedConnection
		}
		return notANoServerDiagnostic
	}
	rest, connectFailure := strings.CutPrefix(diagnostic, "error connecting to ")
	if !connectFailure {
		return notANoServerDiagnostic
	}
	if socket, absent := strings.CutSuffix(rest, " (No such file or directory)"); absent &&
		strings.TrimSpace(socket) != "" {
		return socketAbsent
	}
	return notANoServerDiagnostic
}

// describeLiveTmuxServers and recreateSocketAdvice render the socket-absent
// refusal. They name the exact PIDs rather than a pattern because the obvious
// pattern does not work: tmux renames the server task to "tmux: server", and
// `pkill -x ... tmux` matches NOTHING against that — verified with pgrep, which
// shares pkill's matching (`pgrep -x -u $(id -u) tmux` returned nothing while
// `-x "tmux: server"` matched every server). A recovery hint that silently
// signals nothing is worse than none: the user runs it, believes they acted, and
// reset keeps refusing (Codex on #2956).
//
// Dropping -x is not the fix either. SIGUSR1's default disposition is TERMINATE,
// so a loose name match would kill any unrelated process whose name merely
// contains "tmux". Naming the PID is exact, portable, and cannot spray.
func describeLiveTmuxServers(pids []int) string {
	named := namedPIDs(pids)
	if named == "" {
		return "a tmux server is running for this user"
	}
	if len(pids) == 1 {
		return "a tmux server is running for this user (pid " + named + ")"
	}
	return "tmux server(s) are running for this user (pids " + named + ")"
}

func recreateSocketAdvice(pids []int) string {
	named := pidArgsForSignal(pids)
	if len(named) == 0 {
		return "Find the server (`ps -o pid,comm,args -u \"$(id -u)\"`, look for `tmux: server`), then " +
			"signal it with `kill -USR1 <pid>` to recreate its socket — or stop it — and re-run"
	}
	return "Recreate its socket with " + shellsuggest.Command("kill", append([]string{"-USR1"}, named...)...) +
		" — or stop that server — then re-run"
}

// isTmuxServerComm reports whether a kernel task name is a tmux SERVER.
//
// Measured on Linux: tmux retitles its processes, and the server and a client
// are "tmux: server" and "tmux: client" respectively. A HasPrefix(comm, "tmux")
// test therefore counts CLIENTS as servers — and a client exists whenever
// anyone is actually using tmux, so that mistake turns an ordinary ENOENT into
// "a server is running", refuses the reset, and points `kill -USR1` at a process
// that cannot recreate a server socket (Codex on #2956).
//
// The bare "tmux" fallback is for builds and platforms that do not retitle. It
// is EXACT, never a prefix, for the reason above: a prefix is what swallowed
// the client.
func isTmuxServerComm(comm string) bool {
	return comm == "tmux: server" || comm == "tmux"
}

// namedPIDs renders the PIDs we can actually name; the 0 sentinel means the
// process table could not be read, so there is no pid to print.
func namedPIDs(pids []int) string {
	return strings.Join(pidArgsForSignal(pids), ", ")
}

func pidArgsForSignal(pids []int) []string {
	var out []string
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		out = append(out, strconv.Itoa(pid))
	}
	return out
}

// tmuxDiagnosticSuffix renders CommandDiagnostic as a parenthesized clause for
// an error message, or "" when there is nothing to render.
func tmuxDiagnosticSuffix(err error) string {
	diagnostic := CommandDiagnostic(err)
	if diagnostic == "" {
		return ""
	}
	return " (" + diagnostic + ")"
}

// exactTarget builds an exact-match `-t` target spec for the named session.
//
// tmux resolves a bare `-t name` by exact match first and then PREFIX match, so
// once an agent session dies a bare target silently resolves to a surviving
// sibling — e.g. the shell tab `<name>__shell` of which `<name>` is an exact
// prefix. Capturing or sending to that sibling masks the dead agent and skips
// the liveness check (#1006).
//
// The leading `=` forces an exact session match. The trailing `:` is required
// for the pane-target commands (capture-pane, send-keys, set-option): without
// it tmux parses `=name` as a bare pane spec and reports "can't find pane:
// =name" even when the session exists. Appending the (empty) window component
// makes tmux parse `=name` as the session and resolve to its active pane. The
// session-target commands (kill-session, attach-session) accept the same form,
// so every action command shares one exact-match spelling.
func exactTarget(name string) string {
	return fmt.Sprintf("=%s:", name)
}

// CleanupSessions kills every af_-prefixed tmux session owned by THIS
// agent-factory home, ownership proven by the AF_HOME session-environment
// marker stamped at creation (#1120). Sessions carrying another home's marker
// (a second install, a test's sandbox home) and sessions with no marker at
// all (pre-marker builds, tmux <3.2) are skipped and logged: killing a
// session this home cannot prove it owns is worse than leaving it, and a
// test sweep that escapes onto the developer's real server must be a no-op
// (#1122). `af doctor` lists unowned af_ sessions with a manual kill command.
func CleanupSessions(cmdExec cmd.Executor) error {
	// First try to list sessions. Bounded by tmuxCommandTimeout (#2099): this is
	// the first tmux command of the sweep, and `af reset` runs the sweep
	// synchronously in a short-lived CLI process, so an unbounded stall here is a
	// user-visible hang with no way out but ^C.
	listCtx, listCancel := tmuxTimeoutContext()
	output, err := outputTmuxBoundedWith(listCtx, cmdExec, "ls")
	listTimedOut := listCtx.Err() != nil
	listCancel()

	// The listing has THREE outcomes and they must never collapse into two
	// (#2870). `af reset` calls this before it deletes worktrees, prunes
	// branches, and erases records, so "there are no sessions" is a licence to
	// destroy — and it may only be issued when tmux actually ANSWERED:
	//
	//   listed, and here they are  -> err == nil, fall through and sweep them.
	//   listed, and there are none -> a definitive no-server diagnostic (or an
	//                                 empty successful listing); return nil.
	//   could not list             -> abort. Fail CLOSED, with the underlying
	//                                 error: a warning here is not a safeguard,
	//                                 because by the time it is read the
	//                                 worktree is gone.
	//
	// This is the same rule the storage half of the reset already follows — an
	// unreadable instances.json is preserved rather than treated as an empty
	// repo (#868/#869) — applied to the one read that was still collapsing
	// "I could not tell" into "nothing is there".
	if err != nil {
		if listTimedOut {
			// A wedged server has told us NOTHING about what is running, so the
			// sweep must abort rather than proceed on an empty list — an empty
			// list here would silently read as "nothing to clean up" and report
			// success for a reset that swept nothing.
			return fmt.Errorf("%w: tmux ls after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if NoServerRunning(err) {
			return nil // tmux answered: no server on this socket, so no sessions
		}
		// Name what was unreadable AND what was therefore not done. tmux writes
		// its reason to stderr, which the bare error swallows — `exit status 1`
		// on its own gives the user nothing to act on.
		//
		// The socket-absent case gets its own sentence, because otherwise the
		// message reads like a contradiction: tmux said the socket is missing,
		// yet af refuses as if the state were unknown. It IS unknown, and the
		// reason is not guessable from the diagnostic (#2875).
		if diagnostic, exitOne := tmuxExitOneDiagnostic(err); exitOne &&
			classifyNoServerDiagnostic(diagnostic) == socketAbsent {
			return fmt.Errorf("could not list tmux sessions%s, but %s. A server whose socket is removed "+
				"(a /tmp cleaner will do it) keeps running with its sessions alive, so the missing socket "+
				"does not prove there are none. Refusing to sweep: no tmux session was killed. %s: %w",
				tmuxDiagnosticSuffix(err), describeLiveTmuxServers(tmuxServerProcessPIDs()),
				recreateSocketAdvice(tmuxServerProcessPIDs()), err)
		}
		return fmt.Errorf("could not list tmux sessions%s; refusing to sweep with the session set "+
			"unknown — no tmux session was killed: %w", tmuxDiagnosticSuffix(err), err)
	}

	// Anchor to start-of-line so `af_` embedded in a non-agent session name
	// (e.g. `my_af_project:`) is never matched and killed (#613).
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^%s[^:]*:`, regexp.QuoteMeta(TmuxPrefix)))
	prefixed := re.FindAllString(string(output), -1)
	for i, match := range prefixed {
		prefixed[i] = match[:strings.Index(match, ":")]
	}
	// Capture every listed session's verified pane process tree before marker
	// lookup. If the session vanishes during that lookup, tmux can no longer
	// recover its pane ancestry; this is the last authoritative opportunity to
	// bind any detached helper to the session. Capture errors are retained for
	// the vanished-session branch; a live owned session is captured again just
	// before kill, and a foreign session's unreadable panes never block this
	// home's sweep.
	preMarkerProcesses := make(map[string][]proctree.Process, len(prefixed))
	preMarkerCaptureErrs := make(map[string]error, len(prefixed))
	for _, match := range prefixed {
		preMarkerProcesses[match], preMarkerCaptureErrs[match] = captureSessionProcessTrees(cmdExec, match)
		if errors.Is(preMarkerCaptureErrs[match], ErrTmuxTimeout) {
			// The server is already known to be wedged. Do not launch the
			// ownership probe against it and pay another full timeout for an
			// answer that still could not authorize deletion.
			return fmt.Errorf("cannot capture tmux session %s processes before ownership lookup; refusing to continue cleanup: %w",
				match, preMarkerCaptureErrs[match])
		}
	}

	// Home-scope the sweep (#1122): the af_ prefix alone does not prove this
	// home owns the session — another install or an escaped test process can
	// see the same server. Only the AF_HOME marker match does.
	ownHome, err := afHomeDir()
	if err != nil {
		return fmt.Errorf("cannot resolve this agent-factory home; refusing to sweep tmux sessions: %w", err)
	}
	matches := make([]string, 0, len(prefixed))
	for _, match := range prefixed {
		home, ok, markerErr := sessionHomeMarker(cmdExec, match)
		if markerErr != nil {
			if errors.Is(markerErr, ErrTmuxTimeout) {
				// The same server is already known to be wedged. A fallback
				// probe would wait on it for another full timeout and still
				// could not turn the ownership result into affirmative state.
				return fmt.Errorf("cannot determine tmux session %s ownership; refusing to continue cleanup: %w", match, markerErr)
			}
			// The session may have disappeared after `tmux ls`. Re-probe the
			// exact name after every non-timeout marker error: only an authoritative
			// has-session absence turns this race into a successful cleanup. Session
			// absence is not process absence: a detached AF_SESSION-marked helper may
			// have survived after tmux lost the pane ancestry used by the normal
			// reaper, so the absent branch performs an ownership-scoped process sweep.
			exists, known, probeErr := probeSessionStrict(cmdExec, match)
			if known && !exists {
				if reapErr := reapVanishedSessionProcesses(match, ownHome, preMarkerProcesses[match], preMarkerCaptureErrs[match]); reapErr != nil {
					return fmt.Errorf("tmux session %s vanished during ownership lookup, but its process cleanup is incomplete: %w",
						match, reapErr)
				}
				log.InfoLog.Printf("tmux session %s vanished during ownership lookup; marked orphan process sweep completed", match)
				continue
			}
			if !known {
				return fmt.Errorf("cannot determine tmux session %s ownership or whether it survived; refusing to continue cleanup: %w",
					match, errors.Join(markerErr, probeErr))
			}
			return fmt.Errorf("cannot determine tmux session %s ownership; refusing to continue cleanup: %w", match, markerErr)
		}
		switch {
		case !ok:
			log.InfoLog.Printf("leaving tmux session %s: no AF_HOME ownership marker (pre-marker build or tmux <3.2); kill manually with: %s", match,
				shellsuggest.Command("tmux", "kill-session", "-t", "="+match))
		case filepath.Clean(home) != filepath.Clean(ownHome):
			log.InfoLog.Printf("leaving tmux session %s: owned by another agent-factory home (%s)", match, home)
		default:
			matches = append(matches, match)
		}
	}

	// Capture every session's pane process trees before any kill (#1104);
	// reap synchronously at the end because `af reset` is a short-lived CLI
	// process — a goroutine would die with it before the sweep ran.
	leakedBySession := make(map[string][]proctree.Process, len(matches))
	for _, match := range matches {
		leakedBySession[match] = SessionProcessTrees(cmdExec, match)
	}

	// Only sessions that are actually gone get their captured trees reaped —
	// a session that survives its kill still owns its processes.
	killed := make([]string, 0, len(matches))
	var killErr error
	for _, match := range matches {
		log.InfoLog.Printf("cleaning up session: %s", match)
		// `=` forces an exact session match so a name extracted from `tmux ls`
		// kills exactly that session and never a prefix-matching sibling (#1006).
		//
		// Bounded by tmuxCommandTimeout (#2099) so one undeletable session cannot
		// wedge the whole sweep.
		killCtx, killCancel := tmuxTimeoutContext()
		killErrRaw := runTmuxBoundedWith(killCtx, cmdExec, "kill-session", "-t", exactTarget(match))
		killTimedOut := killCtx.Err() != nil
		killCancel()
		if killErrRaw != nil {
			if killTimedOut {
				// Do NOT fall back to the sessionExists probe on a tripped
				// deadline: it spawns another tmux command against the same
				// wedged server and would hang identically, buying nothing (see
				// tmuxTimeoutContext). Report the wedge and stop.
				killErr = fmt.Errorf("%w: kill-session %s after %s", ErrTmuxTimeout, match, tmuxCommandTimeout)
				break
			}
			// Idempotent teardown (#967): a session can vanish between the
			// `tmux ls` above and this kill (TOCTOU). A gone session is the
			// goal of cleanup, but only an authoritative absence may turn the
			// kill error into success. A failed re-probe leaves teardown unknown
			// and must stop before process trees are reaped.
			exists, known, probeErr := probeSessionStrict(cmdExec, match)
			if !known {
				killErr = fmt.Errorf("failed to kill tmux session %s and cannot determine whether it survived: %w",
					match, errors.Join(killErrRaw, probeErr))
				break
			}
			if exists {
				killErr = fmt.Errorf("failed to kill tmux session %s: %v", match, killErrRaw)
				break
			}
		}
		killed = append(killed, match)
	}

	// Sweep concurrently: the grace waits overlap instead of serializing,
	// and the whole reset still blocks until every sweep finishes.
	var wg sync.WaitGroup
	for _, match := range killed {
		leaked := leakedBySession[match]
		if len(leaked) == 0 {
			continue
		}
		wg.Add(1)
		go func(match string, leaked []proctree.Process) {
			defer wg.Done()
			// These sessions were just killed BY this sweep, on request: their
			// processes are being destroyed with them, not caught escaping (#2765).
			reapSessionProcesses(reapOnRequest, match, leaked, reapGraceWait, reapTermWait)
		}(match, leaked)
	}
	wg.Wait()
	return killErr
}

func reapVanishedSessionProcesses(match, ownHome string, candidates []proctree.Process, captureErr error) error {
	var sweepErr error
	if captureErr != nil {
		sweepErr = fmt.Errorf("could not establish the complete pane process tree before ownership lookup: %w", captureErr)
	}
	// Two refresh/reap passes cover a helper that appears after the pre-marker
	// snapshot and a child it forks during the first bounded grace period. A
	// final non-destructive refresh below is the evidence that cleanup finished.
	for range 2 {
		refreshed, refreshErr := refreshOrphanCandidates(candidates, match)
		sweepErr = errors.Join(sweepErr, refreshErr)
		marked, inspectErr := markedOrphanProcesses(refreshed, match, ownHome)
		sweepErr = errors.Join(sweepErr, inspectErr)
		// The tmux session is GONE and these processes are still alive carrying its
		// ownership markers: they outlived the pane tree that was supposed to
		// contain them. This is the real leak, and the one worth a WARNING (#2765).
		remaining := reapSessionProcesses(reapEscaped, match, marked, reapGraceWait, reapTermWait)
		if len(remaining) > 0 {
			pids := make([]string, 0, len(remaining))
			for _, process := range remaining {
				pids = append(pids, fmt.Sprintf("%d", process.PID))
			}
			sweepErr = errors.Join(sweepErr, fmt.Errorf("marked processes %s are still alive after bounded teardown",
				strings.Join(pids, ", ")))
		}
		candidates = refreshed
	}
	finalCandidates, refreshErr := refreshOrphanCandidates(candidates, match)
	left, inspectErr := markedOrphanProcesses(finalCandidates, match, ownHome)
	sweepErr = errors.Join(sweepErr, refreshErr, inspectErr)
	if len(left) > 0 {
		pids := make([]string, 0, len(left))
		for _, process := range left {
			pids = append(pids, fmt.Sprintf("%d", process.PID))
		}
		sweepErr = errors.Join(sweepErr, fmt.Errorf("marked processes %s appeared or remained after the orphan sweep",
			strings.Join(pids, ", ")))
	}
	return sweepErr
}
