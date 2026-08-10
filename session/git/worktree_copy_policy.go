// Unreadable-source policy for the cross-device copier (#3066).
package git

import "fmt"

// unreadablePolicy decides what the copier does with a source file it has no
// permission to read.
//
// It exists because ONE engine serves three operations with different stakes,
// and the copier cannot tell them apart from the inside:
//
//	operation   source after success   an unreadable file
//	archive     survives               may be skipped — the original is still there
//	move        deleted                must refuse
//	restore     archive deleted        must refuse; its bytes exist nowhere else
//
// A restore that skipped a file would omit it from the restored tree and then
// delete the quarantined archive holding its only copy. That is silent permanent
// data loss, and it is what the first attempt at this feature shipped — the skip
// was unconditional and `relocateWorktreeTo` backs restore as well as archive.
type unreadablePolicy int

// copiedEntryState makes an incomplete archive a manifest state rather than a
// missing manifest entry. Present is zero so every pre-existing constructor and
// every future constructor that forgets the field keeps the strict behavior.
type copiedEntryState uint8

const (
	// refuseUnreadable is the ZERO VALUE on purpose.
	//
	// The safety of this whole mechanism rests on the default rather than on
	// every caller remembering: a new call site that constructs a copy without
	// naming a policy, or a struct that gains this field later, inherits refusal.
	// Permission to skip has to be typed out, which is exactly the property the
	// first attempt lacked.
	refuseUnreadable unreadablePolicy = iota
	// skipUnreadable records the path and continues. Only an ARCHIVE may use it,
	// because only an archive retains the complete secured original tree.
	skipUnreadable
)

const (
	copiedEntryPresent copiedEntryState = iota
	copiedEntryKnownAbsent
)

func (entry copiedEntry) knownAbsent() bool {
	return entry.state == copiedEntryKnownAbsent
}

func (p unreadablePolicy) String() string {
	if p == skipUnreadable {
		return "skip"
	}
	return "refuse"
}

// unreadablePolicyForOperation is intentionally narrow: only the archive verb
// opts into skipping. Unknown/future operation strings inherit REFUSE, as do the
// direct copier and move helpers whose zero/default path never calls this.
func unreadablePolicyForOperation(operation string) unreadablePolicy {
	if operation == "archive" {
		return skipUnreadable
	}
	return refuseUnreadable
}

// unreadableSourceError reports a source file the copier cannot read, under a
// policy that refuses to skip it.
//
// It names the operation, because the remedy differs: an archive can be retried
// after a chmod, while a restore is telling the operator that the archive itself
// holds a file they cannot read and the restore must not proceed without it.
type unreadableSourceError struct {
	path      string
	operation string
	err       error
}

func (e *unreadableSourceError) Error() string {
	// The copier constructs this before anyone knows which operation is running;
	// relocateWorktreeTo stamps it at the boundary that does. An unstamped error
	// still has to read correctly, because a copy reached some other way would
	// otherwise render "cannot  this worktree".
	if e.operation == "" {
		return fmt.Sprintf(
			"cannot copy this worktree: af has no permission to read %s, and copying while silently omitting "+
				"it would produce a tree that is missing a file without saying so; fix the file's permissions "+
				"and retry", e.path)
	}
	return fmt.Sprintf(
		"cannot %s this worktree: af has no permission to read %s, and %sing while silently omitting it "+
			"would produce a tree that is missing a file without saying so; fix the file's permissions and retry",
		e.operation, e.path, e.operation)
}

func (e *unreadableSourceError) Unwrap() error { return e.err }
