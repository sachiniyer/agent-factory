package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

type resumeFromLimitOutcome uint8

const (
	resumeNotPerformed resumeFromLimitOutcome = iota
	resumePerformed
	// resumeTransferred is a committed-but-not-a-resume outcome: a pending
	// handoff mission was queued into the stored Prompt for the scheduled
	// limit retry — no prompt was delivered and no agent restarted. It is a
	// success (OK with a queue note), not the refusal that not-performed
	// reports (#6928 review MED).
	resumeTransferred
)

// The TUI and web reach this handler through apiclient/HTTP. The CLI reaches the
// same handler through daemon.ResumeFromLimit on the gob control socket; only
// the transport differs, while the controlServer and Manager action stay shared.

func (s *controlServer) ResumeFromLimit(req ResumeFromLimitRequest, resp *ResumeFromLimitResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	outcome, conversation, err := s.manager.resumeFromLimitOutcomeConversation(req)
	resp.OK = outcome == resumePerformed || outcome == resumeTransferred
	resp.Conversation = conversation
	if !resp.MutationOutcome.record(err) {
		return err
	}
	switch {
	case outcome == resumeTransferred:
		resp.Reason = "the pending mission was queued for the scheduled limit retry — it sends when the agent resumes"
	case !resp.OK:
		resp.Reason = "the session changed or another operation owns its retry"
	}
	return nil
}

// testHookResumeAfterFirstLock fires in resumeFromLimitOutcome immediately after
// the FIRST of its two locks is acquired, before the second. No-op in production;
// the #2006 ABBA regression test substitutes a barrier so it can pin one resume
// goroutine holding its first lock and force the cross-lock interleaving that the
// inverted order deadlocked on.
var testHookResumeAfterFirstLock = func() {}

// resumeFromLimitOutcome is the outcome-only view every existing caller and
// test holds: callers that need the retry's conversation outcome (#6768) — the
// control handler and the handoff wrapper — call
// resumeFromLimitOutcomeConversation instead.
func (m *Manager) resumeFromLimitOutcome(req ResumeFromLimitRequest) (resumeFromLimitOutcome, error) {
	outcome, _, err := m.resumeFromLimitOutcomeConversation(req)
	return outcome, err
}

func (m *Manager) resumeFromLimitOutcomeConversation(req ResumeFromLimitRequest) (resumeFromLimitOutcome, session.HandoffConversation, error) {
	// resolveActionSession, not findSession: id-first with a {title, repoID}
	// fallback, the same resolver kill/archive/restore and the tab verbs use. This
	// verb re-delivers a prompt INTO a pane, so resolving it by title alone would
	// let a duplicate title across repos type someone's prompt into an unrelated
	// agent — which is why the web, which holds stable ids, sends one (#1934).
	//
	// Every use below is the RESOLVED title rather than req.Title, which an
	// id-keyed request may leave empty.
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return resumeNotPerformed, session.HandoffConversation{}, err
	}
	if instance == nil {
		return resumeNotPerformed, session.HandoffConversation{}, fmt.Errorf("session %q not found", title)
	}
	// ResumeFromLimit is also the explicit recovery door for an ambiguous
	// agent-only handoff. The TUI c action, web Retry handoff button, and CLI
	// retry-limit command all reach this branch. Automatic recovery cannot: it
	// uses ResumePendingHandoffs and still requires PromptNotDelivered.
	if mission := instance.PendingHandoffMission(); mission != "" && instance.CanRetryPendingHandoffMissionDelivery() {
		key := daemonInstanceKey(repoID, instance.Title)
		handoffOutcome, retryErr := m.retryPendingHandoff(pendingHandoffEntry{
			repoID: repoID, key: key, instance: instance,
		}, mission, true)
		switch handoffOutcome {
		case handoffRetryPerformed:
			// A pending agent-handoff mission only exists where the handoff
			// replaced the program — the incoming agent's conversation is a
			// fresh one, resent mission or not (#6768).
			return resumePerformed, session.HandoffConversation{CarryFailure: crossAgentConversationReason}, retryErr
		case handoffRetryTransferred:
			// The mission moved into the stored Prompt for the scheduled
			// limit retry — committed, but nothing was sent or restarted.
			return resumeTransferred, session.HandoffConversation{}, retryErr
		default:
			return resumeNotPerformed, session.HandoffConversation{}, retryErr
		}
	}
	// A committed account swap whose post-commit respawn went blind
	// (ErrAccountSwapAgentTeardownBlind) is recoverable through the alreadySet
	// branches of resumeFromLimitLockedOutcome. That error path marks
	// StartupStateUnknown while preserving the pending swap, and
	// accountSwapResumeEligible gates on StartupStateUnknown first, so without
	// this short-circuit the gate would refuse every Retry with "not blocked on
	// a usage limit" before the committed-swap recovery branches could run. The
	// scheduler must still respect the "suppress automatic retries until
	// inspection" intent of the StartupStateUnknown marker, so the
	// committed-swap bypass lives here at the explicit RPC rather than inside
	// the shared predicate.
	if committedAccountSwap(instance) == nil && !accountSwapResumeEligible(instance) {
		return resumeNotPerformed, session.HandoffConversation{}, fmt.Errorf("session %q is not blocked on a usage limit", title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return resumeNotPerformed, session.HandoffConversation{}, nil
	}
	m.mu.Unlock()

	// Canonical lock order is target-before-op (#2006). DeliverPrompt holds the
	// per-target lock across the op lock it acquires inside SendPrompt, so every
	// path that needs both must take the target lock FIRST. Taking the op lock
	// first here — as this path used to — inverted that order, so a manual resume
	// overlapping a send-prompt (or the auto-resume scheduler) to the same session
	// deadlocked: each held one lock and blocked on the other. The op lock is still
	// only TryLock'd, so a resume never blocks behind a kill teardown that holds it.
	unlock := m.lockTarget(repoID, instance.Title)
	defer unlock()
	testHookResumeAfterFirstLock()

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return resumeNotPerformed, session.HandoffConversation{}, nil
	}
	defer opLock.Unlock()
	// No worktree admission here. The shared body takes it only around the
	// respawn — the one span that can rebuild a missing local worktree —
	// because every other phase persists through the repository start lock,
	// which sits LEFT of admission in the documented order (startLockForRepo).
	// Holding it across the whole request inverted that order against
	// CreateSession's start -> admission sequence (#6197 class, #6880), and on
	// this arm an admission timeout inside the body is still an untouched
	// refusal: nothing below can commit before the respawn runs.

	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return resumeNotPerformed, session.HandoffConversation{}, nil
	}

	return m.resumeFromLimitLockedOutcome(repoID, key, instance, title, committedAccountSwap(instance), true)
}
