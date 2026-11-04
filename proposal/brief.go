package proposal

import (
	"errors"
	"fmt"
	"time"
)

// The web's start and attach ask the daemon to brief the agent itself
// (#6755): the CLI route prints Rules() for the agent to read, but a session
// menu click never runs af inside the pane, so the contract has to arrive as
// a prompt. The debt is durable (refs/af/brief/<session>), so it survives a
// daemon restart and lands exactly once per attach — a repeated attach of the
// same slug reads the settled marker and pastes nothing, while a detach
// clears it and a re-attach is a fresh briefing, matching `af proposal
// attach` printing the rules on every call.
//
// Delivery rides the same guarded send path a proposal push uses: the inline
// attempt happens only when the session's pane can take input (ready, not
// paused by a client hold, agent alive), and an owed contract that could not
// go out immediately waits for the first PushTick where it can — ahead of any
// pending edit push, so the rules always reach the agent before its first
// "the user edited" prompt. A failed send keeps the debt and backs off
// PushQuiet; an unconfirmed send settles, because a paste that may have
// landed must never turn into a second copy.

// briefEntry is the in-memory half of a session's briefing: the durable
// marker says a contract is owed, this says whether a send is in flight,
// when a failed one may try again, when an unconfirmed one came back, and
// what the marker read last. unconfirmedAt seeds the push entry's own
// delivery guard, which exists on the push side — a briefing sent before
// the session had any edits predates its push entry entirely. The marker
// cache keeps an idle tick from launching a git subprocess per attached
// session; attach, detach, and every send attempt invalidate it. Losing the
// entry (a restart) is safe — the next tick reads the marker once.
type briefEntry struct {
	sending       bool
	notBefore     time.Time
	unconfirmedAt time.Time
	markerSet     bool
	markerSlug    string
	markerState   BriefState
	markerLoads   int
}

// withinUnconfirmed reports whether now is inside the quiet window an
// unconfirmed delivery opened at at — the window in which a further prompt
// could splice into the still-unsubmitted paste.
func withinUnconfirmed(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) < PushUnconfirmedTimeout
}

func (s *Service) briefEntryLocked(sessionID string) *briefEntry {
	e := s.briefs[sessionID]
	if e == nil {
		e = &briefEntry{}
		s.briefs[sessionID] = e
	}
	return e
}

// claimBrief takes the session's in-flight briefing slot. It returns false
// when a send is already running, so a request and a tick can never paste the
// contract twice.
func (s *Service) claimBrief(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.briefEntryLocked(sessionID)
	if e.sending {
		return false
	}
	e.sending = true
	return true
}

// invalidateBrief drops the session's cached briefing marker after an
// attach, detach, or recorded delivery changed what the ref holds, so the
// next owed check reads it once instead of trusting the stale copy.
func (s *Service) invalidateBrief(sessionID string) {
	s.mu.Lock()
	if be := s.briefs[sessionID]; be != nil {
		be.markerSet = false
	}
	s.mu.Unlock()
}

// briefGuarded reports whether the session's pane is inside an unconfirmed
// delivery's quiet window — whether the send that opened it was a contract
// or an edit push, and whichever proposal it was for. A further contract
// waits for the window to expire, the same way a further edit push does.
func (s *Service) briefGuarded(sessionID string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if be := s.briefs[sessionID]; be != nil && withinUnconfirmed(be.unconfirmedAt, now) {
		return true
	}
	if e := s.push[sessionID]; e != nil && withinUnconfirmed(e.unconfirmedAt, now) {
		return true
	}
	return false
}

// brief reports what a deliver_rules request did with the contract on info.
// The attach armed the durable debt in its own commit, so this only reads the
// marker back. It runs on the request's goroutine: the send is a pane write
// like a push route's, so a slow paste does not outlive the reply it belongs
// to.
func (s *Service) brief(r *resolved, slug string, info *Summary) {
	got, state, err := r.store.Brief(r.session.ID)
	switch {
	case err != nil || got != slug || state == "":
		errText := "the marker is missing"
		if err != nil {
			errText = err.Error()
		}
		info.RulesDelivery, info.RulesError = "failed", fmt.Sprintf("recording the contract owed to the agent: %s", errText)
	case state == BriefDone:
		// Already delivered for this attachment: a repeated attach of the same
		// proposal is a resync, not a new briefing.
		info.RulesDelivery = "delivered"
	case state == BriefUnconfirmed:
		// An earlier send may not have landed; it still counts as settled, but
		// a repeat attach repeats the honest answer rather than "delivered".
		info.RulesDelivery = "unconfirmed"
		info.RulesError = "the contract prompt may not have reached the agent on the earlier attach; it is not resent, to avoid a double paste"
	case r.session.NoAgent:
		info.RulesDelivery, info.RulesError = "held", "the session has no live agent yet: the contract is owed and will be delivered once it is back and idle"
	case r.session.Paused:
		info.RulesDelivery, info.RulesError = "held", "a client is holding the session's pane (a TUI attach or an unfinished prompt): the contract is owed and will be delivered once the pane is free"
	case !r.session.Ready:
		info.RulesDelivery, info.RulesError = "held", "the agent is busy: the contract is owed and will be delivered once it is idle"
	case s.briefGuarded(r.session.ID):
		info.RulesDelivery, info.RulesError = "held", "an earlier prompt to this pane is still unconfirmed: the contract is owed and goes out once the pane's quiet window expires"
	case !s.claimBrief(r.session.ID):
		info.RulesDelivery, info.RulesError = "held", "the contract is already being delivered"
	default:
		delivery, err := s.sendBrief(r.session.ID, false)
		switch {
		case delivery == DeliveryConfirmed:
			info.RulesDelivery = "delivered"
		case delivery == DeliveryUnconfirmed:
			info.RulesDelivery = "unconfirmed"
			info.RulesError = "the prompt was sent but the pane did not confirm it; the contract may still have landed"
			if err != nil {
				info.RulesError = fmt.Sprintf("the contract prompt may have landed but its send reported a problem (%v); to avoid a double paste it will not be retried", err)
			}
		default:
			info.RulesDelivery, info.RulesError = "failed", briefSendError(err)
		}
	}
}

func briefSendError(err error) string {
	if err != nil {
		return fmt.Sprintf("the contract prompt did not reach the agent (%v); it is still owed and will be retried", err)
	}
	return "the contract prompt did not reach the agent; it is still owed and will be retried"
}

// runBrief sends the session its owed contract. The caller holds the
// in-flight slot (claimBrief). A marker that is already settled or gone — a
// repeat attach's done state, a detach's cleared ref — sends nothing and
// reports confirmed, so a late-running send never revives a settled debt.
// The pane's state is rechecked here, at the send boundary, because a client
// hold or a busy spell can start after the caller picked this session;
// forced shares a user-requested send's override of the pane hold (Send now
// pushes past a TUI attach the same way).
func (s *Service) runBrief(sessionID string, forced bool) (Delivery, error) {
	r, err := s.sessionTarget(sessionID)
	if err != nil {
		return DeliveryFailed, err
	}
	slug, state, err := r.store.Brief(r.session.ID)
	if err != nil {
		return DeliveryFailed, err
	}
	if slug == "" || state != BriefOwed {
		return DeliveryConfirmed, nil
	}
	switch {
	case r.session.NoAgent:
		return DeliveryFailed, errors.New("the session has no live agent")
	case r.session.Paused && !forced:
		return DeliveryFailed, errors.New("a client holds the session's pane")
	case !r.session.Ready:
		return DeliveryFailed, errors.New("the agent is busy")
	}
	if s.briefGuarded(sessionID) {
		// The window can open between the claim and now — an edit push that
		// started before the debt existed can finish unconfirmed mid-flight.
		return DeliveryFailed, errors.New("an earlier prompt to this pane is still unconfirmed")
	}
	delivery, sendErr := s.host.SendPrompt(r.session.ID, Rules(slug, r.store.SlugPath(slug)))
	if delivery != DeliveryFailed {
		// Confirmed and unconfirmed alike settle the debt, whatever sendErr
		// reports: the paste may have landed, and a second copy is worse than
		// a lost one. The marker keeps the distinction for honest reporting.
		if err := r.store.MarkBriefDone(r.session.ID, slug, delivery); err != nil {
			return delivery, err
		}
	}
	return delivery, sendErr
}

// sendBrief releases the in-flight slot after runBrief and backs a failed
// send off PushQuiet, so the next tick retries instead of hammering a pane
// that just refused input.
func (s *Service) sendBrief(sessionID string, forced bool) (Delivery, error) {
	delivery, sendErr := s.runBrief(sessionID, forced)
	now := s.now()
	s.mu.Lock()
	if e := s.briefs[sessionID]; e != nil {
		e.sending = false
		// The attempt may have recorded a delivery or left the debt owed;
		// either way the cached marker is stale.
		e.markerSet = false
		if sendErr != nil || delivery == DeliveryFailed {
			e.notBefore = now.Add(PushQuiet)
		}
	}
	if delivery == DeliveryUnconfirmed {
		// The contract may still be sitting unsubmitted in the pane, so edit
		// prompts queue behind the same quiet window an unconfirmed push gets:
		// the next paste must not splice into it. The brief entry keeps the
		// timestamp for push entries created later — a contract sent before
		// the session had edits predates them.
		if e := s.briefs[sessionID]; e != nil {
			e.unconfirmedAt = now
		}
		if e := s.push[sessionID]; e != nil {
			e.unconfirmedAt = now
		}
	}
	s.mu.Unlock()
	return delivery, sendErr
}

// briefOwedLocked reports whether sessionID's briefing marker still owes the
// contract for slug — the debt that has to go out before any edit push may.
// It runs per attached session on every poll tick under s.mu, so the marker
// read — a git subprocess, two when the marker is missing — is cached on the
// brief entry and re-read only after an attach, detach, or send invalidated
// it. A failed read is never cached: the next tick retries rather than
// treating a transient error as a settled absence. A stale cache can only
// defer a send by one tick, never send the wrong contract: the cached slug
// must still match the live attachment.
func (s *Service) briefOwedLocked(store *Store, sessionID, slug string) bool {
	be := s.briefEntryLocked(sessionID)
	if !be.markerSet {
		be.markerLoads++
		got, state, err := store.Brief(sessionID)
		if err != nil {
			// A failed read is not an answer: leave the cache empty so the
			// next tick retries rather than remembering a transient problem
			// as "no debt" and stranding an owed contract.
			return false
		}
		be.markerSet, be.markerSlug, be.markerState = true, got, state
	}
	return be.markerSlug == slug && be.markerState == BriefOwed
}
