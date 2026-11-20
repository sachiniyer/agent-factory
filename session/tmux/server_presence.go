package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
)

// socketTmuxArgs prepends tmux's `-S <socket>` server option to a command's
// per-command args when socket is non-empty, pinning the command to that one
// tmux server. An empty socket returns args unchanged so the client resolves
// the server from its own environment ($TMUX, else $TMUX_TMPDIR, else the
// default) — the historical behavior callers that already operate on their
// own daemon's ambient socket keep.
//
// Pinned with `-S <path>` rather than named with `-L <name>` because the
// daemon's sessions live on a socket PATH it records, not a socket NAME a
// client re-resolves from TMUX_TMPDIR: a doctor process whose own
// $TMUX/$TMUX_TMPDIR resolves a different server would still reach the wrong
// one through `-L default`. This mirrors the `-S` pin app/config_agent.go
// and commands/accountslogincmd.go already use for this same daemon-vs-client
// divergence (#2019).
func socketTmuxArgs(socket string, args ...string) []string {
	if socket == "" {
		return args
	}
	return append([]string{"-S", socket}, args...)
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
// The corroboration listing is pinned to socket when non-empty, so a caller
// that already pinned its own command to a recorded server corroborates
// against that SAME server rather than re-resolving one from its ambient
// environment and asking the wrong server whether the session is absent.
//
// Only tmux's own exit 1 is widened this way. Any other status is a failure mode
// tmux documents no diagnostic for, and was never a candidate for absence.
func tmuxProvedSessionAbsent(cmdExec cmd.Executor, err error, name, socket string) bool {
	if missingTmuxSession(err, name) {
		return true
	}
	if _, exitOne := tmuxExitOneDiagnostic(err); !exitOne {
		return false
	}
	names, listErr := ListSessionNamesOnSocket(cmdExec, socket)
	if listErr != nil {
		return false
	}
	return !slices.Contains(names, name)
}

// sessionIDProvedAbsent is the one classifier for "this session id is gone" on
// every id-addressed command (#6258). tmux words a vanished-id failure per
// COMMAND — measured on the CI runner, kill-session says "can't find session:
// $N" while show-environment says "no such session: $N" — so no list of strings
// is complete. The diagnostic recognises the one wording the repo already knows
// (and a definitive no-server answer); for any other exit 1 the server's own
// id listing decides, because that read happened and is authoritative. A failure
// that is not an exit 1, or a listing that does not answer, or one that still
// contains the id, is UNKNOWN — never "gone", so never a licence to act.
func sessionIDProvedAbsent(cmdExec cmd.Executor, runErr error, id string) bool {
	if missingTmuxSession(runErr, id) {
		return true
	}
	if _, exitOne := tmuxExitOneDiagnostic(runErr); !exitOne {
		return false
	}
	ids, listErr := listSessionIDs(cmdExec)
	if listErr != nil {
		return false
	}
	return !slices.Contains(ids, id)
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
		// The kernel and SIGUSR1 make the disambiguation possible. Each live
		// same-uid server is asked which socket it is bound to, and only the
		// one bound to this path is sent SIGUSR1, tmux's own recreate-socket
		// signal, while the path is watched (#7533). A path that reappears had
		// a live owner and nothing is proved. One that stays absent, or that no
		// server is bound to, is unclaimed: a server that cannot or does not
		// recreate its socket is unreachable to every tmux client anyway,
		// including the kill-session a refusal would have suggested. Before
		// this, ANY live same-uid server — an unrelated `tmux -L` session —
		// made every missing-socket read answer "unknown" forever, which a
		// routine caller cannot retry past.
		return !tmuxSocketClaimed(absentTmuxSocketPath(diagnostic), tmuxServerProcessPIDs())
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

// tmuxServerSignal delivers the SIGUSR1 recreate-socket probe to a positively
// identified tmux server. It is a package var, not a bare syscall.Kill call, for
// the same reason proctree.Kill is one: a non-ESRCH signal failure such as EPERM
// or EACCES from a sandbox/LSM signal policy is a real state (cleanup.go and
// reap_test.go both call it out — "a D-state sleeper or a process this uid may
// not signal") that a test cannot conjure on demand against a same-uid live
// process. Tests override it to model a denied signal and exercise the
// kill-error branch of tmuxSocketClaimed deterministically, without installing
// a real seccomp filter or a process owned by another uid. It scopes only the
// ownership probe; the reaper delivers SIGTERM/SIGKILL through proctree.Signal,
// so overriding this var cannot change how a teardown signals anything.
var tmuxServerSignal = syscall.Kill

// tmuxServerIdentified is proctree.IsTmuxServer, the positive identity check
// every candidate passes before anything else is asked of it. A package var so
// the decision table can stand fake pids in for servers; nothing else sets it.
var tmuxServerIdentified = proctree.IsTmuxServer

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

// liveTmuxServerPIDs lists the tmux servers owned by this uid: the candidates
// for owning an unlinked socket.
//
// It answers a deliberately BROAD question — "could any live server own an
// unlinked socket?" — by task name, corroborated where the kernel truncated
// it by the one conclusive piece of non-truncated evidence (the title its
// own argv[0] would have produced). Which candidate is bound to THIS socket
// is tmuxSocketClaimed's question, answered from each server's own
// listening sockets (#7533). The asymmetry is chosen: a false "yes" here
// costs that per-server read; a false "no" lets reset delete worktrees out
// from under a live agent.
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
		if !isTmuxServerComm(p.Comm) && !isTruncatedTmuxServerTitle(p) {
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
	return listSessionField(cmdExec, "#{session_name}", "")
}

// ListSessionNamesOnSocket is ListSessionNames pinned to a specific tmux server
// socket via `tmux -S <socket> ls`, exported for `af doctor` (#2874/#2910).
// Doctor observes sessions a daemon in ANOTHER process created: the daemon and
// the doctor process can resolve DIFFERENT $TMUX/$TMUX_TMPDIR values, so a bare
// `tmux ls` lists whatever server doctor's ambient environment reaches, a live
// session on the daemon's socket is absent from the result, and every process
// carrying its marker reads as a verified orphan. Pinning to the socket the
// daemon records makes the listing name the server that actually hosts this
// home's sessions. An empty socket is the ambient listing of ListSessionNames,
// so callers without a recorded socket keep that behavior.
func ListSessionNamesOnSocket(cmdExec cmd.Executor, socket string) ([]string, error) {
	return listSessionField(cmdExec, "#{session_name}", socket)
}

// ServerPIDOnSocket returns the pid of the tmux server ANSWERING socket —
// which process actually serves the path, as opposed to which process a record
// or an environment stamp claims does. A socket path can outlive or be
// rebound by a different server than the one that created it: a tmux server
// orphaned from its socket file keeps running with its sessions alive, and a
// later server can bind the freed path, so a listing's "absent" answer is
// evidence only about the server that answered, never about the stamped one.
// `af doctor` compares this pid against the server pid a process's TMUX env
// stamps to learn whether the listing it got came from the server the process
// actually lived on (#6339).
//
// `display-message -p '#{pid}'` is the query because it answers on a server
// holding zero sessions (exit-empty off), where `ls` returns nothing at all.
//
// A socket with no server reports (0, nil): "nobody answered" is tmux's own
// definitive answer through NoServerRunning — the same determinate-empty
// classification the listing path relies on, including the SIGUSR1 probe that
// gives an orphaned owner the chance to reclaim an unlinked socket before it
// is called unclaimed. Any other failure is a read that did not happen, and
// the error says so.
func ServerPIDOnSocket(cmdExec cmd.Executor, socket string) (int, error) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, socketTmuxArgs(socket, "display-message", "-p", "#{pid}")...)
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		if timedOut {
			return 0, fmt.Errorf("%w: tmux display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		if NoServerRunning(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("could not query tmux server pid%s: %w", tmuxDiagnosticSuffix(err), err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if convErr != nil || pid <= 0 {
		return 0, fmt.Errorf("unparseable tmux server pid %q", strings.TrimSpace(string(out)))
	}
	return pid, nil
}

// listSessionIDs is the ListSessionNames contract for #{session_id}: the
// generation probe's corroboration asks which IDS the server still knows,
// and a name list cannot answer that.
func listSessionIDs(cmdExec cmd.Executor) ([]string, error) {
	return listSessionField(cmdExec, "#{session_id}", "")
}

func listSessionField(cmdExec cmd.Executor, format, socket string) ([]string, error) {
	ctx, cancel := tmuxTimeoutContext()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, socketTmuxArgs(socket, "ls", "-F", format)...)
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

// describeSocketOwners and recreateSocketAdvice render the socket-absent
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
//
// For the same reason `kill -USR1` is offered only for servers, the pids
// positively identified as tmux servers (#7541). A candidate that is not, such
// as a darwin tmux client, whose task name the server prefilter cannot tell
// from a server's, is named with a check to run first and no signal: SIGUSR1
// kills a client. The 0 sentinel for an unreadable process table is never
// named.
func describeSocketOwners(servers, unidentified []int) string {
	named, unknown := pidArgsForSignal(servers), pidArgsForSignal(unidentified)
	var head string
	switch {
	case len(named) == 1:
		head = "a tmux server is running for this user (pid " + named[0] + ")"
	case len(named) > 1:
		head = "tmux servers are running for this user (pids " + strings.Join(named, ", ") + ")"
	case len(unknown) == 0:
		return "a tmux server is running for this user"
	default:
		head = "a tmux server may be running for this user"
	}
	switch len(unknown) {
	case 0:
		return head
	case 1:
		return head + "; pid " + unknown[0] + " could not be identified as a tmux server or ruled out"
	default:
		return head + "; pids " + strings.Join(unknown, ", ") + " could not be identified as tmux servers or ruled out"
	}
}

func recreateSocketAdvice(servers, unidentified []int) string {
	named, unknown := pidArgsForSignal(servers), pidArgsForSignal(unidentified)
	var advice string
	if len(named) == 0 {
		advice = "Find the server (`ps -o pid,comm,args -u \"$(id -u)\"`, look for `tmux: server`), then " +
			"signal it with `kill -USR1 <pid>` to recreate its socket — or stop it — and re-run"
	} else {
		advice = "Recreate its socket with " + shellsuggest.Command("kill", append([]string{"-USR1"}, named...)...) +
			" — or stop that server — then re-run"
	}
	if len(unknown) > 0 {
		advice += ". Check " + shellsuggest.Command("ps", "-o", "pid,comm,args", "-p", strings.Join(unknown, ",")) +
			" before signalling " + pidNoun(unknown) + ": SIGUSR1 terminates a process that is not a tmux server, a tmux client included"
	}
	return advice
}

// deletedSocketDirAdvice renders the deleted-directory clause the
// socket-absent refusal appends (#7647). The uncertainty is discharged by
// recreating the missing chain — but only after a check that makes that
// SAFE, because the deleted component may have been a link to where one of
// those candidates is bound, #7562's own shape: recreated as a plain
// directory, the spelling resolves to an absent basename, reads as
// certain, and rules af's own server out while its sessions still run. So
// each candidate is listed first, through the socket it is actually bound
// to — the kernel spelling the bound-path read already names — and
// compared with af's session names (they begin af_). A candidate holding
// them IS af's server, reached through the deleted link: stop it, or
// restore the link. Only when none does is `mkdir -p <dir>` safe, and it
// makes the socket spelling comparable again.
func deletedSocketDirAdvice(dir string, servers, unidentified []int) string {
	var checks []string
	unnamed := false
	for _, pid := range append(append([]int{}, servers...), unidentified...) {
		if pid <= 0 {
			continue // the unreadable-table sentinel names nothing to check
		}
		if paths, err := listenerPathsOf(pid); err == nil && len(paths) > 0 {
			for _, p := range paths {
				checks = append(checks, "pid "+strconv.Itoa(pid)+": "+
					shellsuggest.Command("tmux", "-S", p, "list-sessions"))
			}
			continue
		}
		checks = append(checks, "pid "+strconv.Itoa(pid)+
			": tmux -S <its bound socket> list-sessions")
		unnamed = true
	}
	check := "tmux -S <its bound socket> list-sessions for each of them"
	if len(checks) > 0 {
		check = strings.Join(checks, "; ")
		if unnamed {
			check += " — where no socket is named, the -S or -L on its command line names it"
		}
	}
	return "The socket's directory was deleted too, and a deleted component may have been a link " +
		"to where one of those candidates is bound — recreating it as a plain directory would make " +
		"the spelling resolve and wrongly rule that server out. First check what each holds: " +
		check + ", and compare with af's session names (they begin `af_`). A candidate holding af's " +
		"sessions IS af's server, reached through the deleted link — stop that server, or restore " +
		"the link it was reached through, then re-run. If none does, recreating the directory is " +
		"safe: " + shellsuggest.Command("mkdir", "-p", dir) +
		" makes the socket spelling comparable again"
}

// pidNoun is "pid N" or "pids N, M".
func pidNoun(pids []string) string {
	if len(pids) == 1 {
		return "pid " + pids[0]
	}
	return "pids " + strings.Join(pids, ", ")
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
// The `: server` suffix, not the exact "tmux: server" string, admits a server
// tmux started under a RENAMED argv[0]. On Linux tmux builds its task title
// from getprogname(), which is argv[0], so `exec -a foo tmux` and a copy or
// symlink named otherwise are titled `foo: server` (measured on tmux 3.4,
// #7242, and documented in internal/proctree/tmuxidentity.go). An exact
// "tmux: server" test dropped such a server at the entry point: its owner
// never became a candidate, every enumerated one was ruled out, and the
// socket-absent probe answered a determinate "unclaimed" (NoServerRunning
// returned true) while the server and its panes were alive — so `af reset`'s
// destructive cleanup proceeded against a still-running server, the exact
// destruction class the socket-absent probe exists to prevent (#2875). The
// suffix never matches a client: clients are "<name>: client". A non-tmux
// process that happens to title itself "x: server" costs one per-server read
// and then, because proctree.IsTmuxServer fails it, is remembered as an
// unprobed possible owner — degrading the answer to "claimed" (unknown) and
// REFUSING destructive cleanup, the prefilter's own declared safe direction.
//
// The bare "tmux" fallback is for builds and platforms that do not retitle. It
// is EXACT, never a prefix, for the reason above: a prefix is what swallowed
// the client.
//
// That fallback also admits every CLIENT on darwin, where tmux cannot retitle
// and every tmux process is named "tmux" (measured, #4678). This is only the
// coarse prefilter for "could a server be alive"; proctree.IsTmuxServer decides
// who is actually signalled.
//
// comm alone cannot carry every title: the kernel truncates it at 15 bytes,
// so a renamed server whose argv[0] basename is longer than seven characters
// loses the suffix this predicate needs. Those are isTruncatedTmuxServerTitle's
// question, asked alongside this one in liveTmuxServerPIDs — never here, since
// the corroboration reads the process, not the name.
func isTmuxServerComm(comm string) bool {
	return comm == "tmux" || strings.HasSuffix(comm, ": server")
}

// linuxTaskCommLen is the kernel's task-name ceiling on Linux:
// TASK_COMM_LEN is 16 including the trailing NUL, so prctl(PR_SET_NAME)
// keeps 15 bytes and silently drops the rest.
const linuxTaskCommLen = 15

// isTruncatedTmuxServerTitle reports whether a task name at the kernel's
// truncation ceiling is evidence of a tmux server retitled under a renamed
// argv[0]. The `: server` suffix survives truncation only while argv[0]'s
// basename is seven characters or fewer; past that the kernel keeps a
// shrinking piece of it — `mediumtmux: ser` — and at fifteen or more it is
// cut entirely, leaving `verylongtmuxna` — so the suffix predicate can
// never see those titles (Codex on #7689).
//
// The corroborating evidence is the process's own command line, which the
// kernel does NOT truncate: the pid is a candidate exactly when its comm
// equals the truncation of "<basename(argv[0])>: server". For a basename of
// eight to fourteen that equality is discriminating — an ordinary process's
// comm is only its own name, which is shorter than the ceiling. At fifteen
// or more it degenerates — the title is cut entirely inside the basename,
// so the check matches every same-named process — and that is accepted,
// deliberately. A candidate is rejected only on conclusive evidence that it
// is not a retitled server: a comm that is not this argv[0]'s truncated
// title is one (a client's "<name>: client" can never equal it, and a client
// with a long name truncates to the same comm, staying a candidate — the
// fail-safe answer). Teaching the filter tmux's argv grammar instead never
// converges: every separator spelling, quoting, alias, and option ordering a
// client accepts is one its server then keeps verbatim. What is merely
// admitted and cannot be identified degrades the answer to "claimed"
// (unknown), refusing destructive cleanup — the prefilter's own declared
// safe direction — while proctree.IsTmuxServer keeps deciding who is
// signalled.
//
// Only Linux retitles the task name — darwin's comm stays the exec'd file
// name, where this comparison would admit every file named past its
// ceiling. Kernel threads are skipped outright: a kernel thread has no
// userspace argv to corroborate, and its comm is never a tmux title. An
// argv that cannot be read admits — unreadable is not evidence of absence.
func isTruncatedTmuxServerTitle(p proctree.Process) bool {
	if runtime.GOOS != "linux" || len(p.Comm) < linuxTaskCommLen || p.KernelThread {
		return false
	}
	argv := proctree.Argv(p.PID)
	if len(argv) == 0 {
		return true
	}
	title := filepath.Base(argv[0]) + ": server"
	if len(title) > linuxTaskCommLen {
		title = title[:linuxTaskCommLen]
	}
	return p.Comm == title
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
// its socket to a /tmp cleaner and keeps running orphaned" (#2875).
//
// The kernel can tell them apart. A listening socket keeps the address it was
// bound to after its file is unlinked, so each candidate server is asked which
// socket it is bound to (tmuxServerSocketBinding), and only a server bound to
// THIS path is sent SIGUSR1, tmux's documented recreate-socket signal (#7533).
// Before that, every live same-uid tmux server was signalled. Each one briefly
// unlinked and rebound its own socket, and a client of an unrelated server that
// connected in that gap failed (measured: 57 of 2,000 capture-pane calls under
// a ~2 ms signal storm). Per candidate:
//
//   - not positively a tmux server (proctree.IsTmuxServer), or the 0 sentinel
//     for a table that could not be read: never signalled. SIGUSR1's default
//     disposition is TERMINATE, which kills a tmux CLIENT before it installs
//     its handler (#4678) and a server's child before it execs (#6581). It
//     remains a possible owner.
//   - bound to another path: ruled out, never signalled.
//   - exited while its sockets were read, as an independent lookup of the pid
//     confirms: disproved like a pid that vanishes before its signal, never
//     signalled (#7541).
//   - bound to this path: the owner. It is signalled, and the path is watched.
//     Where its sockets cannot be read one by one, a match in the socket
//     table also counts, as positive evidence only (#7536).
//   - unresolved (its sockets could not be read, it holds no listener, or the
//     path is relative): never signalled, and it remains a possible owner.
//
// Each SIGUSR1 is logged at INFO with its target, the path the target is bound
// to, and the code that asked, and so is each rule-out, with the paths the
// server is bound to instead (#7541). An "unclaimed" answer resting on a
// rule-out can then be traced the way a signal can.
//
// An owner that recreates the path claims it. Otherwise the answer is
// "unclaimed", a determinate empty, only when no possible owner remains. That
// means every candidate was ruled out, or the signalled owners left the path
// absent: a server that cannot or does not recreate its socket is unreachable to
// every tmux client anyway. A possible owner, or an owner whose signal was
// refused, keeps the answer "claimed" (unknown), because it was never
// disproved. With nothing signalled the answer comes at once, with no watch.
func tmuxSocketClaimed(socketPath string, pids []int) bool {
	signaled := false
	unprobedOwnerPossible := false
	for _, pid := range pids {
		if pid <= 0 {
			// The process table could not be read: an owner was not disproved.
			unprobedOwnerPossible = true
			continue
		}
		if !tmuxServerIdentified(pid) {
			// The pid passed the liveTmuxServerPIDs comm prefilter but
			// could not be positively identified as a server (proctree.
			// IsTmuxServer is strict: an unreadable command line, a
			// darwin foreground `tmux -D`, or a same-uid client all fail).
			// Skip signalling it — SIGUSR1's default disposition is
			// TERMINATE — but remember it: it occupies a slot the prefilter
			// admitted, and a skipped owner is never disproved, so a
			// socket that stays absent is NOT evidence against it.
			unprobedOwnerPossible = true
			continue
		}
		binding, boundTo := tmuxServerSocketBinding(pid, socketPath)
		switch binding {
		case socketBindingElsewhere:
			log.InfoLog.Printf("tmux socket %s is missing: tmux server pid %d is bound to %s instead, per its own "+
				"sockets read one by one, so it is ruled out and not signalled (asked by %s)", socketPath, pid, boundTo, socketProbeCaller())
			continue
		case socketBindingGone:
			log.InfoLog.Printf("tmux socket %s is missing: tmux server pid %d exited while its sockets were "+
				"read, so it is ruled out and not signalled (asked by %s)", socketPath, pid, socketProbeCaller())
			continue
		case socketBindingUnresolved:
			unprobedOwnerPossible = true
			continue
		}
		kerr := tmuxServerSignal(pid, syscall.SIGUSR1)
		outcome := "sent"
		if kerr != nil {
			outcome = "not delivered: " + kerr.Error()
		}
		source := "its own socket, read by sock_diag/proc_pidfdinfo"
		if binding == socketBindingBoundByTable {
			source = "the /proc/<pid>/net/unix table, since its own socket could not be read"
		}
		log.InfoLog.Printf("tmux socket %s is missing: SIGUSR1 to tmux server pid %d, which is bound to %s per %s, "+
			"so it recreates the socket (%s; asked by %s)", socketPath, pid, boundTo, source, outcome, socketProbeCaller())
		switch {
		case kerr == nil:
			signaled = true
		case errors.Is(kerr, syscall.ESRCH):
			// The pid vanished between the positive identity check and the
			// signal. Gone is gone: this candidate IS disproved, so it is
			// neither counted as signalled nor remembered as a possible owner.
		default:
			// A positively-identified live server we could not signal — e.g.
			// EPERM or EACCES from a sandbox/LSM signal policy, the denied-signal
			// state cleanup.go and reap_test.go acknowledge as real. A live
			// owner that kill() refuses is NOT disproved, and a socket that
			// stays absent is NOT evidence against it: the safe direction is
			// "claimed" (unknown), exactly the branch the IsTmuxServer skip
			// above takes. The doc comment's contract — "a table with live
			// servers that could not all be signaled answers claimed" — already
			// says so; before this arm existed a mixed table (one kill
			// succeeding, one failing) answered "unclaimed", manufacturing a
			// determinate empty for a live owner that was never disproved and
			// licensing CleanupSessions' destructive path against it.
			unprobedOwnerPossible = true
		}
	}
	if !signaled {
		if unprobedOwnerPossible {
			return true
		}
		// Every candidate was ruled out. A server that bound the path since the
		// client's read would show as the file itself.
		_, err := os.Stat(socketPath)
		return err == nil
	}
	deadline := time.Now().Add(socketReclaimWindow)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			// A prefilter-passing pid was skipped as not positively a
			// server. It could be the live owner of this unlinked socket
			// (a darwin `tmux -D` foreground server, a sidUnknown daemon,
			// or a client the comm prefilter admitted). Socket absence
			// after signalling the *other* servers does not disprove it,
			// so the safe direction — the one the doc comment and the
			// module's "a failed read is not an empty result" invariant
			// both demand — is "claimed" (unknown), refusing the
			// destructive cleanup that a false "unclaimed" would license.
			return unprobedOwnerPossible
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// socketReclaimWindow is how long tmuxSocketClaimed watches for a signalled
// owner to recreate its socket. A package var so a test can widen it and prove
// that a probe with no owner to signal never waits on it.
var socketReclaimWindow = 500 * time.Millisecond

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
