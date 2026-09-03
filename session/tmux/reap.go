package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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
// tmux session's panes: each pane root (verified to be a live child of a
// tmux server), its ppid-descendants, and its kernel-session members. The
// teardown paths call it BEFORE kill-session; `af doctor` uses it to map a
// live session's legitimate processes. This public diagnostic form is strictly
// best-effort: a command/snapshot failure returns nil, while malformed individual
// pane rows are omitted and any independently verified panes are still returned.
// Destructive teardown uses captureSessionProcessTrees below so it also receives
// the completeness error and can refuse workspace cleanup.
//
// The list-panes probe is bounded by tmuxCommandTimeout (#1917): it runs first
// on the kill teardown, so an unbounded stall here wedges the kill before
// kill-session is even attempted. A tripped deadline degrades to the existing
// nil (best-effort) result — nothing is reaped, which is the safe direction: a
// wedged server has told us nothing about which processes are actually leaked.
func SessionProcessTrees(cmdExec cmd.Executor, sanitizedName string) []proctree.Process {
	procs, _ := captureSessionProcessTrees(cmdExec, sanitizedName)
	return procs
}

// captureSessionProcessTrees is the evidence-bearing half of
// SessionProcessTrees. Ordinary user-driven teardown remains best-effort and uses
// any partial result, but a caller about to delete or move the worktree also needs
// to know whether the capture itself was complete. Returning that answer separately
// prevents "no descendants" from being confused with "the process table/list-panes
// could not be read" (#2260 review).
func captureSessionProcessTrees(cmdExec cmd.Executor, sanitizedName string) ([]proctree.Process, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmdExec,
		"list-panes", "-s", "-t", exactTarget(sanitizedName), "-F", "#{pane_pid}")
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: list-panes for %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
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
		// (#3469).
		if tmuxProvedSessionAbsent(cmdExec, err, sanitizedName) {
			return nil, fmt.Errorf("%w: %v", ErrSessionVanishedBeforeCapture, err)
		}
		return nil, fmt.Errorf("cannot list panes before teardown: %w", err)
	}
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot snapshot pane process trees before teardown: %w", err)
	}
	seen := make(map[int]bool)
	var procs []proctree.Process
	var captureErrs []error
	add := func(p proctree.Process) {
		if !seen[p.PID] {
			seen[p.PID] = true
			procs = append(procs, p)
		}
	}
	for _, field := range strings.Fields(string(out)) {
		panePID, err := strconv.Atoi(field)
		if err != nil || panePID <= 1 {
			captureErrs = append(captureErrs, fmt.Errorf("invalid pane pid %q in list-panes output", field))
			continue
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
	return procs, errors.Join(captureErrs...)
}

// markedOrphanProcesses filters a previously captured pane process tree down to
// processes whose immutable launch environment proves they belong to this exact
// tmux generation and AF home. The capture happens while the listed session
// still exists; if ownership lookup then loses the session, AF_SESSION,
// AF_SESSION_GEN, and AF_HOME provide the authority that the vanished tmux
// environment no longer can.
//
// The candidate set is load-bearing. Scanning every same-user process would
// turn an unrelated unreadable /proc environment into a possible helper and
// make successful absence impossible on hardened hosts. Within the captured
// tree, unreadable or mismatched provenance is genuinely UNKNOWN and blocks
// worktree deletion rather than being collapsed into "not ours".
//
// An EMPTY cohort — the blind vanished-session sweep, which captured no
// predecessor to place anyone against — still refuses, but as ONE message rather
// than a line per pid: there the pid/generation pairing is the operator's whole
// decision procedure, and blindGenerationRefusal is what carries it (#3706).
func markedOrphanProcesses(candidates []proctree.Process, sanitizedName, ownHome string, generations orphanGenerationSet) ([]proctree.Process, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot inspect captured processes after tmux session vanished: %w", err)
	}
	selfUID, selfUIDKnown := proctree.UID(os.Getpid())
	if !selfUIDKnown {
		return nil, fmt.Errorf("cannot determine reset process ownership while checking vanished tmux session %s", sanitizedName)
	}
	selfChain := selfAndAncestorProcesses(snap)
	cleanHome := filepath.Clean(ownHome)
	var owned []proctree.Process
	var inspectErrs []error
	var unplaceable []markedGeneration
	for _, process := range candidates {
		same, identityErr := proctree.SameIdentity(process)
		if identityErr != nil {
			inspectErrs = append(inspectErrs, fmt.Errorf("cannot determine whether captured pid %d is still the same process: %w",
				process.PID, identityErr))
			continue
		}
		if !same {
			continue
		}
		uid, uidKnown := proctree.UID(process.PID)
		if !uidKnown {
			sameAfter, recheckErr := proctree.SameIdentity(process)
			switch {
			case recheckErr != nil:
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot recheck captured pid %d after owner lookup failed: %w",
					process.PID, recheckErr))
			case sameAfter:
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot determine owner of captured live pid %d", process.PID))
			}
			continue
		}
		if uid != selfUID {
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d belongs to uid %d, not reset uid %d",
				process.PID, uid, selfUID))
			continue
		}

		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			sameAfter, recheckErr := proctree.SameIdentity(process)
			switch {
			case recheckErr != nil:
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot recheck captured pid %d after environment lookup failed: %w",
					process.PID, errors.Join(envErr, recheckErr)))
			case sameAfter:
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot determine whether captured live pid %d belongs to session %s: %w",
					process.PID, sanitizedName, envErr))
			}
			continue
		}
		processSession, hasSession := processEnvValue(environ, EnvMarkerSession)
		if !hasSession {
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d has no %s marker for vanished session %s",
				process.PID, EnvMarkerSession, sanitizedName))
			continue
		}
		if processSession != sanitizedName {
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d marks session %s instead of vanished session %s",
				process.PID, processSession, sanitizedName))
			continue
		}
		processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
		if !hasHome {
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d marks session %s but has no %s ownership marker",
				process.PID, sanitizedName, EnvMarkerHome))
			continue
		}
		if filepath.Clean(processHome) != cleanHome {
			continue
		}
		processGeneration, hasGeneration := processEnvValue(environ, EnvMarkerGeneration)
		switch {
		case hasGeneration && generations.empty():
			// An EMPTY cohort is the blind vanished-session sweep (cleanup.go): it
			// captured no predecessor, so it has nothing to place this generation
			// against — a different statement from "outside a cohort I know", which
			// is what the case below reports. Collected instead of refused one pid
			// at a time so the refusal can be ONE message naming every unplaceable
			// pid and the generation it carries: on this path that comparison is
			// the operator's entire decision procedure, and it does not fit in a
			// per-pid line (#3706). See blindGenerationRefusal.
			unplaceable = append(unplaceable, markedGeneration{pid: process.PID, generation: processGeneration})
			continue
		case hasGeneration && !generations.values[processGeneration]:
			// Marker scans are filtered before they enter this bounded set. A
			// different generation already present here may be a descendant that
			// changed its environment after capture, so absence is not proved. It
			// must remain a refusal rather than being silently reclassified as a
			// replacement (#3309 review).
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d marks generation %s outside the vanished session %s generation cohort",
				process.PID, processGeneration, sanitizedName))
			continue
		case !hasGeneration && !generations.legacy:
			inspectErrs = append(inspectErrs, fmt.Errorf("captured live pid %d marks session %s but has no %s generation marker",
				process.PID, sanitizedName, EnvMarkerGeneration))
			continue
		}
		if selfChain[process.PID] {
			inspectErrs = append(inspectErrs, fmt.Errorf("refusing to reap reset process or ancestor pid %d for vanished session %s",
				process.PID, sanitizedName))
			continue
		}
		owned = append(owned, process)
	}
	if len(unplaceable) > 0 {
		inspectErrs = append(inspectErrs, blindGenerationRefusal(sanitizedName, unplaceable))
	}
	return owned, errors.Join(inspectErrs...)
}

// markedGeneration is one live process carrying the vanished session's
// AF_SESSION marker and this home's AF_HOME marker, plus a generation the sweep
// has no captured predecessor to place it against.
type markedGeneration struct {
	pid        int
	generation string
}

// blindGenerationRefusal is the message a blind vanished-session sweep leaves
// behind, and on that path it is the whole explanation an operator gets (#3706).
//
// It deliberately does NOT say "kill these pids". The callers that still reach
// the blind branch after #3700 — the background CleanupSessions sweep and
// unlocked internal cleanup — hold no claim on this session's lifecycle, so they
// cannot rule out that a marked process is a legitimate same-name REPLACEMENT
// created while the sweep was running (#3309). A kill hint here would be a
// footgun aimed at a healthy session, printed by a sweep that runs on its own.
//
// What it can do is hand over the comparison it is not allowed to make itself.
// The generation each pid carries is already in hand; the other half — what the
// LIVE session of that name carries — is one `show-environment` away, and the
// two verdicts that follow from it are unambiguous. So the message names the
// pids and their generations, gives that read, and states both outcomes.
//
// The suggested target is exactTarget's `=name:` form rather than a bare name on
// purpose: tmux resolves a bare -t as a prefix match, so a pasted suggestion
// could report a DIFFERENT session's generation — in the one message whose
// answer decides what an operator kills.
func blindGenerationRefusal(sanitizedName string, marked []markedGeneration) error {
	named := make([]string, 0, len(marked))
	for _, process := range marked {
		named = append(named, fmt.Sprintf("pid %d (%s=%s)", process.pid, EnvMarkerGeneration, process.generation))
	}
	// sanitizedName preserves `%` by design, so it rides as an ARGUMENT and never
	// gets spliced into the format string (#1211) — including inside the
	// suggestions, which are built from pieces by shellsuggest.
	return fmt.Errorf("marked processes outlived vanished tmux session %s carrying a generation this sweep"+
		" has no captured predecessor to place: %s. Nothing was signalled and nothing was removed — this sweep holds"+
		" no claim on that session name, so a marked process may be a live same-name replacement rather than a"+
		" leftover of the vanished session, and it will not signal what it cannot attribute; it retries on every"+
		" pass. To tell the two apart, compare each generation above against the one the live session of that name"+
		" carries: %s. %s names the af session behind that tmux name, and each pid's own copy stays readable at"+
		" /proc/<pid>/environ. A generation that matches the live session belongs to it — leave that pid alone."+
		" A generation no live session claims is a leftover of the vanished session — that pid is safe to kill,"+
		" and the next sweep then proceeds.",
		sanitizedName, strings.Join(named, ", "),
		shellsuggest.Command("tmux", "show-environment", "-t", exactTarget(sanitizedName), EnvMarkerGeneration),
		shellsuggest.Command("af", "sessions", "list", "--json"))
}

// orphanGenerationSet is the immutable cohort present when a vanished-session
// sweep begins. A pre-generation process is represented by legacy; every newer
// tmux launch carries a random value. Later marker refreshes may add helpers from
// this cohort, but never processes from a same-named replacement (#3309).
type orphanGenerationSet struct {
	legacy bool
	values map[string]bool
}

func orphanGenerations(candidates []proctree.Process, sanitizedName string) orphanGenerationSet {
	generations := orphanGenerationSet{values: make(map[string]bool)}
	for _, process := range candidates {
		same, err := proctree.SameIdentity(process)
		if err != nil || !same {
			continue
		}
		environ, err := proctree.Environ(process.PID)
		if err != nil {
			continue
		}
		if session, ok := processEnvValue(environ, EnvMarkerSession); !ok || session != sanitizedName {
			continue
		}
		generation, ok := processEnvValue(environ, EnvMarkerGeneration)
		if !ok {
			generations.legacy = true
			continue
		}
		generations.values[generation] = true
	}
	return generations
}

func (g orphanGenerationSet) contains(environ []string) bool {
	generation, ok := processEnvValue(environ, EnvMarkerGeneration)
	if !ok {
		return g.legacy
	}
	return g.values[generation]
}

func (g orphanGenerationSet) empty() bool {
	return !g.legacy && len(g.values) == 0
}

// refreshOrphanCandidates closes the capture-to-marker TOCTOU window. A pane
// can launch another helper after the pre-marker snapshot but before the marker
// lookup observes that tmux removed the session. Once absence is authoritative,
// no pane remains to launch more work, so a fresh process-table snapshot can add
// every process still tied to the captured kernel sessions/trees plus any
// readable process carrying the exact AF_SESSION marker from the generation
// cohort fixed at sweep entry.
//
// Environment failures on unrelated processes are deliberately ignored: they
// are not evidence of membership. Failures on captured/SID/descendant
// candidates remain visible when markedOrphanProcesses validates the bounded
// set, preserving unknown without making hardened hosts globally unreadable.
func refreshOrphanCandidates(captured []proctree.Process, sanitizedName string, generations *orphanGenerationSet) ([]proctree.Process, error) {
	refreshed, snap, err := refreshCapturedAncestry(captured, sanitizedName)
	if err != nil {
		return captured, err
	}
	byPID := make(map[int]int, len(refreshed))
	for index, process := range refreshed {
		byPID[process.PID] = index
	}
	for _, process := range snap {
		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			continue
		}
		if session, ok := processEnvValue(environ, EnvMarkerSession); ok && session == sanitizedName &&
			(generations == nil || generations.contains(environ)) {
			refreshed = addOrReplaceOrphanCandidate(refreshed, byPID, process)
		}
	}
	return refreshed, nil
}

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
func addOrReplaceOrphanCandidate(candidates []proctree.Process, byPID map[int]int, process proctree.Process) []proctree.Process {
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
func reapSessionProcesses(reason reapReason, sanitizedName string, procs []proctree.Process, grace, termWait time.Duration) []proctree.Process {
	return proctree.KillEscalating(procs, grace, termWait, func(outcome proctree.ReapOutcome, format string, args ...any) {
		logReapOutcome(reason, sanitizedName, outcome, format, args...)
	})
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
