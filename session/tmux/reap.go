package tmux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/teardownreq"
	"github.com/sachiniyer/agent-factory/log"
)

// Process-tree reaping (#1104). `tmux kill-session` only SIGHUPs the pane's
// foreground process group; anything that detached from it (a backgrounded
// `yes`, an agent-spawned helper in its own process group) survives teardown
// as an orphan and can burn a core forever — eight of those starved the tmux
// server on the dev box. So every teardown path captures the pane's full
// descendant tree BEFORE kill-session (afterwards orphans reparent to init
// and the ancestry is unrecoverable) and, after a grace period for SIGHUP
// handlers, SIGTERMs then SIGKILLs whatever is still alive.
//
// Safety properties:
//   - Only processes captured from the session's own panes are ever
//     signalled: a pane PID is trusted only if it is a live child of a tmux
//     server, and the capture set is its ppid-descendants plus processes
//     sharing its kernel session id (tmux makes each pane root a session
//     leader, so SID membership proves pane ancestry even for processes
//     already reparented to init).
//   - Every signal re-verifies the (pid, starttime) identity via
//     proctree.Signal, so a recycled PID is never signalled.
//   - If kill-session fails and the tmux session survives, nothing is
//     reaped — a live session's processes are not leaks.
//
// Best-effort by design: capture failures (no /proc, session already gone,
// mock executors in tests) degrade to a no-op, and reap outcomes are logged,
// never returned as errors.

var (
	// reapGraceWait is how long a captured process gets to exit on its own
	// after kill-session (SIGHUP) before being SIGTERMed. "Captured", not
	// "leaked": most of them are a requested teardown's own pane tree going down
	// with the session, and only the vanished-session sweep is looking at
	// processes that escaped one (#2765). Matches
	// paneExitWait's reasoning: long enough for an agent to flush state,
	// short enough to bound the sweep. var, not const, so tests can lower it.
	reapGraceWait = 3 * time.Second
	// reapTermWait is how long a SIGTERMed process gets before SIGKILL.
	reapTermWait = 2 * time.Second
)

// SessionProcessTrees enumerates every live process belonging to the named
// tmux session's panes: each pane root (verified to be a live child of a tmux
// server), its ppid-descendants, and its kernel-session members. The
// teardown paths call it BEFORE kill-session; `af doctor` uses it to map a
// live session's legitimate processes. This public diagnostic form is strictly
// best-effort: a command/snapshot failure returns nil, while malformed individual
// pane rows are omitted and any independently verified panes are still returned.
// Report-bearing callers that must not render a failed read as an empty result
// (or as a concrete finding) use CaptureSessionProcessTrees below so they also
// receive the completeness error and can refuse to classify on a nil that only
// means "could not be read".
//
// The list-panes probe is bounded by tmuxCommandTimeout (#1917): it runs first
// on the kill teardown, so an unbounded stall here wedges the kill before
// kill-session is even attempted. A tripped deadline degrades to the existing
// nil (best-effort) result — nothing is reaped, which is the safe direction: a
// wedged server has told us nothing about which processes are actually leaked.
func SessionProcessTrees(cmdExec cmd.Executor, sanitizedName string) []proctree.Process {
	procs, _ := CaptureSessionProcessTrees(cmdExec, sanitizedName)
	return procs
}

// CaptureSessionProcessTrees is the evidence-bearing half of
// SessionProcessTrees. Ordinary user-driven teardown remains best-effort and uses
// any partial result via SessionProcessTrees above, but a caller about to delete
// or move the worktree — or to report on pane membership — also needs to know
// whether the capture itself was complete. Returning that answer separately
// prevents "no descendants" from being confused with "the process table/list-panes
// could not be read" (#2260 review).
//
// The consumers are report-bearing as well as destructive: `af doctor`'s
// escaped-process check decides a marked process escaped a LIVE session's pane
// tree by its ABSENCE from this result, so a per-session list-panes failure that
// returned nil would read as a proven-empty tree and manufacture a false
// escaped-process finding for every ordinary in-pane child of that session.
// Taking the error lets it report blindness instead of either an empty result
// or a concrete finding, the way the package already treats an unreadable
// process table or session list.
//
// This ambient form resolves the tmux server from the caller's own environment,
// which is correct for a caller driving its OWN daemon's sessions (the teardown
// paths, which inherit the daemon's socket). Doctor — a separate process
// observing a daemon's sessions — uses CaptureSessionProcessTreesOnSocket to
// pin the read to the socket the daemon recorded, so a list-panes against
// doctor's ambient (wrong) server does not return ErrSessionVanishedBeforeCapture
// for a session that is live on the recorded one and arm a kill off the
// fall-through.
func CaptureSessionProcessTrees(cmdExec cmd.Executor, sanitizedName string) ([]proctree.Process, error) {
	captured, err := captureSessionProcessTrees(cmdExec, sanitizedName, "", false)
	return captured.Procs, err
}

// CaptureSessionProcessTreesOnSocket is CaptureSessionProcessTrees pinned to a
// specific tmux server socket via `tmux -S <socket> list-panes`. See
// CaptureSessionProcessTrees for the contract; the socket pin is the point:
// `af doctor` lists the daemon's sessions against the recorded socket, so the
// pane capture that follows must read the SAME server or a live session reads
// as vanished. An empty socket is the ambient form.
func CaptureSessionProcessTreesOnSocket(cmdExec cmd.Executor, sanitizedName, socket string) ([]proctree.Process, error) {
	captured, err := captureSessionProcessTrees(cmdExec, sanitizedName, socket, false)
	return captured.Procs, err
}

// captureSessionProcessTrees is the capture-internal form: it returns the
// whole paneCapture so a caller about to kill can compute protection from the
// SAME snapshot the process list came from (#7435 item 3). wantServer says
// the caller STORES the captured Server record — only the pre-marker sweep
// does, for the vanished-session verdict; every other capture path discards
// it, so only then does the capture pay the #{pid} #{socket_path} round
// trip and the namespace proof it enables (#7581 item 4).
func captureSessionProcessTrees(cmdExec cmd.Executor, sanitizedName, socket string, wantServer bool) (paneCapture, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec,
		socketTmuxArgs(socket, "list-panes", "-s", "-t", ExactTarget(sanitizedName), "-F", paneRowFormat)...)
	if err != nil {
		if ctx.Err() != nil {
			return paneCapture{}, fmt.Errorf("%w: list-panes for %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
		}
		// tmux ANSWERED that the session is not there. Marked with a sentinel
		// rather than returned as a plain failure, because whether it is a
		// determinate EMPTY depends on something only the caller knows: whether it
		// had already observed a pane of this session.
		//
		//   - No pane ever observed: the session does not exist, so it has no panes
		//     and no pane processes. A determinate empty.
		//   - A pane WAS observed moments ago: the session exited between the two
		//     reads, so this is a RACE, and the pane ancestry list-panes would have
		//     returned is lost. Descendants and SID members that outlive the leader
		//     are then unaccounted for — leader death cannot prove they stopped
		//     writing (#1104/#802).
		//
		// Measured: `can't find session: <name>` on a live server, `no server
		// running on <socket>` when the server itself is gone, and `no current
		// target` on a server that is still up but holds no sessions at all —
		// all exit 1. tmuxProvedSessionAbsent classifies the third by asking for
		// the session listing, because that answer names nothing to match on
		// (#3469). The corroboration listing is pinned to the same socket so a
		// caller that pinned its own list-panes corroborates against that server
		// rather than re-resolving one from its ambient environment.
		if tmuxProvedSessionAbsent(cmdExec, err, sanitizedName, socket) {
			return paneCapture{}, fmt.Errorf("%w: %v", ErrSessionVanishedBeforeCapture, err)
		}
		return paneCapture{}, fmt.Errorf("cannot list panes before teardown: %w", err)
	}
	captured, perr := paneRowsToProcesses(string(out), sanitizedName)
	// The rows carry only the numeric server fields — the socket path is
	// free-form text and is never a listing field (#7469 R5) — so the
	// capture completes the identity from the server's own single-value
	// answer on the SAME pinned socket, corroborated by its pid, but ONLY
	// when the caller stores the record (#7581 item 4). The read spends
	// what is left of the capture's own deadline — one bounded read, never
	// a fresh one stacked after it — and a read that fails leaves the
	// record incomplete: undecidable, never wrong.
	deadline, _ := ctx.Deadline()
	if wantServer && captured.Server.PID > 0 {
		captured.Server = resolveServerIdentity(corroboratedServerSocket(captured.Server,
			func() (int, string, error) {
				return serverSocketPIDForWithin(cmdExec, socket, sanitizedName, time.Until(deadline))
			}))
	}
	return captured, perr
}

// serverSocketPIDForWithin is the capture path's corroborated
// (pid, socket_path) read — the free-function counterpart of
// TmuxSession.serverSocketPIDWithin, pinned to the same socket the listing
// came from and bounded by what the listing's deadline had left
// (#7469 R5, #7581 item 4). A var seam so a test observes the remainder
// actually passed, not only the answer.
var serverSocketPIDForWithin = func(cmdExec cmd.Executor, socket, sanitizedName string, budget time.Duration) (int, string, error) {
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec, socketTmuxArgs(socket,
		"display-message", "-p", "-t", ExactTarget(sanitizedName), "#{pid} #{socket_path}")...)
	if err != nil {
		return 0, "", err
	}
	return parseServerSocketReply(string(out))
}

// paneCapture is the verified pre-kill process evidence for one tmux session:
// the process list itself, the single process-table snapshot it was captured
// from, and the proven pane roots' kernel session ids. Killing callers compute
// protection from Snapshot so the kill cannot move a reaper out from under a
// stale tree (#7435 item 3), and a vanished-session cohort proves membership
// against PaneSIDs rather than trusting a marker alone (#7435 item 5).
// RecordedPrograms is the tier-2 identity evidence the same window affords:
// each verified pane root's RECORDED launch program resolved to its agent
// executable, read from the root's argv while tmux can still answer for it
// (#7435 R16 — the root's Comm is af's `sh` wrapper, never the program).
type paneCapture struct {
	Procs            []proctree.Process
	Snapshot         map[int]proctree.Process
	PaneSIDs         map[int]bool
	RecordedPrograms map[int]string
	// Server is the tmux-server identity the pane rows carried, kernel-
	// resolved (#7469): the vanished-session sweep judges a detached marked
	// carrier against it — provably another server means foreign, refused
	// and named, never signalled. Zero when the rows could not answer the
	// server fields, which keeps every verdict undecidable.
	Server ServerIdentity
}

// paneRowsToProcesses turns list-panes rows (paneRowFormat, one per line)
// into the captured process trees of those panes. Split out of
// CaptureSessionProcessTrees so a caller that verified the incarnation the rows
// came from (the pinned reap, #6258) shares the exact same parsing.
func paneRowsToProcesses(rows, sanitizedName string) (paneCapture, error) {
	snap, err := proctree.Snapshot()
	if err != nil {
		return paneCapture{}, fmt.Errorf("cannot snapshot pane process trees before teardown: %w", err)
	}
	seen := make(map[int]bool)
	captured := paneCapture{
		Snapshot: snap,
		PaneSIDs: make(map[int]bool),
	}
	var captureErrs []error
	add := func(p proctree.Process) {
		if !seen[p.PID] {
			seen[p.PID] = true
			captured.Procs = append(captured.Procs, p)
		}
	}
	for _, field := range splitPaneRows(rows) {
		row, err := parsePaneRow(field)
		panePID := row.pid
		if err != nil {
			// The parse error carries the case-specific text: a dead pane
			// missing its @af_pane_pid stamp fails closed with an actionable
			// message (#5319), where a flat "invalid pane pid" would hide it.
			captureErrs = append(captureErrs, fmt.Errorf("%w in list-panes output", err))
			continue
		}
		if panePID <= 1 {
			captureErrs = append(captureErrs, fmt.Errorf("invalid pane pid %q in list-panes output", field))
			continue
		}
		if captured.Server.PID == 0 && row.server.PID > 0 {
			// The server identity is identical on every row of a session —
			// the first answered row records its numeric fields; the
			// caller merges the corroborated socket read and resolves the
			// kernel fields (#7469 R5).
			captured.Server = row.server
		}
		if row.dead {
			// A held dead pane (#4506 review): its pid may already be reaped, or
			// reused by another process. See deadPaneProcesses.
			survivors, asLive, err := deadPaneProcesses(snap, sanitizedName, row)
			if err != nil {
				captureErrs = append(captureErrs, err)
				continue
			}
			for _, p := range survivors {
				add(p)
			}
			if !asLive {
				continue
			}
		}
		root, ok := snap[panePID]
		if !ok {
			captureErrs = append(captureErrs, fmt.Errorf("pane process %d disappeared before its descendants could be captured", panePID))
			continue
		}
		// A real pane root is a direct child of a tmux server. Anything
		// else (stale output, a mock executor's canned reply that happens
		// to parse as a PID) is rejected so we can never sweep a tree that
		// isn't ours.
		parent, ok := snap[root.PPID]
		if !ok || !strings.HasPrefix(parent.Comm, "tmux") {
			captureErrs = append(captureErrs, fmt.Errorf("pane process %d is not a verified child of a tmux server", panePID))
			continue
		}
		captured.PaneSIDs[root.SID] = true
		// The pane root's argv is the last read of its RECORDED launch
		// program while tmux can still name the pane — a vanished session
		// loses both, so the tier-2 identity must be taken now or never
		// (#7435 R16). A non-agent or unresolvable program records no entry:
		// shell and process tabs leave tier 2 shut (#7435 R15).
		if exe := recordedPaneProgramExe(proctree.Argv(panePID)); exe != "" {
			if captured.RecordedPrograms == nil {
				captured.RecordedPrograms = make(map[int]string)
			}
			captured.RecordedPrograms[panePID] = exe
		}
		for _, p := range proctree.TreeOf(snap, panePID) {
			add(p)
		}
		// tmux makes the pane root a session leader, so members of its
		// kernel session are pane descendants even when their spawner
		// already exited and they were reparented to init.
		for _, p := range proctree.SessionMembers(snap, root.SID) {
			add(p)
		}
	}
	return captured, errors.Join(captureErrs...)
}

// graceObservationBarrier is called at the top of every observation pass of a
// vanished-session grace wait, BEFORE that pass takes its process snapshot.
// Production leaves it nil, so this costs one nil check per pass.
//
// It exists because the descendant a refusal is ABOUT has to exist, and still be
// attributable, at the moment a pass looks — and a fixture that forks it N
// milliseconds into the grace is racing reapGraceWait for that rather than
// establishing it. That race is what made
// TestVanishedSessionSweepRefusesDescendantThatChangesGeneration turn master red
// on a loaded arm64 runner with the sweep code untouched (#3766): the child was
// simply born after the last pass, so nothing mismatched and the refusal the
// assertion demands was never produced. A barrier here lets the fixture create
// the process and block until it has PROVED the process table shows it, knowing
// the pass that follows is the one that will see it.
//
// The placement is the contract, and it is two-sided:
//
//   - BEFORE the snapshot, not after. A hook past the refresh could only report
//     what the pass already saw, and a fixture that built its process there would
//     be released into the deadline check below with nothing left to look again.
//   - The deadline is checked at the BOTTOM of the loop, so the pass that follows
//     a barrier return always happens, however long the barrier held. That is the
//     guarantee the fixtures rely on; a barrier that held past the deadline would
//     otherwise buy nothing.
//
// It does not extend the deadline: with the hook nil this loop is byte-for-byte
// the loop it was, and a holding barrier simply spends the window it was given.
var graceObservationBarrier func(match string)

// observeOrphanAncestry keeps the verified tree connected throughout its grace
// period. A helper can fork, call setsid, remove its markers, and then outlive
// its parent; after the parent exits no final snapshot can reconstruct that
// relationship. Polling while the parent is still alive retains the child's
// process identity so the later ownership check can refuse it rather than
// collapsing an unprovable survivor into absence.
func observeOrphanAncestry(captured []proctree.Process, sanitizedName string, wait time.Duration) ([]proctree.Process, error) {
	deadline := time.Now().Add(wait)
	var observeErr error
	for {
		if barrier := graceObservationBarrier; barrier != nil {
			barrier(sanitizedName)
		}
		refreshed, snap, err := refreshCapturedAncestry(captured, sanitizedName)
		if err != nil {
			observeErr = errors.Join(observeErr, err)
		} else {
			captured = refreshed
			live := false
			for _, process := range captured {
				current, ok := snap[process.PID]
				if ok && current.StartID == process.StartID {
					live = true
					break
				}
			}
			if !live {
				return captured, observeErr
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return captured, observeErr
		}
		if remaining > 50*time.Millisecond {
			remaining = 50 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func refreshCapturedAncestry(captured []proctree.Process, sanitizedName string) ([]proctree.Process, map[int]proctree.Process, error) {
	snap, err := proctree.Snapshot()
	if err != nil {
		return captured, nil, fmt.Errorf("cannot refresh processes after tmux session %s vanished: %w", sanitizedName, err)
	}
	byPID := make(map[int]int, len(captured))
	refreshed := make([]proctree.Process, 0, len(captured))
	add := func(process proctree.Process) {
		refreshed = addOrReplaceOrphanCandidate(refreshed, byPID, process)
	}
	for _, process := range captured {
		add(process)
		current, alive := snap[process.PID]
		if alive && current.StartID == process.StartID {
			for _, descendant := range proctree.TreeOf(snap, process.PID) {
				add(descendant)
			}
		}
		for _, member := range proctree.SessionMembers(snap, process.SID) {
			add(member)
		}
	}
	return refreshed, snap, nil
}

// addOrReplaceOrphanCandidate deduplicates by PID without confusing a PID
// slot with a process identity. A current snapshot or marker scan must replace
// an older entry when the same PID now carries another StartID; otherwise the
// stale identity would be rejected later while the replacement escaped review.
//
// A tracked teardown requester (#5182) is never admitted to the orphan cohort:
// it is blocked on this teardown's reply, not a leak, and it cannot exit until
// the reply is sent — admitting it would only stall every grace pass and then
// report the caller it was built to spare.
func addOrReplaceOrphanCandidate(candidates []proctree.Process, byPID map[int]int, process proctree.Process) []proctree.Process {
	if isTeardownRequester(process) {
		return candidates
	}
	if index, exists := byPID[process.PID]; exists {
		if candidates[index].StartID != process.StartID {
			candidates[index] = process
		}
		return candidates
	}
	byPID[process.PID] = len(candidates)
	return append(candidates, process)
}

func processEnvValue(environ []string, name string) (string, bool) {
	prefix := name + "="
	for _, value := range environ {
		if strings.HasPrefix(value, prefix) {
			return value[len(prefix):], true
		}
	}
	return "", false
}

func selfAndAncestorProcesses(snap map[int]proctree.Process) map[int]bool {
	ancestors := make(map[int]bool)
	pid := os.Getpid()
	for range 128 {
		if pid <= 0 || ancestors[pid] {
			break
		}
		ancestors[pid] = true
		process, ok := snap[pid]
		if !ok {
			break
		}
		pid = process.PPID
	}
	return ancestors
}

// reapReason says WHY a captured process tree is being reaped. It is the
// caller's answer and never proctree's: KillEscalating is handed a list and
// cannot know whether those processes escaped anything (#2765).
//
// The distinction is not cosmetic. Every reap used to report every process as
// "leaked", and the single most reliable way to produce that line was to use the
// feature exactly as designed — `af sessions archive --self`. A session that
// archives itself makes the request from INSIDE the pane tree it is asking to
// tear down, and it is blocked on the very RPC doing the tearing, so it cannot
// exit within the grace period and is guaranteed to be SIGTERMed. Reaping it is
// not a leak; it is what "archive this session" MEANS. An operator grepping the
// log for leaks found their own supported gesture, which is exactly the noise
// that teaches people to stop reading the log.
type reapReason struct {
	// clause names the cause in the log line, so the reader is told which of the
	// two situations they are looking at rather than left to infer it.
	clause string
	// expected is true when a process still alive at SIGTERM time is a normal
	// consequence of this teardown rather than a finding about it.
	expected bool
}

var (
	// reapOnRequest: someone asked for this session to be destroyed — a kill, an
	// archive, `af reset`, or the cleanup of a session that failed to start.
	// Everything in the pane tree is SUPPOSED to die, including the caller that
	// asked. Only the escalation tiers beyond a plain SIGTERM stay warnable here:
	// a process ignoring SIGTERM or surviving SIGKILL is abnormal no matter who
	// asked for the teardown.
	reapOnRequest = reapReason{clause: "tearing down on request", expected: true}
	// reapEscaped: the tmux session is already GONE and these processes are still
	// alive carrying its ownership markers. They outlived the pane tree that was
	// supposed to contain them — the leak this reaper was built for (#1104), and
	// the case the WARNING is worth reading.
	reapEscaped = reapReason{clause: "leaked past its pane tree", expected: false}
)

// reapSessionProcesses waits for the captured processes to exit after
// kill-session, then escalates SIGTERM → SIGKILL on survivors (identity
// verified — see proctree.KillEscalating). Runs synchronously; teardown
// paths that must stay snappy call it in a goroutine. Every signal is logged
// per-process, at the severity the reason and the outcome agree on.
//
// protected is the reap's never-signal set — see reapProtectedSet. A caller
// that kills before reaping must compute it BEFORE the kill, from the same
// snapshot the pane-tree capture used: a pane shell or CLI dying on the kill
// reparents the reaper out of the tree, and protection decided from a later
// table is decided from the wrong one (Codex on #7433, #7435 item 3). nil
// computes it at reap entry from a fresh snapshot — correct only for a
// caller with no preceding kill. att is the caller's sweep attestation
// memo: a recompute that runs inside a sweep must classify claimants in
// that sweep's scope, not open a fresh memo per pass (#7509); callers with
// no sweep scope pass nil.
func reapSessionProcesses(reason reapReason, sanitizedName string, procs []proctree.Process, grace, termWait time.Duration, protected protectedSet, att *proctree.ExeAttestations) []proctree.Process {
	// A tracked teardown requester stays IN the candidate set — the reaper's
	// per-signal recheck spares it only while the registry still reports it
	// tracked, so a reprieve that ends mid-reap lets the next tier reap it
	// instead of escaping this teardown permanently (Codex on #5186). It is
	// never waited on (blocked on this teardown's reply, it cannot exit inside
	// the window) and never signalled while registered; the orphan-sweep
	// ingestion also refuses it (addOrReplaceOrphanCandidate).
	exemptedMidReap := make(map[int]bool)
	for _, p := range procs {
		if isTeardownRequester(p) {
			exemptedMidReap[p.PID] = true
			log.InfoLog.Printf("teardown requester pid %d (%s) is blocked on this teardown's reply; "+
				"excluding it from signalling (#5182)", p.PID, p.Comm)
		}
	}
	// #7433 R2 / #7435: the reaper's own chain and any positively-identified
	// agent-factory daemon subtree are exempt for EVERY caller. Descent is
	// not ownership: a daemon EnsureDaemon forked out of the pane being
	// reaped lands in that pane's captured tree, along with the CLI above it
	// and its children. proctree.Signal spares only os.Getpid; without this
	// set the reap waited seconds on the caller, then signalled its CLI, its
	// children, and the foreign daemon they belonged to. The tmux-server
	// boundary keeps it narrow: a pane process under a server reparented
	// beneath the reaper still meets the server first and is reaped.
	if protected == nil {
		snap, err := proctree.Snapshot()
		if err != nil {
			log.WarningLog.Printf("cannot snapshot the process table to compute the reaper's protected set at reap entry: %v", err)
		}
		protected = reapProtectedSet(snap, procs, att)
	}
	exemptedProtected := make(map[int]bool)
	// And it can be registered AFTER the capture — a teardown still in its
	// grace wait when this process's own destructive RPC finally reaches its
	// handler (Codex on #5186). The exempt predicate is re-run before every
	// signal tier so a late-tracked requester is still spared.
	remaining := proctree.KillEscalatingExcept(procs, func(p proctree.Process) bool {
		if exclusion, excluded := protected[p.PID]; excluded {
			if !exemptedProtected[p.PID] {
				exemptedProtected[p.PID] = true
				if exclusion.positive {
					// WARNING, not INFO: a positively-placed carrier is
					// LEFT ALIVE under a Known close, and that close may
					// remove the worktree out from under it — the line
					// names the pid, its placement, and that it keeps
					// running (#7491 item 2).
					log.WarningLog.Printf("process pid %d (%s) is %s; "+
						"excluding it from signalling — LEFT ALIVE, and not blocking cleanup (#7435, #7491)",
						p.PID, p.Comm, exclusion.reason)
				} else {
					log.InfoLog.Printf("process pid %d (%s) is %s; "+
						"excluding it from signalling (#7435)", p.PID, p.Comm, exclusion.reason)
				}
			}
			return true
		}
		if !isTeardownRequester(p) {
			return false
		}
		if !exemptedMidReap[p.PID] {
			exemptedMidReap[p.PID] = true
			log.InfoLog.Printf("teardown requester pid %d (%s) registered while this reap was already waiting; "+
				"excluding it from signalling (#5182)", p.PID, p.Comm)
		}
		return true
	}, teardownreq.SignalUnlessTracked, grace, termWait, func(outcome proctree.ReapOutcome, format string, args ...any) {
		logReapOutcome(reason, sanitizedName, outcome, format, args...)
	})
	// An UNPROVEN-ancestry exclusion is exempt from signals but not from the
	// survivor report: KillEscalatingExcept drops every still-exempt process
	// from its return, and "nothing left" on this path authorizes cleanup —
	// so a process whose daemon ancestry could not be proven must be reported
	// exactly like a SIGKILL survivor rather than silently dropped (#7435 R3,
	// Codex 4593788804). A POSITIVELY identified exclusion (a proven daemon
	// subtree, the reaper's own chain) is legitimately spared and stays out.
	for _, p := range procs {
		if verdict, ok := protected[p.PID]; !ok || verdict.positive {
			continue
		}
		if same, err := proctree.SameIdentity(p); err != nil || same {
			remaining = append(remaining, p)
		}
	}
	return remaining
}

// processPIDs renders a process set as its PIDs, in order.
func processPIDs(procs []proctree.Process) []string {
	pids := make([]string, 0, len(procs))
	for _, process := range procs {
		pids = append(pids, strconv.Itoa(process.PID))
	}
	return pids
}

// processPIDList renders a process set as a PID list for PROSE. Every teardown
// path that REFUSES on survivors has to name them — the caller's only move is to
// go look at them — and each had grown its own copy of this loop.
//
// The separator is ", " because this reads inside a sentence. A `ps -p` argument
// needs the comma WITHOUT the space, so build that from processPIDs directly:
// joined with ", " it is one space-bearing argument that shellsuggest correctly
// quotes as a single word, producing a suggestion that does not run.
func processPIDList(procs []proctree.Process) string {
	return strings.Join(processPIDs(procs), ", ")
}

// logReapOutcome writes one per-process reap line at the severity the reason and
// the outcome agree on. Split out of the closure above so the mapping is one
// named thing a test can drive through every tier — including the ones an
// end-to-end teardown cannot force on demand (a process that ignores SIGTERM,
// one that survives SIGKILL).
//
// Only a plain SIGTERM on a requested teardown is routine. The escalation tiers
// stay warnable under every reason: a process ignoring SIGTERM or surviving
// SIGKILL is abnormal regardless of who asked for the teardown, and folding
// those into the downgrade would trade one kind of unreadable log for another.
func logReapOutcome(reason reapReason, sanitizedName string, outcome proctree.ReapOutcome, format string, args ...any) {
	logger := log.WarningLog
	if reason.expected && outcome == proctree.ReapSignalled {
		logger = log.InfoLog
	}
	// sanitizedName is a runtime value that deliberately preserves `%` (see tmux
	// name sanitization), so it must be a `%s` ARGUMENT — never spliced into the
	// format string, where its `%` sequences would be interpreted and corrupt the
	// log (#1211). The clause goes through the same door: it is a constant today,
	// and passing it as an argument is what keeps that from becoming load-bearing.
	// `format` itself is a constant literal supplied by KillEscalating, so
	// concatenating it is safe.
	logger.Printf("tmux %s: %s: "+format, append([]any{sanitizedName, reason.clause}, args...)...)
}
