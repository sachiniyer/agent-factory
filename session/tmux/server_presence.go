package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// missingTmuxSession recognizes tmux's explicit exact-target absence answer.
// Exit status 1 alone is ambiguous: wrapper, policy, and unclassified connection
// failures can return the same status while the named session remains unknown.
// A definitive no-server answer counts too: no session can remain on a socket
// that holds no server — routed through NoServerRunning so the has-session
// re-probes get the same ENOENT treatment as the listing. The finding on #2875
// named this path explicitly: `tmux ls` may find AF sessions, the socket may then
// be removed by a /tmp cleaner, and this fallback probe would answer ENOENT while
// the server and its panes are still running.
//
// It reads the DIAGNOSTIC only, and is therefore incomplete on its own: see
// tmuxProvedSessionAbsent, which every caller that acts on absence goes through.
func missingTmuxSession(err error, name string) bool {
	diagnostic, ok := tmuxExitOneDiagnostic(err)
	if !ok {
		return false
	}
	return diagnostic == "can't find session: "+name || NoServerRunning(err)
}

// tmuxProvedSessionAbsent reports whether tmux has PROVED the named session is
// not on its server, for a command that ran and answered with a failure.
//
// missingTmuxSession decides that from the diagnostic alone, which is enough for
// the answers tmux gives while it still has a session to make "current". It is
// NOT enough when the server holds NO sessions at all: cmd_find_target then
// fails to establish a current target before it ever looks at `-t`, so
// has-session and list-panes answer `no current target` — exit 1, naming neither
// the session nor a socket, and matching none of the strings above. Measured on
// tmux 3.4 (#3469).
//
// That state is not exotic. A server passes through it on the way out after its
// last session is killed, which is what made the reap suite's refusal family
// flaky: the fixture kills the only session on a private server, and whether the
// following has-session lands before the server exits (`no current target`,
// unclassified) or after it (`no server running on <socket>`, classified)
// decided which refusal reason CleanupSessions produced. Under `set -s
// exit-empty off` it is not a window at all but the server's PERMANENT resting
// state, so on those users' boxes `af reset` refused cleanup of a session that
// was genuinely gone, every time.
//
// The fix is not a third string to be right about. That would still leave
// `server exited unexpectedly` (also measured, racing the same shutdown) and
// every future wording unclassified, which is how the mechanism survives a
// spelling fix. Instead an unrecognized exit 1 is corroborated against tmux's
// own session listing: ListSessionNames succeeds only on an AUTHORITATIVE
// answer — exit 0, or a definitive no-server — so a name absent from what it
// returns is absent from the server, whatever the failed command said. A listing
// that does not answer leaves the session UNKNOWN, exactly as before. This keeps
// the package invariant intact rather than bending it: absence is still proved
// by a read that happened, never inferred from one that failed.
//
// Only tmux's own exit 1 is widened this way. Any other status is a failure mode
// tmux documents no diagnostic for, and was never a candidate for absence.
func tmuxProvedSessionAbsent(cmdExec cmd.Executor, err error, name string) bool {
	if missingTmuxSession(err, name) {
		return true
	}
	if _, exitOne := tmuxExitOneDiagnostic(err); !exitOne {
		return false
	}
	names, listErr := ListSessionNames(cmdExec)
	if listErr != nil {
		return false
	}
	return !slices.Contains(names, name)
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
		// SIGUSR1 is what makes the disambiguation possible: it is tmux's own
		// recreate-socket signal, so every live same-uid server is asked to
		// reclaim its socket and the path is watched. A path that reappears
		// had a live owner and nothing is proved; one that stays absent is
		// unclaimed — a server that cannot or does not recreate its socket is
		// unreachable to every tmux client anyway, including the kill-session
		// a refusal would have suggested. Before this, ANY live same-uid
		// server — an unrelated `tmux -L` session — made every missing-socket
		// read answer "unknown" forever, which a routine caller cannot retry
		// past.
		socket := absentTmuxSocketPath(diagnostic)
		return !tmuxSocketClaimed(socket, tmuxServerProcessPIDs(socket))
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

// tmuxServerProcessPIDs answers which same-uid tmux servers could own a
// socket path — see liveTmuxServerPIDs. A package var so tests can pin the
// answer: the real probe reads the host's process table, so a developer box
// with tmux running would otherwise give a different result from a container
// that has none.
var tmuxServerProcessPIDs = liveTmuxServerPIDs

// PinServerProbeForTest fixes the "which servers could own this socket?"
// answer and returns the restore. It exists for internal/testguard.IsolateTmux,
// which declares a private-socket world for a test; without it that isolation
// is incomplete, because the probe reads the HOST process table and would see
// the developer's own tmux servers inside a world that is supposed to have
// none.
//
// Test-only by contract, not by build tag: testguard lives in internal/ and is
// the only caller.
func PinServerProbeForTest(pids ...int) (restore func()) {
	prev := tmuxServerProcessPIDs
	tmuxServerProcessPIDs = func(string) []int { return pids }
	return func() { tmuxServerProcessPIDs = prev }
}

// liveTmuxServerPIDs answers "which server owns socketPath?" with the set of
// same-uid pids that could: the socket's positively-attested owners when the
// kernel names them (attestedSocketOwnerPIDs), else every process that could
// own a socket at all — a tmux SERVER. A tmux CLIENT is the one tmux-named
// process this set excludes: it can never own or recreate a socket, and
// SIGUSR1's default disposition is terminate (#4349).
//
// "Could own" is decided by argv, not by the task name alone: on darwin
// p_comm is "tmux" for server and client alike — setproctitle rewrites argv
// (kern.procargs2) but never p_comm — so a comm test cannot separate them
// there. Only a POSITIVE client sighting drops a pid out: an unreadable argv
// or an unretitled "tmux" stays, because dropping a real server turns an
// unknown into a confident unclaimed — and af would sweep under a live one.
//
// It still answers a deliberately BROAD question when ownership cannot be
// read back — "could any live server own an unlinked socket?" — rather than
// "is the server for THIS socket alive?". The asymmetry is chosen: a false
// "yes" costs a refused sweep the user can retry after checking; a false "no"
// lets reset delete worktrees out from under a live agent. Over-refusal is
// also rare in practice, because a server on the DEFAULT socket has a socket
// file, so it answers ECONNREFUSED rather than ENOENT.
//
// A process table we cannot read reports TRUE, for the same reason: an
// unreadable table is not evidence of absence.
func liveTmuxServerPIDs(socketPath string) []int {
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
		// comm is "tmux: server" or "tmux". "tmux" is the ambiguous arm —
		// darwin's p_comm is "tmux" for the server AND every client — so argv
		// gets the last word there. A positive "tmux: client" drops out;
		// anything unreadable or unretitled stays, because dropping a real
		// server turns an unknown into a confident unclaimed.
		if p.Comm == "tmux" && tmuxArgvNamesClient(proctree.Argv(pid)) {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	if owners := attestedSocketOwnerPIDs(socketPath, pids); owners != nil {
		return owners
	}
	return pids
}

// The vocabulary of a tmux retitle, and the reason it is matched as a title
// rather than as a path.
//
// tmux writes its process title from exactly ONE place: proc_start() in
// proc.c, which calls setproctitle("%s (%s)", name, socket_path) with name
// "server" (server.c) or "client" (client.c), and the platform's setproctitle
// prepending the program name. Two shapes come out of that one line, and
// nothing else does:
//
//   - "tmux: <role> (<socket path>)" — the full retitle, wherever
//     setproctitle rewrites argv (darwin's kern.procargs2, the BSDs).
//   - "tmux: <role>" — the same title truncated. tmux's own compat
//     setproctitle formats into a 16-byte buffer and, on overflow, cuts back
//     to the LAST SPACE, which is why Linux's p_comm reads "tmux: server"
//     rather than "tmux: server (/".
//
// The suffix is a PATH, and that is precisely what filepath.Base cannot
// survive on a title: Base("tmux: client (/private/tmp/tmux-501/default)") is
// "default)". A normally retitled client therefore failed to match, stayed in
// the fallback PID set, and was signalled — #4349 reintroduced through an
// incomplete match (#4432). A retitled process title is not a path; only an
// UNretitled argv[0] is one, and Base belongs there and nowhere else.
const (
	tmuxTitlePrefix = "tmux: "
	tmuxRoleServer  = "server"
	tmuxRoleClient  = "client"
)

// tmuxTitleNamesRole reports whether title is tmux's retitle for role.
//
// The role must END — at the end of the title, or at the " (" that opens the
// socket path — so both shapes above match while "tmux: clientfoo" does not.
// A longer word is a different process, not a suffixed client: widening the
// match to a bare prefix would let anything that merely starts "tmux: client"
// drop a pid out of the set, which is the one direction this file must never
// guess in.
func tmuxTitleNamesRole(title, role string) bool {
	rest, ok := strings.CutPrefix(title, tmuxTitlePrefix)
	if !ok {
		return false
	}
	return rest == role || strings.HasPrefix(rest, role+" (")
}

// tmuxArgvNamesClient reports whether argv positively names a tmux CLIENT —
// the retitle setproctitle leaves in argv[0] and darwin's p_comm hides. Only a
// positive sighting counts: an argv that is unreadable, unretitled, or
// retitled into a shape we cannot resolve to a role reports FALSE and the pid
// stays in the set. Widening the match must never cost that direction — a pid
// dropped on a guess turns an unknown into a confident "unclaimed", and af
// would sweep under a live server.
func tmuxArgvNamesClient(argv []string) bool {
	return len(argv) > 0 && tmuxTitleNamesRole(argv[0], tmuxRoleClient)
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
	return listSessionField(cmdExec, "#{session_name}")
}

// listSessionIDs is the ListSessionNames contract for #{session_id}: the
// generation probe's corroboration asks which IDS the server still knows,
// and a name list cannot answer that.
func listSessionIDs(cmdExec cmd.Executor) ([]string, error) {
	return listSessionField(cmdExec, "#{session_id}")
}

func listSessionField(cmdExec cmd.Executor, format string) ([]string, error) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, "ls", "-F", format)
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

// isTmuxServerComm reports whether a kernel task name could be a tmux
// SERVER. It is the cheap first filter of liveTmuxServerPIDs, not the
// verdict: the "tmux" arm covers server and client alike wherever the
// retitle lives in argv but never reaches p_comm (darwin, #4349), and that
// arm is resolved against argv itself before a pid is kept.
//
// Measured on Linux: tmux retitles its processes, and the server and a client
// are "tmux: server" and "tmux: client" respectively. A HasPrefix(comm, "tmux")
// test therefore counts CLIENTS as servers — and a client exists whenever
// anyone is actually using tmux, so that mistake turns an ordinary ENOENT into
// "a server is running", refuses the reset, and points `kill -USR1` at a process
// that cannot recreate a server socket (Codex on #2956).
//
// The bare "tmux" fallback is for builds and platforms that do not retitle —
// and for the ones whose retitle does not reach p_comm. It is EXACT, never a
// prefix, for the reason above: a prefix is what swallowed the client.
func isTmuxServerComm(comm string) bool {
	return comm == "tmux: server" || comm == "tmux"
}

// absentTmuxSocketPath extracts the socket path from tmux's ENOENT diagnostic
// ("error connecting to <socket> (No such file or directory)"), or "" when the
// diagnostic is not that one.
func absentTmuxSocketPath(diagnostic string) string {
	rest, ok := strings.CutPrefix(diagnostic, "error connecting to ")
	if !ok {
		return ""
	}
	socket, ok := strings.CutSuffix(rest, " (No such file or directory)")
	if !ok {
		return ""
	}
	return strings.TrimSpace(socket)
}

// tmuxSocketClaimed reports whether a live tmux server claims the socket at
// socketPath. It is the active half of the socket-absent check: ENOENT alone
// cannot distinguish "no server ever created this socket" from "a server lost
// its socket to a /tmp cleaner and keeps running orphaned" (#2875), so every
// live same-uid tmux server is sent SIGUSR1 — tmux's documented
// recreate-socket signal — and the path is watched. An orphaned owner
// recreates the path; the socket staying absent afterward means no live
// server was bound to it, so a failed read against it is a determinate empty.
//
// pids comes from tmuxServerProcessPIDs(socketPath) — the socket's attested
// owners where the kernel names them, else every same-uid process that could
// own a socket. A pid is re-verified as a tmux server at signal time
// (isLiveTmuxServer, from its own argv — NOT proctree.IsTmuxServer, whose
// "tmux:" prefix counts clients) so a pid reused since the snapshot cannot
// spray SIGUSR1 — whose default disposition is TERMINATE — into an unrelated
// process, and a client that reached the list is refused a second time
// (#4349).
//
// Ambiguity survives in one direction only: a table with live servers that
// could not all be signaled answers "claimed" (unknown), because a live owner
// was not disproved. An empty table answers "unclaimed" outright.
func tmuxSocketClaimed(socketPath string, pids []int) bool {
	signaled := false
	for _, pid := range pids {
		if pid <= 0 || !isLiveTmuxServer(pid) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGUSR1); err == nil {
			signaled = true
		}
	}
	if !signaled {
		return len(pids) != 0
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// isLiveTmuxServer re-verifies at signal time that pid names a tmux SERVER,
// from its own argv — the check proctree.IsTmuxServer cannot do, because its
// "tmux:" prefix match counts a client as a server, and a client must never
// be signalled (#4349). A positively named server — "tmux: server" or "tmux:
// server (<socket path>)" — or an unretitled tmux binary passes; a client, a
// retitle that names no role we recognise, and an argv that has gone
// unreadable do not, because a signal needs a CONFIRMED server, not a
// plausible one.
//
// The two arms read argv[0] differently on purpose, and #4432 is what a single
// filepath.Base over both of them costs. A RETITLED argv[0] is a title, so it
// is matched whole; Base was wrong on it in BOTH directions:
//
//   - it drops a real suffixed server OUT of the gate, which merely withholds
//     a signal — tmuxSocketClaimed then reports the socket claimed, the
//     over-refusal this file already chooses on purpose; and
//   - it lifts a CLIENT into the gate whenever the title is truncated so its
//     tail reads like a path component "tmux" — Base("tmux: client
//     (/private/tmp/tmux") is "tmux". That is the severe direction: SIGUSR1 at
//     a client, whose default disposition for it is terminate.
//
// An UNRETITLED argv[0] genuinely is the executed path ("/opt/homebrew/bin/
// tmux"), so Base is correct there — and only there.
func isLiveTmuxServer(pid int) bool {
	argv := proctree.Argv(pid)
	if len(argv) == 0 {
		return false
	}
	if strings.HasPrefix(argv[0], tmuxTitlePrefix) {
		return tmuxTitleNamesRole(argv[0], tmuxRoleServer)
	}
	return filepath.Base(argv[0]) == "tmux"
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
