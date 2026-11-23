package session

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// accountSwapRecorder serializes a swap with the question "how did it end?": the
// swap holds mu from start to its terminal result, so a status request made while
// it is mid-flight WAITS for it instead of observing an intermediate layout.
// The terminal record also carries the id the swap arrived with, so the daemon
// asking after a lost response can tell THIS swap's outcome from an earlier
// one's stale record (#7773).
type accountSwapRecorder struct {
	mu      sync.Mutex
	outcome string
	program string
	id      string
	// cancelled is the last swap id the server-side discard tombstoned: a
	// request that names it can never commit — its staged scope is gone —
	// so the request is refused into rolled_back instead (#7773 R1).
	cancelled string
}

// discardStaged tombstones a swap id: a request that names it afterwards can
// only roll back. It is the server-side half of the daemon's "this swap is not
// committed" verdict, recorded under the same serialization the swap runs in.
func (r *accountSwapRecorder) discardStaged(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		r.cancelled = id
	}
}

// noteStagedDiscarded publishes the discard's tombstone as this swap's own
// terminal verdict (#7793): the staged scope is provably gone, so a daemon
// whose record-clear checkpoint was lost asks the SAME question on replay and
// gets the replayable rolled_back this swap earned — never an anonymous
// "none" it cannot tell from "the request is still queued". It claims the
// slot only when EMPTY, the refuseLocked rule: a straggler's discard must not
// displace the terminal record of a swap that actually ran.
func (r *accountSwapRecorder) noteStagedDiscarded(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuseLocked(id)
}

// refuseLocked records a request refused before it ever reached the rotation —
// tombstoned, or staged-for-another-swap — under its own id. It claims the
// slot only when it is EMPTY: a straggler must never displace any swap's
// terminal record — not even its own, where a duplicated request would
// otherwise downgrade a real commit into a rollback the host never did
// (#7773 R1). A straggler's own reconcile still reaches not-committed through
// the stale-record arm. Caller holds r.mu.
func (r *accountSwapRecorder) refuseLocked(id string) {
	if r.id == "" {
		r.id, r.outcome, r.program = id, swapOutcomeRolledBack, ""
	}
}

// run executes swap and records how it ended, under the swap's own identity.
func (r *accountSwapRecorder) run(id string, swap func() (string, error)) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" && id == r.cancelled {
		// The daemon already resolved this swap as not committed and its
		// staged scope was discarded — the request arrived too late to ever
		// commit (#7773 R1).
		r.refuseLocked(id)
		return "", fmt.Errorf("account swap %s: the staged scope was discarded before the request ran; refusing to rotate", id)
	}
	prevID, prevOutcome, prevProgram := r.id, r.outcome, r.program
	r.id, r.outcome, r.program = id, swapOutcomeUnknown, "" // this swap's, until it is terminal
	program, err := swap()
	switch {
	case err == nil:
		r.outcome, r.program = swapOutcomeCommitted, program
	case errors.Is(err, errStageNotMine):
		// Refused before the rotation — the staged scope was not this
		// swap's to take, and the refusal arrived while the outgoing agent
		// was still live. The straggler claims a free slot as its own
		// rolled_back; any terminal record already there is restored, so a
		// late arrival can never displace another swap's evidence — or
		// downgrade its own. A post-stop refusal is errStageRebound, which
		// does NOT match this arm: the outgoing agent was restarted on the
		// unchanged scope, making the refusal this swap's own rolled_back
		// (the default arm).
		if prevID != "" {
			r.id, r.outcome, r.program = prevID, prevOutcome, prevProgram
		} else {
			r.outcome = swapOutcomeRolledBack
		}
	case strings.Contains(err.Error(), accountScopeIndeterminate):
		r.outcome = swapOutcomeUnknown
	default:
		r.outcome = swapOutcomeRolledBack
	}
	return program, err
}

// status blocks until any running swap is terminal, then reports its outcome
// and the id of the swap that outcome belongs to.
func (r *accountSwapRecorder) status() (outcome, program, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome == "" {
		return swapOutcomeNone, "", ""
	}
	return r.outcome, r.program, r.id
}
