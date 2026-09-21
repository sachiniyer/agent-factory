package session

import (
	"errors"
	"time"
)

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
// The count itself stays in-memory: a restart resets it to zero, so adoption
// evidence after a restart instead comes from the durable obligation —
// owedOnComplete, cleared by a delivery's durable discharge (the notify's
// persist, which NoteAdoptionDelivery blocks on before returning nil) — and
// from the durable pane-churn watermark for input that reaches no agent-server
// entry point (#4162). The durable clear is NOT in the in-memory critical
// section: it is the notify's responsibility and runs after i.mu is released,
// so NoteAdoptionDelivery must not return nil until that persist has landed,
// or a unclean exit before the in-memory retry poll re-arms a teardown the
// adoption already vetoed. Concurrent deliveries serialize through that
// in-flight discharge: the first caller to clear owedOnComplete installs a
// discharge future on i.discharge before releasing i.mu, and a second caller
// arriving while its notify is still running waits on that future instead of
// proceeding past nil owedOnComplete to a PTY write the durable marker would
// survive.

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

// adoptionDischarge is the in-flight durable discharge of owedOnComplete that a
// single NoteAdoptionDelivery caller drives and any concurrent callers wait on
// and share. The first caller to take the marker clears it in memory, installs
// the future on Instance.discharge under i.mu, releases i.mu, and runs the
// notify. A second caller arriving while the first is still parked in the
// notify has no in-memory marker to take — the first already cleared it —
// so without the future it would proceed past nil owedOnComplete to its own
// PTY write; the durable marker that first caller's persist is about to leave
// set on a failed write would then survive an unclean exit and re-arm a
// teardown that destroys the second caller's work. Sharing the discharge makes
// the second caller wait for the first's persist result instead: on success
// both may write (the marker is durably cleared once for both); on failure
// both are refused so no PTY write lands on top of a durable marker the
// failed write left set.
//
// Set on Instance.discharge under i.mu by the discharging caller before it
// releases i.mu, and released (err assigned, done closed, the field cleared)
// under i.mu by that same caller once the notify returns. The discharged
// future's done channel is the synchronization barrier: an err write before
// close is visible to every waiter after the receive.
type adoptionDischarge struct {
	done chan struct{}
	err  error
}

// NoteAdoptionDelivery records that a delivery is about to be written to this
// session's PTY, and reports whether it may proceed. Every agent-server entry
// point that writes to the PTY calls it FIRST — see the file comment for why
// before rather than after, and TestAgentServerWritePathsAreAllAdoption for the
// property that keeps the set complete.
//
// A delivery also discharges a pending on_complete obligation: prompting a
// finished task session IS the adoption the marker exists to defer to. The
// in-memory clear happens in the same critical section as the count bump, so
// the teardown's under-fence read of either sees this delivery; the DURABLE
// clear is the notify's responsibility and runs AFTER the lock is released — it
// performs storage I/O and must not hold i.mu, let alone re-enter it.
//
// Because the durable clear is the agent-server path's ONLY durable adoption
// signal (the pane-churn watermark is explicitly disclaimed for input that
// reaches an agent-server entry point), NoteAdoptionDelivery does NOT return
// nil until the notify's persist has landed. A best-effort notify whose retry
// lives only in memory would let a unclean exit before the next poll lose the
// discharge while leaving the durable marker set, re-arming a teardown that
// reaps a session the user just adopted. So the notify returns any persist
// error and this call propagates it: on failure the in-memory marker is
// restored so the caller's refused delivery does not leave the in-memory state
// half-discharged, and a re-attempted delivery re-runs the durable clear. The
// caller — SendPromptWithStatus/InputTab/Input — must NOT write to the PTY
// when this returns an error; the PTY write on top of an unset durable marker
// is exactly the work-losing window.
//
// Two callers racing into a marked session serialize through that durable
// discharge via i.discharge: the first to take i.mu clears the marker in
// memory, installs the discharge, releases i.mu, and runs the notify; a
// second caller arriving while the notify is in flight finds no in-memory
// marker to clear (the first already cleared it) and the discharge in
// progress, so it waits on the discharge's result rather than proceeding to
// its own PTY write. The durable clear is one and the same for both: on
// success both may write (the marker is durably gone, once, by the first);
// on failure both are refused so no PTY write lands on top of a durable
// marker the failed persist left set — without the shared discharge a second
// caller that bypassed the in-flight clear would, on the first's persist
// failure, write its bytes onto a pane the re-armed teardown would reap,
// losing the second caller's work even though its own delivery succeeded.
func (i *Instance) NoteAdoptionDelivery() error {
	i.mu.Lock()
	if i.adoption.closed {
		i.mu.Unlock()
		return ErrAdoptionFenced
	}
	i.adoption.deliveries++
	// A durable discharge is already in flight from a concurrent delivery:
	// share its result instead of proceeding to the PTY write. The in-memory
	// marker is gone (the discharging caller took it) and the durable marker
	// is what the discharging caller's persist is about to clear or leave
	// set; this caller's bytes depend on the same durable verdict, so wait
	// on the future and return its result. The count stays bumped: the
	// discharging caller's count and this caller's are both delivered before
	// either returns, so the teardown reads both through the fence.
	if i.discharge != nil {
		wait := i.discharge
		i.mu.Unlock()
		<-wait.done
		return wait.err
	}
	var notify func(*Instance) error
	var marker *PendingOnCompleteData
	var discharge *adoptionDischarge
	if i.owedOnComplete != nil {
		marker = i.owedOnComplete
		i.owedOnComplete = nil
		notify = i.owedOnCompleteNotify
		if notify != nil {
			discharge = &adoptionDischarge{done: make(chan struct{})}
			i.discharge = discharge
		}
	}
	i.touchLocked()
	i.mu.Unlock()
	if notify != nil {
		if err := notify(i); err != nil {
			// The durable discharge did not land. Restore the in-memory
			// marker so a re-attempted delivery re-runs the durable clear
			// rather than proceeding on top of a durable marker the failed
			// write left set. The delivery count stays bumped: the user's
			// intent to adopt stands, and it is the in-memory stand-down
			// signal a concurrent teardown would read through the fence
			// even before the discharge retries. The notify did not record
			// an in-memory settleOwed retry — that retry would race this
			// restore and write the restored marker back to disk — so the only
			// re-attempt is the caller's, which re-enters this function.
			//
			// Release any concurrent caller parked on this discharge's future
			// with the same error so its PTY write is also refused: the
			// durable marker this persist just failed to clear is the same
			// durable veto that would have reaped its panes after a restart.
			i.mu.Lock()
			if i.owedOnComplete == nil {
				i.owedOnComplete = marker
			}
			if i.discharge == discharge {
				discharge.err = err
				close(discharge.done)
				i.discharge = nil
			}
			i.touchLocked()
			i.mu.Unlock()
			return err
		}
		// The durable discharge landed. Release any concurrent caller parked
		// on the future with nil so its PTY write may proceed: the marker is
		// durably gone for both, once, by this notify.
		i.mu.Lock()
		if i.discharge == discharge {
			close(discharge.done)
			i.discharge = nil
		}
		i.touchLocked()
		i.mu.Unlock()
		return nil
	}
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

// OwedOnComplete returns the pending on_complete obligation filed for this
// session's finished run (#4162), or nil when none is owed. The in-memory form
// of InstanceData.PendingOnComplete.
func (i *Instance) OwedOnComplete() *PendingOnCompleteData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.owedOnComplete
}

// SetOwedOnComplete files or clears the obligation in memory. The daemon owns
// making that change durable: filing persists the marked row BEFORE the teardown
// wait begins, and clearing persists the discharge once a decision is reached. This
// call only moves memory.
func (i *Instance) SetOwedOnComplete(marker *PendingOnCompleteData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.owedOnComplete = marker
}

// FileOwedOnCompleteIfNotDischarged files the durable on_complete marker atomically
// under i.mu, but ONLY if no adoption delivery has landed since the run-end baseline
// was pinned. It is the paused-poll path's race fix (#4162's missing half):
//
//	deferTaskSessionLifecycleWhilePaused parks the in-memory intent under m.mu and
//	releases m.mu BEFORE calling fileOwedTaskLifecycle. The marker is filed here,
//	outside m.mu, so a TUI/browser keystroke that arrives in that window reaches
//	NoteAdoptionDelivery — which takes i.mu ALONE, not m.mu — between the unlock
//	and SetOwedOnComplete. NoteAdoptionDelivery sees owedOnComplete == nil and so
//	discharges nothing durably: it only bumps the in-memory delivery count.
//
// Without this helper, fileOwedTaskLifecycle then files a marker whose FiledAt
// postdates the keystroke. A restart before the next backstop poll wipes the
// in-memory count (the only evidence of that pre-filing delivery) while leaving
// the post-keystroke marker durable, so the unpaused drain finds no adoption
// evidence and authorizes a declared kill/archive against a session the user has
// just typed into — exactly the wrong-reap the durable marker exists to prevent.
//
// The helper closes that window by taking i.mu ONCE and refusing to file when
// adoption.deliveries has already advanced past adoption.atRunEnd. A delivery
// that arrives first leaves deliveries > atRunEnd, so the helper refuses and the
// unpaused drain stands down on the in-memory deliveries != atRunEnd check it
// already re-reads at drain time; if a restart wipes that in-memory check before
// the drain runs it ALSO wipes the (never-filed) marker, which is the pre-#4162
// shape the durable churn watermark exists to cover. A delivery that arrives
// after the helper has filed finds a real marker to clear, takes the existing
// NoteAdoptionDelivery path unchanged, and discharges durably.
//
// filed is true when the marker was set (and the caller must persist it) and
// false when the obligation is already discharged by a pre-filing delivery (and
// the caller must persist nothing).
func (i *Instance) FileOwedOnCompleteIfNotDischarged(marker *PendingOnCompleteData) (filed bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adoption.deliveries > i.adoption.atRunEnd {
		// A delivery landed in the pre-filing window: the in-memory count is
		// already the stand-down signal, and filing a marker whose FiledAt
		// postdates the keystroke is the very bug this method exists to close.
		// The marker stays nil; the caller persists nothing.
		i.owedOnComplete = nil
		return false
	}
	i.owedOnComplete = marker
	return true
}

// SetOwedOnCompleteNotify installs the callback invoked (outside i.mu) when a
// delivery discharges the obligation — the daemon's hook for making an
// adoption durable before the process can lose it. The callback returns any
// persist error so NoteAdoptionDelivery can propagate it and refuse the PTY
// write rather than proceed on top of a discharge that did not land. Not
// persisted itself: the daemon re-installs it wherever a marked row
// materializes.
func (i *Instance) SetOwedOnCompleteNotify(notify func(*Instance) error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.owedOnCompleteNotify = notify
}

// LastPaneChurnAt returns the most recent observed pane-output timestamp,
// which is durable (InstanceData.LastPaneChurnAt) — the adoption evidence that
// survives a restart where the in-memory delivery counter cannot.
func (i *Instance) LastPaneChurnAt() time.Time {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.lastPaneChurnAt
}

// ClaimOwedDrain marks the obligation's lifecycle worker in flight and reports
// whether this call won it. The marker can be re-armed while a worker is still
// waiting on adopted hooks — a refresh re-parks the intent and the next tick
// would otherwise launch a second teardown beside it. One worker per
// obligation, per generation: the claim is in-memory because a restarted
// daemon starts with none.
func (i *Instance) ClaimOwedDrain() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.owedDrainActive {
		return false
	}
	i.owedDrainActive = true
	return true
}

// ReleaseOwedDrain frees the worker claim. The worker's deferred release covers
// every exit — committed teardown, stand-down, timeout, and the shutdown stop —
// so a re-arm can only ever find a claim no worker still holds.
func (i *Instance) ReleaseOwedDrain() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.owedDrainActive = false
}
