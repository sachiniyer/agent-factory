package proposal

import (
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
// marker says a contract is owed, this says whether a send is in flight and
// when a failed one may try again. Losing it (a restart) is safe — the marker
// re-derives it.
type briefEntry struct {
	sending   bool
	notBefore time.Time
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

// brief reports what a deliver_rules request did with the contract on info,
// arming the durable debt first. It runs on the request's goroutine: the send
// is a pane write like a push route's, so a slow paste does not outlive the
// reply it belongs to.
func (s *Service) brief(r *resolved, slug string, info *Summary) {
	state, err := r.store.OweBrief(r.session.ID, slug)
	switch {
	case err != nil:
		info.RulesDelivery, info.RulesError = "failed", fmt.Sprintf("recording the contract owed to the agent: %v", err)
	case state == BriefDone:
		// Already delivered for this attachment: a repeated attach of the same
		// proposal is a resync, not a new briefing.
		info.RulesDelivery = "delivered"
	case r.session.NoAgent:
		info.RulesDelivery, info.RulesError = "held", "the session has no live agent yet: the contract is owed and will be delivered once it is back and idle"
	case r.session.Paused:
		info.RulesDelivery, info.RulesError = "held", "a client is holding the session's pane (a TUI attach or an unfinished prompt): the contract is owed and will be delivered once the pane is free"
	case !r.session.Ready:
		info.RulesDelivery, info.RulesError = "held", "the agent is busy: the contract is owed and will be delivered once it is idle"
	case !s.claimBrief(r.session.ID):
		info.RulesDelivery, info.RulesError = "held", "the contract is already being delivered"
	default:
		delivery, err := s.sendBrief(r.session.ID)
		switch {
		case err == nil && delivery == DeliveryConfirmed:
			info.RulesDelivery = "delivered"
		case err == nil && delivery == DeliveryUnconfirmed:
			info.RulesDelivery = "unconfirmed"
			info.RulesError = "the prompt was sent but the pane did not confirm it; the contract may still have landed"
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
func (s *Service) runBrief(sessionID string) (Delivery, error) {
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
	delivery, sendErr := s.host.SendPrompt(r.session.ID, Rules(slug, r.store.SlugPath(slug)))
	if sendErr == nil && delivery != DeliveryFailed {
		// Confirmed and unconfirmed alike settle the debt: the unconfirmed paste
		// may have landed, and a second copy is worse than a lost one.
		if err := r.store.MarkBriefDone(r.session.ID, slug); err != nil {
			return delivery, err
		}
	}
	return delivery, sendErr
}

// sendBrief releases the in-flight slot after runBrief and backs a failed
// send off PushQuiet, so the next tick retries instead of hammering a pane
// that just refused input.
func (s *Service) sendBrief(sessionID string) (Delivery, error) {
	delivery, sendErr := s.runBrief(sessionID)
	now := s.now()
	s.mu.Lock()
	if e := s.briefs[sessionID]; e != nil {
		e.sending = false
		if sendErr != nil || delivery == DeliveryFailed {
			e.notBefore = now.Add(PushQuiet)
		}
	}
	s.mu.Unlock()
	return delivery, sendErr
}

// briefOwedLocked reports whether sessionID's briefing marker still owes the
// contract for slug — the debt that has to go out before any edit push may.
func (s *Service) briefOwedLocked(store *Store, sessionID, slug string) bool {
	got, state, err := store.Brief(sessionID)
	return err == nil && got == slug && state == BriefOwed
}
