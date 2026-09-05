package session

import "errors"

// Adoption fencing (#3865).
//
// A task session whose run has finished may carry a declared on_complete verb —
// archive or kill. The daemon decides that verb at the completion edge and acts
// on it minutes later, after post_worktree_commands finish. The session is
// visible and Ready for that whole wait, and a user who prompts it has ADOPTED
// it: the work is theirs now, not the task's, and the stale verb must not land
// on it.
//
// #2953 tried four times to DETECT that from lifecycle state and failed four
// times, because each signal answers a different question:
//
//   - "still idle now" is a LEVEL. A user's turn that starts and settles reads
//     LiveReady again, and taskRunActive is permanently false once the task's own
//     run ends, so neither can separate the task's idle from the user's.
//   - stateEpoch moves on both edges of that turn but NOT on delivery: a prompt
//     is handed to the agent-server with no Transition, so between the send and
//     the poll observing work every lifecycle signal reads exactly as it did at
//     completion.
//   - and any amount of detecting is still not SERIALIZING. A check that runs
//     before the teardown claims its locks leaves a gap for a delivery to land
//     in while the teardown is on its way to the destructive call.
//
// So this file holds two things, and they are deliberately one mutex apart:
//
//	deliveries — a monotonic count of DELIVERIES, an event, bumped by every
//	             agent-server entry point that writes to the PTY.
//	closed     — a fence the teardown shuts before it reads that count.
//
// # The critical sections, exactly
//
// Everything below runs under i.mu, and the whole guarantee is that these two
// bodies cannot interleave:
//
//	DELIVERY (NoteAdoptionDelivery, called by the agent-server BEFORE the write)
//	  ┌ i.mu ─────────────────────────────────────────┐
//	  │ if closed { return ErrAdoptionFenced }         │   ← refuse
//	  │ deliveries++                                   │   ← count
//	  └────────────────────────────────────────────────┘
//	  … then the actual PTY write happens, outside the lock …
//
//	TEARDOWN (CloseAdoptionFence, called by the daemon INSIDE the session's
//	          killsInFlight claim + per-session op-lock, before it destroys)
//	  ┌ i.mu ─────────────────────────────────────────┐
//	  │ closed = true                                  │   ← shut
//	  │ return deliveries                              │   ← read
//	  └────────────────────────────────────────────────┘
//	  … compare against the baseline; destroy, or stand down and reopen …
//
// One mutex, so one of the two goes first, and both outcomes are safe:
//
//   - delivery first  ⇒ it counted, so the teardown reads a count that differs
//     from the baseline and stands down. The session lives, and the write lands.
//   - teardown first  ⇒ the fence is shut, so the delivery is REFUSED and no
//     byte reaches a pane the teardown is about to tear down.
//
// There is no third ordering, which is what "the delivery either happens before
// the decision and is seen, or finds the session gone" means when the delivering
// path cannot take the daemon's op-lock. Manager.SendPrompt CAN take it and does
// — it is serialized by the op-lock as well, and the count is its second belt —
// but browser/TUI PTY input reaches InputTab with no manager lock at all, and
// this fence is the whole of its serialization.
//
// # Why the baseline is captured at the transition, not read later
//
// atRunEnd is written inside the SAME i.mu section that clears taskRunActive —
// the completion transition itself. #2953 captured it afterwards, in the poll,
// with persistPollChange (storage I/O) in between; a delivery landing in that
// gap was counted INTO the baseline and read as though it had always been there.
// Captured at the transition, a delivery anywhere after it — including inside the
// poll's own remaining work — is strictly greater than the baseline.
//
// # Why the mark is taken BEFORE the write
//
// A send that crosses a socket has an ambiguous failure ("never sent" vs "sent,
// reply lost"). For THIS question a possible delivery must count as one, or a
// send that landed with a lost reply leaves the session eligible for a teardown
// that destroys the user's work. Counting first also removes the window in which
// the prompt has landed and the mark has not. The cost is over-counting a
// delivery that never happened, whose only effect is standing a teardown down —
// leaving a session in place, which is the recoverable outcome and the same one
// the hook-wait timeout already produces.
//
// In-memory only, and deliberately: it describes a window between one
// completion edge and one teardown goroutine, and no such window survives a
// daemon restart.

// ErrAdoptionFenced refuses a PTY write to a session whose teardown has already
// claimed it. It is returned to the writer — a browser terminal frame, a TUI
// attach keystroke — so the stream ends with a cause instead of the keystroke
// being silently swallowed by a session that is about to stop existing.
var ErrAdoptionFenced = errors.New("session is being torn down; input refused")

// adoptionFence is the per-instance state described above. Guarded by i.mu; it
// is a plain value on Instance so it cannot be lost across a runtime swap the
// way the agent-server cache is.
type adoptionFence struct {
	// deliveries counts writes to the PTY, monotonically and forever. Only ever
	// compared for equality against a captured baseline, never interpreted.
	deliveries uint64
	// atRunEnd is deliveries as it stood at the completion transition.
	atRunEnd uint64
	// closed refuses further deliveries while a teardown holds this session.
	closed bool
}

// NoteAdoptionDelivery records that a delivery is about to be written to this
// session's PTY, and reports whether it may proceed. Every agent-server entry
// point that writes to the PTY calls it FIRST — see the file comment for why
// before rather than after, and TestAgentServerWritePathsAreAllAdoption for the
// property that keeps the set complete.
func (i *Instance) NoteAdoptionDelivery() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adoption.closed {
		return ErrAdoptionFenced
	}
	i.adoption.deliveries++
	return nil
}

// AdoptionDeliveries returns the current delivery count. For tests and for the
// daemon's stand-down log line; the DECISION reads the count through
// CloseAdoptionFence, which is the only read serialized against delivery.
func (i *Instance) AdoptionDeliveries() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.adoption.deliveries
}

// AdoptionDeliveriesAtRunEnd returns the delivery count as it stood at the
// completion transition — the baseline a teardown owed to that completion
// compares against. Zero for a session whose run never ended, which no teardown
// is owed for.
func (i *Instance) AdoptionDeliveriesAtRunEnd() uint64 {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.adoption.atRunEnd
}

// CloseAdoptionFence shuts the fence and returns the delivery count in ONE
// critical section. The caller must already hold the session's daemon-side fence
// (killsInFlight + the per-session op-lock) so that the other delivery path,
// Manager.SendPrompt, is serialized too; this call covers the PTY paths, which
// take no manager lock.
//
// Every caller must pair it with ReopenAdoptionFence once the operation it
// guards has finished, whichever way that went.
func (i *Instance) CloseAdoptionFence() uint64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoption.closed = true
	return i.adoption.deliveries
}

// ReopenAdoptionFence readmits deliveries. Called when a teardown stands down,
// and also after one that went through: the session is gone from the manager by
// then, so reopening is a no-op for it, and unconditional reopening is what
// keeps a teardown that failed for any other reason from leaving a live session
// permanently unable to accept input.
func (i *Instance) ReopenAdoptionFence() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.adoption.closed = false
}

// captureAdoptionBaselineLocked pins the delivery count as the completion
// transition sees it. Caller holds i.mu for writing, and must be the same
// critical section that clears taskRunActive — that adjacency is the fix for
// #2953's third P1 and is asserted by
// TestAdoptionBaselineIsCapturedAtTheCompletionTransition.
func (i *Instance) captureAdoptionBaselineLocked() {
	i.adoption.atRunEnd = i.adoption.deliveries
}
