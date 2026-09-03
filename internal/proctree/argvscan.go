package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// scanSnapshot, scanArgv and scanUID are the reads ProcessesMatchingArgv makes,
// indirected so a test can reproduce the failures that matter and cannot
// otherwise be staged on a live /proc: a process table that cannot be READ, an
// argv the kernel refuses, and the ownership that decides whether that refusal
// is about us. A scan that got any of them wrong would be indistinguishable from
// a healthy one — the hidepid=1 case in particular looks perfectly fine on every
// machine that is not mounted that way.
var (
	scanSnapshot = Snapshot
	scanArgv     = readArgv
	scanUID      = readUID
)

// ArgvUnreadableError reports the one process whose argv could not be read and
// therefore failed a whole scan. The PID is carried rather than only formatted
// into the message so a caller — or a test asserting WHICH refusals are allowed
// to fail a scan — can ask about that process instead of parsing prose.
type ArgvUnreadableError struct {
	PID int
	Err error
}

func (e *ArgvUnreadableError) Error() string {
	return fmt.Sprintf("cannot read the argv of pid %d: %v", e.PID, e.Err)
}

func (e *ArgvUnreadableError) Unwrap() error { return e.Err }

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
// # Whose refusal counts
//
// That refusal rule is scoped by OWNERSHIP, and the scoping is not a softening
// of it. A process owned by another uid cannot be a child this process spawned,
// so being refused its argv says nothing about anything we are looking for —
// while failing the scan on it would be catastrophic on the machines where it
// happens. A procfs mounted hidepid=1 refuses every OTHER user's
// /proc/<pid>/cmdline, so an unscoped rule would fail on the first foreign pid
// and make every caller refuse, permanently, on every retry, on a shared box
// that may hold no launcher at all. A guard whose failure mode is permanent is
// worse than the hazard it prevents.
//
// Darwin has the same shape with different spelling and needs no special mount:
// XNU answers kern.procargs2 for another user's process with EINVAL — not
// EPERM — so there this is the ordinary case rather than the exotic one. That is
// why the rule is written against OWNERSHIP and not against a list of errno
// values, which differ by platform and by kernel version.
//
// A refusal on a process running as US still fails the scan: that one could be
// the thing we are looking for. So does one whose owner cannot be read at all —
// "I could not tell whether it is ours" is a third answer, and it is not the
// negative.
//
// # Platforms
//
// On an ordinary Linux mount /proc/<pid>/cmdline is world-readable, so the only
// routine per-pid failure is the process exiting mid-scan and this returns a
// complete answer. Under hidepid=1, and on darwin — where XNU withholds
// KERN_PROCARGS2 from foreign uids as a matter of course — the answer is
// complete for everything this process could own and silent about the rest,
// which is the most that can honestly be said there.
//
// What remains reportable on both is a refusal about a process that COULD be
// ours: a same-uid target that darwin's code-signing restrictions cover, or one
// whose ownership could not be read. Those come back as *ArgvUnreadableError,
// and a caller must handle it rather than read a truncated list as complete.
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
			// Not ours, so its refusal is not evidence about us. Ownership is
			// read from /proc/<pid> itself, which hidepid=1 still discloses —
			// it restricts the directory's CONTENTS, not who owns it.
			if uid, known := scanUID(pid); known && uid != os.Getuid() {
				continue
			}
			return nil, &ArgvUnreadableError{PID: pid, Err: err}
		}
		if match(argv) {
			matched = append(matched, ProcessArgv{Process: process, Argv: argv})
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Process.PID < matched[j].Process.PID })
	return matched, nil
}
