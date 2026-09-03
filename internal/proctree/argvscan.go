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
	scanIdentity = SameIdentity
)

// ArgvUnreadableError reports the one process whose argv could not be read and
// therefore failed a whole scan. The PID is carried rather than only formatted
// into the message so a caller — or a test asserting WHICH refusals are allowed
// to fail a scan — can ask about that process instead of parsing prose.
type ArgvUnreadableError struct {
	PID int
	Err error
	// OwnedByUs distinguishes the two ways a refusal reaches here, because they
	// are different situations and a reader cannot tell them apart from the
	// errno: true means the process positively runs under our uid and could be
	// something we spawned, false means its ownership could not be read at all
	// while the process was still present.
	OwnedByUs bool
}

func (e *ArgvUnreadableError) Error() string {
	whose := "and its owner could not be read either, so it cannot be ruled out as ours"
	if e.OwnedByUs {
		whose = "which runs as this user and so could be a process we spawned"
	}
	return fmt.Sprintf("cannot read the argv of pid %d, %s: %v", e.PID, whose, e.Err)
}

func (e *ArgvUnreadableError) Unwrap() error { return e.Err }

// ProcessArgv pairs a matched process with the argv it matched ON, so a caller
// can act on the arguments it recognised without a second read — which would
// race the very exec this is used to catch.
type ProcessArgv struct {
	Process Process
	Argv    []string
}

// isGone reports whether an error is a process saying it is not there any more,
// as opposed to a read that failed. ErrProcessExited is the answer both process
// oracles give for a corpse; a missing /proc entry and ESRCH are how Linux says
// the same thing when the entry is already unlinked.
func isGone(err error) bool {
	return errors.Is(err, ErrProcessExited) ||
		errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH)
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
// Before either of those becomes a failure, though, a second oracle is asked
// whether the snapshotted process is still THERE. Darwin needs it:
// kern.procargs2 answers EINVAL for a departed process exactly as it does for a
// foreign one, and kern.proc.pid then has no ownership to report either, so a
// process that merely exited mid-scan looks identical to one we are being
// refused. Its absence is a fact, and it is read as one rather than inferred
// from an errno — a process still present stays a failure (#3693).
//
// That question is asked about the process INSTANCE, not the number: a PID that
// has been recycled onto something else answers "gone" for the one we were
// refused, which is the truth about it (#3695 review).
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
		case errors.Is(err, ErrNoArgv), isGone(err):
			// Gone, or nothing to classify. Skipping can only omit a process
			// that no longer exists; it can never invent one.
			continue
		default:
			// Not ours, so its refusal is not evidence about us. Ownership is
			// read from /proc/<pid> itself, which hidepid=1 still discloses —
			// it restricts the directory's CONTENTS, not who owns it.
			uid, known := scanUID(pid)
			if known && uid != os.Getuid() {
				continue
			}
			// Ownership did not settle it, so ask the second oracle whether the
			// process is even still there. On darwin this is the difference
			// between the two facts EINVAL covers, and nothing else can tell
			// them apart: a departed process answers kern.procargs2 exactly as a
			// foreign one does, and its ownership is unreadable for the same
			// reason it is unreadable for anything that no longer exists.
			//
			// SameIdentity rather than a bare existence check, because a PID is
			// not an identity: the snapshotted process can exit and its number be
			// reused before this line runs, and "some process answers to 45" is
			// not "the one we were refused is still there". It compares the start
			// stamp, so a recycled PID reports the original instance as departed —
			// the (pid, StartID) pairing this package uses everywhere it is about
			// to act on a process.
			//
			// "Gone" is established POSITIVELY: only a definitive false skips.
			// An identity that could not be revalidated is UNKNOWN and still
			// fails, as does a process that is still itself.
			if same, identityErr := scanIdentity(process); identityErr == nil && !same {
				continue
			}
			return nil, &ArgvUnreadableError{PID: pid, Err: err, OwnedByUs: known}
		}
		if match(argv) {
			matched = append(matched, ProcessArgv{Process: process, Argv: argv})
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Process.PID < matched[j].Process.PID })
	return matched, nil
}
