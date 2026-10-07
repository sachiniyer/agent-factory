package daemon

import (
	"fmt"
)

type resumeFromLimitOutcome uint8

const (
	resumeNotPerformed resumeFromLimitOutcome = iota
	resumePerformed
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
	outcome, err := s.manager.resumeFromLimitOutcome(req)
	resp.OK = outcome == resumePerformed
	if !resp.MutationOutcome.record(err) {
		return err
	}
	if !resp.OK {
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

func (m *Manager) resumeFromLimitOutcome(req ResumeFromLimitRequest) (resumeFromLimitOutcome, error) {
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
		return resumeNotPerformed, err
	}
	if instance == nil {
		return resumeNotPerformed, fmt.Errorf("session %q not found", title)
	}
	// ResumeFromLimit is also the explicit recovery door for an ambiguous
	// agent-only handoff. The TUI c action, web Retry handoff button, and CLI
	// retry-limit command all reach this branch. Automatic recovery cannot: it
	// uses ResumePendingHandoffs and still requires PromptNotDelivered.
	if mission := instance.PendingHandoffMission(); mission != "" && instance.CanRetryPendingHandoffMissionDelivery() {
		key := daemonInstanceKey(repoID, instance.Title)
		performed, retryErr := m.retryPendingHandoff(pendingHandoffEntry{
			repoID: repoID, key: key, instance: instance,
		}, mission, true)
		if performed {
			return resumePerformed, retryErr
		}
		return resumeNotPerformed, retryErr
	}
	if !accountSwapResumeEligible(instance) {
		return resumeNotPerformed, fmt.Errorf("session %q is not blocked on a usage limit", title)
	}

	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return resumeNotPerformed, nil
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
		return resumeNotPerformed, nil
	}
	defer opLock.Unlock()
	worktreeAdmission, err := m.lockLocalWorktreeAdmissionWithin(repoID, title, "resume", instance)
	if err != nil {
		return resumeNotPerformed, err
	}
	defer unlockWorktreeAdmission(worktreeAdmission)

	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return resumeNotPerformed, nil
	}

	return m.resumeFromLimitLockedOutcome(repoID, key, instance, title, committedAccountSwap(instance))
}
