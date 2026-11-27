package session

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/log"
)

// accountSwapRecorder serializes a swap with the question "how did it end?": the
// swap holds mu from start to its terminal result, so a status request made while
// it is mid-flight WAITS for it instead of observing an intermediate layout.
// The terminal record also carries the id the swap arrived with, so the daemon
// asking after a lost response can tell THIS swap's outcome from an earlier
// one's stale record (#7773).
//
// When store is set (the in-sandbox agent-server process), every terminal
// transition persists the recorder's state to the session dir so a restart
// rehydrates it — otherwise the recorder forgets every verdict on a process
// restart and answers anonymous "none", which a settled host cannot tell from
// "still queued" and fences the row forever (#7793). nil for the daemon's
// in-process local sessions, which have no agent-server process to restart —
// and nil for an agent-server whose session dir failed the install's
// writability probe (#7907): the explicitly-unbacked posture, decided once at
// startup and kept for the session's whole life, which is exactly this same
// no-store behavior rather than a refusal or a silent loss of backing
// discovered mid-swap.
type accountSwapRecorder struct {
	mu      sync.Mutex
	outcome string
	program string
	id      string
	// cancelled is the last swap id the server-side discard tombstoned: a
	// request that names it can never commit — its staged scope is gone —
	// so the request is refused into rolled_back instead (#7773 R1).
	cancelled string
	// store, when non-nil, is the durable backing the recorder rehydrates from
	// at startup and persists to on every terminal transition — and nil is a
	// stable, decided-once state: a daemon's in-process session never gets a
	// store, and an agent-server whose session dir failed the install's
	// writability probe is explicitly unbacked for its whole life (#7907).
	// See account_swap_recorder_store.go.
	store *accountSwapStore
}

// discardStaged tombstones a swap id: a request that names it afterwards can
// only roll back. It is the server-side half of the daemon's "this swap is not
// committed" verdict, recorded under the same serialization the swap runs in.
//
// It also claims the rolled_back verdict under that id in the SAME write, when
// the slot is free (#7793 review): once the id is tombstoned it can never
// commit, so the verdict is already determined. Publishing it now closes the
// crash window between this save and noteStagedDiscarded's later save — a
// restart that lost the second save rehydrates THIS swap's own rolled_back
// rather than anonymous none, the way a bare tombstone would leave it reading
// forever.
func (r *accountSwapRecorder) discardStaged(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.persistLocked()
	if id != "" {
		r.cancelled = id
		r.refuseLocked(id)
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
	defer r.persistLocked()
	r.refuseLocked(id)
}

// refuseLocked records a request refused before it ever reached the rotation —
// tombstoned, or staged-for-another-swap — under its own id. It claims the
// slot only when it is EMPTY: a straggler must never displace any swap's
// terminal record — not even its own, where a duplicated request would
// otherwise downgrade a real commit into a rollback the host never did
// (#7773 R1). A straggler's own reconcile still reaches not-committed through
// the stale-record arm. The one upgrade is a slot already holding THIS swap's
// own pre-rotation checkpoint (swapOutcomePending): the discard tombstoned it
// and just removed its stage, so it can never commit and its verdict is
// rolled_back. A terminal unknown is NOT upgraded — swap() ran and its scope
// could not be put back, so the rotation may have reached the host and the
// discard must not settle a rollback against a layout it cannot prove (#7862
// review). The discard holds the swap's lock, so no run() is in flight to
// clobber. Caller holds r.mu.
func (r *accountSwapRecorder) refuseLocked(id string) {
	if r.id == "" {
		r.id, r.outcome, r.program = id, swapOutcomeRolledBack, ""
		return
	}
	if r.id == id && r.outcome == swapOutcomePending {
		r.outcome = swapOutcomeRolledBack
	}
}

// run executes swap and records how it ended, under the swap's own identity.
func (r *accountSwapRecorder) run(id string, swap func() (string, error)) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.persistLocked()
	if id != "" && id == r.cancelled {
		// The daemon already resolved this swap as not committed and its
		// staged scope was discarded — the request arrived too late to ever
		// commit (#7773 R1).
		r.refuseLocked(id)
		return "", fmt.Errorf("account swap %s: the staged scope was discarded before the request ran; refusing to rotate", id)
	}
	prevID, prevOutcome, prevProgram := r.id, r.outcome, r.program
	r.id, r.outcome, r.program = id, swapOutcomePending, "" // this swap's, until it is terminal
	// Persist this swap's id under a pending verdict BEFORE the rotation
	// (#7793 review): the deferred save at the end of run leaves the prior
	// swap's terminal record on disk throughout the rotation, so a crash
	// during swap() rehydrates that stale named verdict and B's reconcile
	// mistakes it for a queued request — discarding into a definite rollback
	// even though the host may already run B's incoming account. Persisting
	// B's own id as pending makes a restart rehydrate B (not A); the
	// reconcile fences safely the way a fresh pre-fix recorder did, instead
	// of settling a row/host account mismatch.
	//
	// The checkpoint is a state DISTINCT from a terminal unknown (#7862
	// review): `pending` means the rotation has not started, so the discard
	// may upgrade it to rolled_back over a proven scopeOutgoing; a terminal
	// `unknown` means swap() ran and its scope could not be put back, which
	// must stay fenced. Keeping the states distinct lets the discard tell a
	// checkpoint it can settle from a terminal verdict it cannot.
	//
	// This pre-write is required, not best-effort: if it cannot land, the
	// prior swap's terminal record stays on disk and a restart rehydrates
	// the wrong swap's verdict — the exact mismatch above. Remove that stale
	// record so a restart rehydrates nothing (the way a fresh pre-fix
	// recorder answered none and fenced); an unlink still succeeds on a
	// full or otherwise-unwritable-as-creates disk. If even the remove
	// fails, refuse the swap before anything rotates — the host never moves
	// while the durable record could name another swap (#7793 review R1).
	if err := r.persistCheckedLocked(); err != nil {
		if clrErr := r.store.clear(); clrErr != nil {
			r.id, r.outcome, r.program = prevID, prevOutcome, prevProgram
			return "", fmt.Errorf("account swap %s: could not checkpoint the swap record before rotating: %w", id, clrErr)
		}
	}
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
//
// A non-terminal checkpoint (swapOutcomePending) is reported as anonymous
// "none": `pending` is only ever observable after a restart rehydrates it,
// because `status` blocks while `run` holds the lock, so a live caller never
// sees the in-flight checkpoint. Reporting it as `none` sends every restart
// reconcile through master's `none` arm (probe, serialized discard, re-read)
// instead of fencing the NEXT swap forever on a non-own `pending` a stale
// record left on disk (#7862 review F1) — a non-terminal checkpoint never
// tells a reconcile more than master's anonymous `none`. The slot's own
// settle still works: the discard's `refuseLocked` upgrades THIS swap's own
// `pending` to `rolled_back` internally (the recorder keeps the real state),
// so the re-read returns the slot's own terminal verdict.
func (r *accountSwapRecorder) status() (outcome, program, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome == "" || r.outcome == swapOutcomePending {
		return swapOutcomeNone, "", ""
	}
	return r.outcome, r.program, r.id
}

// persistCheckedLocked writes the recorder's durable state to its store, when
// one is installed, and returns any error so a caller that needs the write to
// land can react. Caller holds r.mu.
func (r *accountSwapRecorder) persistCheckedLocked() error {
	if r.store == nil {
		return nil
	}
	return r.store.save(accountSwapRecordData{
		Outcome:   r.outcome,
		Program:   r.program,
		ID:        r.id,
		Cancelled: r.cancelled,
	})
}

// persistLocked writes the recorder's durable state to its store, when one is
// installed. Best-effort: a failed write logs a warning — the swap's terminal
// outcome stands in memory for this process, and a restart reports the
// pre-rotation checkpoint left on disk as anonymous "none" (see status), so
// the reconcile re-asks the host the way a fresh pre-fix recorder did — never
// a wrong-account verdict — rather than failing the swap over a side record.
// Caller holds r.mu.
func (r *accountSwapRecorder) persistLocked() {
	if err := r.persistCheckedLocked(); err != nil {
		log.WarningLog.Printf("account swap recorder: could not persist the terminal record; a restart re-asks the host for it: %v", err)
	}
}

// restore rehydrates the recorder from its durable store's loaded state.
// Called once at agent-server startup, before the recorder serves any request:
// the fresh process inherits the terminal verdict + tombstone the prior process
// reached, so a restart reconcile that asks for the recorded swap's outcome
// gets this swap's own verdict back — not the anonymous "none" a stage-less,
// prev-less host can never tell from "still queued" (#7793).
func (r *accountSwapRecorder) restore(d accountSwapRecordData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcome = d.Outcome
	r.program = d.Program
	r.id = d.ID
	r.cancelled = d.Cancelled
}
