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
	// reapGraceWait is how long leaked processes get to exit on their own
	// after kill-session (SIGHUP) before being SIGTERMed. Matches
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

// markedOrphanProcesses finds same-user processes whose immutable launch
// environment proves they belong to this exact tmux session and AF home. It is
// the fallback after tmux authoritatively reports that a session vanished
// before its pane tree could be captured: AF_SESSION recovers the lost session
// identity, while AF_HOME keeps a same-named process from another install out
// of the reap set.
//
// An unreadable environment on a live same-user process is UNKNOWN, not an
// absent marker. Returning both the processes we did prove and an error lets
// cleanup reap known-owned orphans while still refusing worktree deletion when
// it could not rule out another one. Processes owned by another uid cannot have
// been launched by this user's reset target and are skipped before reading
// their environment.
func markedOrphanProcesses(sanitizedName, ownHome string) ([]proctree.Process, error) {
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot inspect processes after tmux session vanished: %w", err)
	}
	selfUID, selfUIDKnown := proctree.UID(os.Getpid())
	if !selfUIDKnown {
		return nil, fmt.Errorf("cannot determine reset process ownership while checking vanished tmux session %s", sanitizedName)
	}
	selfChain := selfAndAncestorProcesses(snap)
	cleanHome := filepath.Clean(ownHome)
	var owned []proctree.Process
	var inspectErrs []error
	for _, process := range snap {
		uid, uidKnown := proctree.UID(process.PID)
		if !uidKnown {
			if proctree.AliveSame(process) {
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot determine owner of live pid %d", process.PID))
			}
			continue
		}
		if uid != selfUID {
			continue
		}

		environ, envErr := proctree.Environ(process.PID)
		if envErr != nil {
			if proctree.AliveSame(process) {
				inspectErrs = append(inspectErrs, fmt.Errorf("cannot determine whether live pid %d belongs to session %s: %w",
					process.PID, sanitizedName, envErr))
			}
			continue
		}
		processSession, hasSession := processEnvValue(environ, EnvMarkerSession)
		if !hasSession || processSession != sanitizedName {
			continue
		}
		processHome, hasHome := processEnvValue(environ, EnvMarkerHome)
		if !hasHome {
			inspectErrs = append(inspectErrs, fmt.Errorf("live pid %d marks session %s but has no %s ownership marker",
				process.PID, sanitizedName, EnvMarkerHome))
			continue
		}
		if filepath.Clean(processHome) != cleanHome {
			continue
		}
		if selfChain[process.PID] {
			inspectErrs = append(inspectErrs, fmt.Errorf("refusing to reap reset process or ancestor pid %d for vanished session %s",
				process.PID, sanitizedName))
			continue
		}
		owned = append(owned, process)
	}
	return owned, errors.Join(inspectErrs...)
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

// reapLeakedProcesses waits for the captured processes to exit after
// kill-session, then escalates SIGTERM → SIGKILL on survivors (identity
// verified — see proctree.KillEscalating). Runs synchronously; teardown
// paths that must stay snappy call it in a goroutine. Every signal is logged
// per-process.
func reapLeakedProcesses(sanitizedName string, procs []proctree.Process, grace, termWait time.Duration) []proctree.Process {
	return proctree.KillEscalating(procs, grace, termWait, func(format string, args ...any) {
		// sanitizedName is a runtime value that deliberately preserves `%` (see
		// tmux name sanitization), so it must be a `%s` ARGUMENT — never spliced
		// into the format string, where its `%` sequences would be interpreted
		// and corrupt the log (#1211). `format` itself is a constant literal
		// supplied by KillEscalating, so concatenating it is safe.
		log.WarningLog.Printf("tmux %s: "+format, append([]any{sanitizedName}, args...)...)
	})
}
