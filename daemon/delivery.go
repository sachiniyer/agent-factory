package daemon

// Prompt delivery: the daemon's serialized create-or-send path for a targeted
// delivery. Extracted from control.go (#1223): the delivery machinery is a
// self-contained unit — the per-target lock, existence check, wait-for-create
// retry, and its error classification — and keeping it here keeps control.go
// under its file-length ceiling.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// targetDeliverWait bounds how long DeliverPrompt waits for a target session to
// materialize after losing a creation race to a process outside this daemon
// (e.g. `af sessions create`); targetDeliverPoll is the retry cadence. The wait
// only matters on that cross-process path — in-daemon deliveries serialize on
// the per-target lock and never enter it.
var (
	targetDeliverWait = 30 * time.Second
	targetDeliverPoll = 100 * time.Millisecond
)

// testHookDeliverAfterTargetLock fires in DeliverPrompt immediately after the
// per-target lock is acquired, before the op lock is taken (inside SendPrompt).
// No-op in production; the #2006 ABBA regression test substitutes a signal so it
// can prove DeliverPrompt holds the target lock before releasing the resume
// goroutine — the ordering that used to deadlock. Mirrors the existing
// testHookPollBeforePublish seam.
var testHookDeliverAfterTargetLock = func() {}

// StatusDeferredAttached is the delivery status returned when an automated task
// delivery (DeferWhileAttached) targets a session a TUI is attached full-screen
// to (#1586). It extends the "started"/"sent" status vocabulary. It is
// deliberately NOT "errored:"-prefixed, so a cron task records a benign
// deferred run (not a failure) and re-fires on its next tick; the watch path
// converts it into errTargetBusy to re-queue and retry after detach.
const StatusDeferredAttached = "deferred: target attached"

// errTargetBusy signals that an automated task delivery was deferred because a
// TUI is attached full-screen to the target session (#1586). It never crosses
// the control socket — DeliverPrompt reports the deferral via the
// StatusDeferredAttached status string instead — so this sentinel is minted and
// matched entirely daemon-side, by the watch delivery path, to drive the
// durable re-queue/retry without tripping the delivery-failure alarm (#1238).
var errTargetBusy = errors.New("target session is attached; delivery deferred until detach")

// errNotAttempted marks a delivery failure that provably sent NOTHING: the
// attempt died on a pre-flight check, before any session was created and before
// any keystroke reached a pane. The watch paths refund the rate slot such an
// attempt reserved (#2102) — the rule is "refund iff nothing was delivered", so
// a deferral (errTargetBusy), a cap refusal (errAtConcurrencyLimit), and a
// pre-flight error are the same case, and only the deferrals used to refund.
//
// Deliberately NOT applied to a failed create or a failed send. Both cross the
// control socket, and delivery itself is keystrokes-then-submit, so their error
// can surface AFTER the session was created or the prompt was already pasted;
// the caller cannot distinguish "never sent" from "sent, then the reply was
// lost". Refunding an ambiguous failure would let a delivered event escape the
// per-minute budget — the exact pressure this limiter exists to bound — so
// ambiguity stays charged. Under-refunding costs a little throughput;
// over-refunding breaks the guarantee.
var errNotAttempted = errors.New("delivery not attempted")

// notAttempted tags err as a pre-flight failure that provably delivered nothing.
// The wrapper is TRANSPARENT — Error() is the wrapped message verbatim — so these
// errors reach `af sessions send-prompt` users and the broadcast error field with
// no internal token trailing a pasteable command (#2512). nil in, nil out.
func notAttempted(err error) error {
	if err == nil {
		return nil
	}
	return &notAttemptedError{err: err}
}

type notAttemptedError struct{ err error }

func (e *notAttemptedError) Error() string        { return e.err.Error() }
func (e *notAttemptedError) Unwrap() error        { return e.err }
func (e *notAttemptedError) Is(target error) bool { return target == errNotAttempted }

// notDeliveredMarker is the WIRE-VISIBLE phrase that lets a pre-flight failure be
// recognized after net/rpc flattens it to a plain string. The task delivery path
// reaches the manager over the daemon's OWN control socket (deliverPromptForTask
// is the control-client DeliverPrompt, taskrun.go), and net/rpc destroys the
// *notAttemptedError type and its Is() method — so the type alone cannot carry
// the classification back to the watcher (that is why the first cut of #2501 did
// not actually refund). Same idiom, same reason, as atConcurrencyLimitErrText
// (#1892) — but chosen to be NATURAL user-facing text that already belongs in
// these messages ("... prompt not delivered"), NOT an appended token, so the wire
// survivability costs the user nothing. Every pre-flight message reachable from
// the watch path carries it; deliverTaskPrompt re-mints the sentinel on a match.
const notDeliveredMarker = "prompt not delivered"

// isNotAttemptedErr reports whether err — INCLUDING one flattened by net/rpc on
// its way back from the daemon — is a pre-flight not-attempted failure. The
// phrase arm is what survives the RPC round trip; the type arm covers in-process
// callers. The phrase is absent from the ambiguous send/create failures (which
// say "failed to send prompt" / "failed to auto-create"), so it never over-refunds.
func isNotAttemptedErr(err error) bool {
	return err != nil && (errors.Is(err, errNotAttempted) || strings.Contains(err.Error(), notDeliveredMarker))
}

// deferWhileAttached reports whether an automated delivery must be held because a
// TUI is attached full-screen to the target (#1586). Every SendPrompt delivery
// path routes through this: the fast "exists" path AND both wait-then-send paths
// (the concurrent-create retry here and the re-emerging-root path in
// rootagent.go). A TUI can attach during either wait — PauseStatusPoll leases by
// (repoID, title) even before the session exists — so all three must re-check the
// lease right before sending, or an automated prompt pastes into the pane the
// user is typing in (#1638). Only automated deliveries set DeferWhileAttached; a
// manual send-prompt is an explicit user action and still lands immediately.
func (m *Manager) deferWhileAttached(repoID string, req DeliverPromptRequest) bool {
	if !req.DeferWhileAttached {
		return false
	}
	// DeliverPrompt addresses the row by title, but the pause lease belongs to
	// its stable identity. Resolve only the already-tracked row here; an absent
	// row cannot be attached, while legacy ID-less pauses still use the title
	// fallback inside isPollPaused.
	m.mu.Lock()
	instance := m.instances[daemonInstanceKey(repoID, req.Title)]
	id := ""
	if instance != nil {
		id = instance.ID
	}
	m.mu.Unlock()
	return m.isPollPaused(repoID, req.Title, id)
}

// DeliverPrompt delivers a prompt to a target session, auto-creating that
// session when it does not exist. The whole create-or-send decision runs under
// a per-(repo, title) lock, so concurrent deliveries to the same shared target
// serialize: the first creates the session, the rest send into it in arrival
// order. This is the fix for #865, where the pre-lock path let two deliveries
// both observe "missing", both attempt creation, and dropped the loser's prompt
// when reserveCreate rejected the duplicate. Returns "started" when this call
// created the session and "sent" when it delivered into an existing one.
func (m *Manager) DeliverPrompt(req DeliverPromptRequest) (string, error) {
	// These all fail before any create or send — nothing was delivered — so they
	// are pre-flight and tagged notAttempted so the watch path refunds the rate
	// slot (#2501). RepoFromPath in particular is watch-reachable: a momentarily
	// unresolvable project during an outage must not burn budget.
	if req.Prompt == "" {
		return "", notAttempted(fmt.Errorf("prompt is required"))
	}
	if req.RepoPath == "" {
		return "", notAttempted(fmt.Errorf("repo path is required"))
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		// Carry notDeliveredMarker so this is refundable across the RPC hop (#2501):
		// a momentarily unresolvable project during an outage must not burn budget.
		return "", notAttempted(fmt.Errorf("%w; %s", err, notDeliveredMarker))
	}

	unlock := m.lockTarget(repo.ID, req.Title)
	defer unlock()
	testHookDeliverAfterTargetLock()

	exists, deleting, liveness, err := m.targetSessionState(repo.ID, req.Title)
	if err != nil {
		// A pre-flight state refresh that failed sent nothing (#2501): the watch
		// path must refund the rate slot this attempt reserved, not charge it.
		return "", notAttempted(fmt.Errorf("could not check target session %q state; %s: %w", req.Title, notDeliveredMarker, err))
	}
	if deleting {
		return "", notAttempted(fmt.Errorf("target session %q is being deleted; %s", req.Title, notDeliveredMarker))
	}
	if err := promptTargetLivenessError(req.Title, liveness); err != nil {
		return "", notAttempted(err)
	}
	if exists {
		// A TUI is attached full-screen to this session (#1160 pause lease), so
		// the user is typing directly into its pane. Pasting an automated task
		// prompt + Enter now would append to and submit their half-typed line
		// (#1586). Defer instead of delivering: the caller holds the event (watch
		// re-queues and retries after detach; cron records the benign deferred
		// status and re-fires next tick) rather than corrupting live input. Only
		// automated deliveries set DeferWhileAttached — a manual send-prompt is an
		// explicit user action and still lands immediately.
		if m.deferWhileAttached(repo.ID, req) {
			return StatusDeferredAttached, nil
		}
		if err := m.SendPrompt(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt}); err != nil {
			return "", err
		}
		return "sent", nil
	}

	// If the absent target is this repo's daemon-managed root agent — only
	// momentarily gone while the ensure loop re-materializes it — wait for it to
	// return and send into it, rather than falling through to auto-create (which
	// the reserved-name guard would reject, dropping the event with a misleading
	// "pick another name" error; #1223).
	if status, handled, rerr := m.deliverToReemergingRoot(repo, req); handled {
		return status, rerr
	}

	// The session is absent and, because deliveries to this target serialize on
	// the per-target lock, no other in-daemon delivery is creating it. Create it
	// now and deliver the prompt as its initial prompt.
	created, err := m.CreateSession(context.Background(), CreateSessionRequest{
		Title:    req.Title,
		RepoPath: req.RepoPath,
		Program:  req.Program,
		Prompt:   req.Prompt,
	})
	if err != nil {
		// A creator outside this daemon (a plain `af sessions create`, the API)
		// can still claim the title between our check and reserveCreate. Rather
		// than drop the prompt (#865), wait for the session to materialize and
		// send into it. Genuine conflicts (branch collisions, config errors)
		// are not retryable and surface as-is.
		if isConcurrentCreateErr(err) {
			if werr := m.waitForTargetSession(repo.ID, req.Title); werr != nil {
				// The target never materialized within the wait (its liveness went
				// bad, it was deleted, or targetDeliverWait elapsed): nothing was
				// sent, so this is pre-flight and must refund (#2501). This is the
				// outage path — a 30s wait that times out repeatedly would otherwise
				// drain the budget the recovery needs.
				return "", notAttempted(werr)
			}
			// A TUI can attach during the wait above, so re-check the defer lease
			// before sending — otherwise this path pastes into an attached pane the
			// "exists" path would have deferred (#1638).
			if m.deferWhileAttached(repo.ID, req) {
				return StatusDeferredAttached, nil
			}
			if serr := m.SendPrompt(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt}); serr != nil {
				return "", serr
			}
			return "sent", nil
		}
		return "", fmt.Errorf("failed to auto-create target session %q: %w", req.Title, err)
	}
	return createdTaskStatus(created), nil
}

// lockTarget acquires the per-(repo, title) delivery lock, creating it on first
// use, and returns the unlock function. Mirrors startLockForRepo: the map is
// guarded by m.mu but the returned mutex is held outside it, so a long-running
// delivery never blocks unrelated manager operations.
func (m *Manager) lockTarget(repoID, title string) func() {
	m.mu.Lock()
	key := daemonInstanceKey(repoID, title)
	lock := m.targetLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.targetLocks[key] = lock
	}
	m.mu.Unlock()

	lock.Lock()
	return lock.Unlock
}

// targetSessionState reports whether a session with the given title exists for
// the repo (in memory or persisted), whether it is mid-teardown, and the live
// daemon instance's liveness when one is tracked. Deleting is transient
// in-memory state that is never persisted (#844/#847); the daemon's KillSession
// path records it in killsInFlight, while TUI-initiated teardown is reflected on
// the live instance as OpKilling.
func (m *Manager) targetSessionState(repoID, title string) (exists, deleting bool, liveness session.Liveness, err error) {
	m.mu.Lock()
	if rerr := m.refreshLocked(); rerr != nil {
		m.mu.Unlock()
		return false, false, session.LivenessUnset, rerr
	}
	key := daemonInstanceKey(repoID, title)
	inst := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return true, true, session.LivenessUnset, nil
	}
	if inst != nil {
		return true, inst.IsTearingDown(), inst.GetLiveness(), nil
	}

	exists, err = repoHasSessionTitle(repoID, title)
	return exists, false, session.LivenessUnset, err
}

// waitForTargetSession blocks until the target session exists, surfacing
// undeliverable liveness states rather than delivering into them, bounded by
// targetDeliverWait.
func (m *Manager) waitForTargetSession(repoID, title string) error {
	deadline := time.Now().Add(targetDeliverWait)
	for {
		exists, deleting, liveness, err := m.targetSessionState(repoID, title)
		if err != nil {
			// Carry notDeliveredMarker on EVERY error path so a wait that ends
			// without delivering is refundable across the RPC hop (#2501). The
			// deleting/liveness branches already say "prompt not delivered"; the
			// state-refresh and timeout paths must too, since the timeout is the
			// outage case a root-targeted watch task hits.
			return fmt.Errorf("could not check target session %q state; %s: %w", title, notDeliveredMarker, err)
		}
		if deleting {
			return fmt.Errorf("target session %q is being deleted; %s", title, notDeliveredMarker)
		}
		if err := promptTargetLivenessError(title, liveness); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for target session %q to be created; %s", title, notDeliveredMarker)
		}
		time.Sleep(targetDeliverPoll)
	}
}

// errConcurrentCreate marks the retryable race in #865: another creator already
// claimed the exact title between DeliverPrompt's existence check and its
// reserveCreate, so the session will materialize shortly and waiting-then-sending
// is correct. Only the genuine in-flight reservation/record rejections wrap it
// (see validateTitleAvailableLocked and appendInstanceData). Terminal conflicts
// — a tmux orphan with no daemon/disk record (#916), a branch collision, or a
// remote hook-name clash — stay plain so DeliverPrompt surfaces them immediately
// instead of waiting out waitForTargetSession's timeout.
var errConcurrentCreate = errors.New("concurrent create in progress")

// isConcurrentCreateErr reports whether a CreateSession failure is the retryable
// concurrent-create race in #865. Substring matching on "already exists" used to
// also catch the tmux-orphan rejection (#916), which is terminal and would never
// resolve by waiting; classification now keys off the errConcurrentCreate
// sentinel so only genuinely-retryable rejections trigger wait-then-send.
func isConcurrentCreateErr(err error) bool {
	return errors.Is(err, errConcurrentCreate)
}
