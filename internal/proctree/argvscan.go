package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"syscall"
)

// ErrNoArgv reports that a process has no argument vector to classify — a
// kernel thread, or one the kernel has already torn down far enough to clear
// it.
//
// Like ErrProcessExited it is a POSITIVE finding rather than a read failure:
// there is nothing to match, as opposed to something we were not allowed to
// look at. A scan may skip it without inventing a fact; it must not skip a
// refusal the same way.
var ErrNoArgv = errors.New("proctree: process has no argv")

// scanSnapshot and scanArgv are the reads ProcessesMatchingArgv makes, indirected
// so a test can reproduce the two failures that matter and cannot otherwise be
// staged on a live /proc: a process table that cannot be READ, and an argv the
// kernel refuses. Both must arrive as errors rather than as an absence, and a
// scan that got them wrong would be indistinguishable from a healthy one.
var (
	scanSnapshot = Snapshot
	scanArgv     = readArgv
)

// ProcessArgv pairs a matched process with the argv it matched ON, so a caller
// can act on the arguments it recognised without a second read — which would
// race the very exec this is used to catch.
type ProcessArgv struct {
	Process Process
	Argv    []string
}

// ProcessesMatchingArgv returns every process in the live process table whose
// argument vector satisfies match, sorted by PID.
//
// It exists for identities that live in a command line and nowhere else. The
// motivating one is a `systemd-run` that has execve'd but has not yet had its
// StartTransientUnit call answered (#3667): for that interval the scope it is
// about to create does not exist, so the unit it names in its own argv is the
// only handle anything has on it — and its parent may already be gone, which
// rules out ancestry as an oracle.
//
// # Three answers, not two
//
// A process table that cannot be read is an ERROR, never an empty slice: the
// callers of this are deciding whether a tree is safe to modify, and "I could
// not look" must not arrive as "nothing is there" (#2874). The same rule governs
// each individual argv read, with one exception that is a fact rather than a
// guess: a process that has EXITED between the snapshot and its argv read, or
// that never had an argv at all (ErrNoArgv), is skipped, because "it is gone"
// and "it has nothing to classify" are both answers. Anything else — a refusal,
// a malformed read — fails the whole scan.
//
// # Platforms
//
// On Linux /proc/<pid>/cmdline is world-readable, so the only routine per-pid
// failure is the process exiting mid-scan, and this returns a complete answer.
// On darwin the kernel withholds KERN_PROCARGS2 for foreign uids and for
// code-signing-restricted processes, so a scan of the whole table normally
// reports UNKNOWN rather than a partial answer. That is the honest result and
// it is deliberate: the one caller today is Linux-gated, and a future darwin
// caller must handle the error rather than read a truncated list as complete.
func ProcessesMatchingArgv(match func(argv []string) bool) ([]ProcessArgv, error) {
	if match == nil {
		return nil, errors.New("proctree: ProcessesMatchingArgv needs a match function")
	}
	snap, err := scanSnapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot read the process table: %w", err)
	}
	var matched []ProcessArgv
	for pid, process := range snap {
		argv, err := scanArgv(pid)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoArgv), errors.Is(err, ErrProcessExited),
			errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ESRCH):
			// Gone, or nothing to classify. Skipping can only omit a process
			// that no longer exists; it can never invent one.
			continue
		default:
			return nil, fmt.Errorf("cannot read the argv of pid %d: %w", pid, err)
		}
		if match(argv) {
			matched = append(matched, ProcessArgv{Process: process, Argv: argv})
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Process.PID < matched[j].Process.PID })
	return matched, nil
}
