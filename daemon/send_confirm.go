package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// beginSendConfirmation captures the pre-send frame the #6474 re-check needs to
// exclude a turn that was already running. Call it before the send.
func beginSendConfirmation(instance *session.Instance) *task.SendConfirmation {
	return task.BeginInstanceSendConfirmation(context.Background(), instance)
}

// promoteUnverifiedSend is #6474: a delivery that landed but could not be proven
// at the Enter should not read as unconfirmed forever. A sent-unverified send is
// re-checked for a bounded window and promoted to the existing delivered status
// ONLY on positive evidence bound to this send — the agent's turn began after
// it. An emptied composer, a changed pane, or an agent that was already busy is
// not evidence, and any miss leaves sent-unverified untouched. Other verdicts
// and errors pass through. The same status feeds a task run's last_run_status.
func (m *Manager) promoteUnverifiedSend(instance *session.Instance, status session.PromptDeliveryStatus, sendErr error, c *task.SendConfirmation) session.PromptDeliveryStatus {
	if sendErr != nil || status != session.PromptSentUnverified || !c.Confirm(context.Background()) {
		return status
	}
	m.info().Printf("prompt to %q: sent-unverified promoted to delivered; the agent's turn began after the send (#6474)", instance.Title)
	return session.PromptDelivered
}

// sendAttemptClock is the clock SendPromptWithEvidence stamps its attempt with,
// also binding the confirmation's evidence boundary to that same instant and
// stamping it into `attemptedAt` for the asynchronous re-verification watch
// that a still-unverified task send may arm (#6590).
func sendAttemptClock(c *task.SendConfirmation, attemptedAt *time.Time) func() time.Time {
	return func() time.Time {
		at := nowFunc()
		*attemptedAt = at
		c.BindAttempt(at)
		return at
	}
}

// sendReverifyWindow bounds the asynchronous re-verification an unproven task
// send keeps — sent-unverified (#6590) or sent-unconfirmed (#6819). The bounded
// Confirm window exists to answer "did the agent take this send" within
// seconds; the poll-fed watch exists because the common miss — a queued draft
// waiting out a long-running turn — answers the same question minutes later.
// A turn beginning this far after the send can no longer be attributed to it,
// so the pending watch retires unfired and the recorded verdict stands.
// Package var so tests compress it.
var sendReverifyWindow = 10 * time.Minute

// unverifiedSendWatch is one task send awaiting the re-verification of its
// recorded verdict — the send's bound chrome watcher and the evidence window
// it may still fire inside. Guards under pausedMu alongside taskRunTurnWatches.
type unverifiedSendWatch struct {
	// attemptedAt is the send's own attempt instant — the TurnWatch boundary
	// and the frame-age fence — and expiresAt is its evidence window.
	attemptedAt time.Time
	expiresAt   time.Time
	// agent is the resolved agent whose chrome the watch reads; a mid-window
	// handoff rebuilds the watch on the same boundary rather than feed it
	// signatures its rules cannot read.
	agent string
	watch *task.TurnWatch
	// fired banks positive evidence: the send's verdict write follows the arm
	// by the caller's delivery-settle latency, so a watch that fired before
	// that write commits keeps retrying its promotion on later captures rather
	// than losing the evidence (#6592 review).
	fired bool
}

// reverifiableSendStatus is the set of send verdicts that record an UNPROVEN
// task delivery — sent-unverified, or could-not-confirm when the pane was not
// observable at the send at all (#6819). For either, the send's turn evidence
// may still arrive, so the re-verification watch stays armed on the same
// contract. A delivered, refused, or errored send has a verdict there is
// nothing left to re-verify.
func reverifiableSendStatus(status session.PromptDeliveryStatus) bool {
	return status == session.PromptSentUnverified || status == session.PromptCouldNotConfirm
}

// armUnverifiedSendAfter keeps an unproven TASK send's evidence contract armed
// past the bounded Confirm window (#6590, #6819): the status poll feeds the
// send's bound watch, and a turn the agent demonstrably began after the send
// promotes the task's recorded verdict. Only task provenance arms — a manual
// send-prompt owns no task row its verdict could be promoted on — and a send
// already delivered, refused, or errored has a verdict there is nothing left
// to re-verify.
func (m *Manager) armUnverifiedSendAfter(repoID string, instance *session.Instance, taskID string, status session.PromptDeliveryStatus, sendErr error, c *task.SendConfirmation, attemptedAt time.Time) {
	if sendErr != nil || !reverifiableSendStatus(status) || taskID == "" {
		return
	}
	m.armUnverifiedSendWatch(stableSessionKey(repoID, instance), instance, taskID, attemptedAt, c.ReverifyWatch())
}

// armUnverifiedSendWatch registers an unproven task send for the poll-fed
// re-verification the bounded Confirm window could not finish (#6590). A task
// whose verdict is already delivered or never attempted never arms — the send
// either provably landed or provably did not.
func (m *Manager) armUnverifiedSendWatch(key string, instance *session.Instance, taskID string, attemptedAt time.Time, watch *task.TurnWatch) {
	m.armUnverifiedSendWatchUntil(key, instance, taskID, attemptedAt, attemptedAt.Add(sendReverifyWindow), watch)
}

// armUnverifiedSendWatchUntil is armUnverifiedSendWatch with an explicit
// evidence deadline — the boot re-arm rebuilds on a boundary strictly later
// than the send's true attempt, so it must also expire strictly inside the
// true window rather than ride the extra slack.
func (m *Manager) armUnverifiedSendWatchUntil(key string, instance *session.Instance, taskID string, attemptedAt, expiresAt time.Time, watch *task.TurnWatch) {
	if watch == nil || taskID == "" || attemptedAt.IsZero() {
		return
	}
	agent := instance.ResolvedAgent()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	// The task's run status carries its LATEST send's verdict, so a re-send
	// supersedes every earlier pending watch for the task — on this session
	// and on any other the task was retargeted away from. A superseded
	// watch's banked evidence could only ever promote a verdict that is no
	// longer its send's, and the verdict-write slack cannot tell two sends
	// seconds apart (#6592 review).
	for otherKey, otherByTask := range m.unverifiedSendWatches {
		delete(otherByTask, taskID)
		if len(otherByTask) == 0 {
			delete(m.unverifiedSendWatches, otherKey)
		}
	}
	byTask := m.unverifiedSendWatches[key]
	if byTask == nil {
		byTask = map[string]*unverifiedSendWatch{}
		m.unverifiedSendWatches[key] = byTask
	}
	byTask[taskID] = &unverifiedSendWatch{
		attemptedAt: attemptedAt,
		expiresAt:   expiresAt,
		agent:       agent,
		watch:       watch,
	}
}

// noteUnverifiedSendEvidence feeds one pane capture through the session's
// pending re-verification watches (#6590) — the same evidence contract the
// bounded Confirm applied, folded over the poll's frames instead of a private
// re-check loop. A fired watch promotes its task's recorded verdict after the
// lock is released; nothing weaker — pane churn, an emptied composer, an
// unreadable capture, a pre-send frame, a turn the send predates — can fire
// it. Called beside noteTaskRunTurnEvidence on both poll paths.
func (m *Manager) noteUnverifiedSendEvidence(key string, instance *session.Instance, obs session.Observation, capturedAt time.Time) {
	if !obs.Captured {
		return
	}
	// ResolvedAgent, not Program: the pane runs what the last handoff or
	// override installed, and only that agent's chrome can prove the send's
	// turn. Read under i.mu BEFORE pausedMu — the send path and sweep take the
	// two separately and never nest them, same as the run watcher.
	agent := instance.ResolvedAgent()
	now := nowFunc()
	var retries []string
	retryAt := map[string]time.Time{}
	m.pausedMu.Lock()
	byTask := m.unverifiedSendWatches[key]
	for taskID, entry := range byTask {
		if !now.Before(entry.expiresAt) {
			// Past the window a turn can no longer be attributed to the send;
			// the watch retires unfired and the recorded verdict stands.
			delete(byTask, taskID)
			continue
		}
		if !entry.fired {
			// A frame captured before the send cannot feed it — the same
			// timestamp fence noteTaskRunTurnEvidence applies to its own watch.
			if entry.attemptedAt.After(capturedAt) {
				continue
			}
			if entry.agent != agent {
				entry.agent = agent
				entry.watch = task.NewTurnWatch(agent, entry.attemptedAt)
			}
			if !entry.watch.Observe(obs.Content, capturedAt) {
				continue
			}
			// Bank the evidence rather than deleting the entry: the send's
			// verdict may not be on the record yet — the arm precedes the
			// caller's delivery-settle write — so a promotion refused now must
			// retry on later captures, and the fired flag holds the evidence
			// until it applies, its task is removed (#7042), or the window
			// closes (#6592 review).
			entry.fired = true
		}
		retries = append(retries, taskID)
		retryAt[taskID] = entry.attemptedAt
	}
	if len(byTask) == 0 {
		delete(m.unverifiedSendWatches, key)
	}
	m.pausedMu.Unlock()
	for _, taskID := range retries {
		// A newer send for the task may have superseded this watch between
		// the feed and now — promote only while this send's banked evidence
		// is still the latest on the record (#6592 review).
		m.pausedMu.Lock()
		entry := m.unverifiedSendWatches[key][taskID]
		still := entry != nil && entry.fired && entry.attemptedAt.Equal(retryAt[taskID])
		m.pausedMu.Unlock()
		if !still {
			continue
		}
		outcome := m.promoteUnverifiedTaskSend(taskID, instance.Title, retryAt[taskID])
		if outcome == promotionRetry {
			continue
		}
		// Only the call that actually removes the watch reports a removed
		// task, so two poll paths racing on the same capture still log one
		// line per watch (#7042).
		if m.retireUnverifiedSendWatch(key, taskID, retryAt[taskID]) && outcome == promotionTaskGone {
			m.info().Printf("task %s: removed while its delivery to %q was being re-verified; the re-verification retires (#7042)", taskID, instance.Title)
		}
	}
}

// retireUnverifiedSendWatch removes the fired watch for exactly the send bound
// to attemptedAt and reports whether it did. A re-send may have replaced the
// entry since the promotion was attempted; that newer watch is left alone.
func (m *Manager) retireUnverifiedSendWatch(key, taskID string, attemptedAt time.Time) bool {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	byTask := m.unverifiedSendWatches[key]
	if byTask == nil {
		return false
	}
	removed := false
	if e := byTask[taskID]; e != nil && e.fired && e.attemptedAt.Equal(attemptedAt) {
		delete(byTask, taskID)
		removed = true
	}
	if len(byTask) == 0 {
		delete(m.unverifiedSendWatches, key)
	}
	return removed
}

// unverifiedVerdictWriteSlack bounds how far after a send's attempt instant
// its recorded verdict may have landed. The watcher stamps last_run_at as the
// delivery settles — the send's own duration plus the bounded Confirm window —
// so a verdict inside this slack is plausibly this send's, and one outside it
// belongs to another delivery.
var unverifiedVerdictWriteSlack = 90 * time.Second

// reverifiableRunStatus is the recorded last_run_status values whose send may
// still be proven after the fact — sent-unverified and sent-unconfirmed
// (#6819). Any other verdict is settled and the promotion compare-and-set must
// refuse it.
func reverifiableRunStatus(status string) bool {
	return status == TaskStatusSentUnverified || status == TaskStatusSentUnconfirmed
}

// unverifiedPromotion is what one promotion attempt leaves a fired watch with.
type unverifiedPromotion int

const (
	// promotionRetry: the compare-and-set refused — the send's verdict may not
	// be on the record yet (#6592 review) — or the store failed in a way that
	// says nothing about the task. The banked evidence waits for a later
	// capture.
	promotionRetry unverifiedPromotion = iota
	// promotionApplied: the verdict is promoted and the watch is done.
	promotionApplied
	// promotionTaskGone: the task no longer exists, so no retry can ever
	// apply and the watch retires (#7042).
	promotionTaskGone
)

// promoteUnverifiedTaskSend applies a fired re-verification watch: the task's
// recorded unproven verdict — sent-unverified or sent-unconfirmed — becomes
// sent. The compare-and-set IS the safety property — it re-reads the row under
// the store lock and promotes only the verdict THIS send produced: the status
// still reads unproven AND last_run_at sits within the verdict-write slack of
// this send's attempt. A newer outcome already on the row, a settled verdict,
// and — when the task was retargeted and a different session's delivery
// rewrote the record — a verdict stamped outside the slack are all refused
// rather than clobbered (#6590, #6592 review). last_run_at is passed nil: the
// promotion is not a new occurrence, and rewriting the stamp would hide missed
// fires from the overdue derivation.
//
// A task that no longer exists is told apart from every other store error by
// the store's typed *task.NotFoundError, never by its text: only that answer
// retires the watch, and it logs nothing here — the caller logs the retirement
// once. Any other error keeps the evidence banked and retries (#7042).
func (m *Manager) promoteUnverifiedTaskSend(taskID, title string, attemptedAt time.Time) unverifiedPromotion {
	var promoted string
	_, applied, err := task.UpdateTaskStatusIf(taskID, nil, taskSendStatus(session.PromptDelivered), func(cur task.Task) bool {
		if !reverifiableRunStatus(cur.LastRunStatus) || cur.LastRunAt == nil {
			return false
		}
		if cur.LastRunAt.Before(attemptedAt) ||
			cur.LastRunAt.After(attemptedAt.Add(unverifiedVerdictWriteSlack)) {
			return false
		}
		promoted = cur.LastRunStatus
		return true
	})
	if err != nil {
		var nf *task.NotFoundError
		if errors.As(err, &nf) && nf.ID == taskID {
			return promotionTaskGone
		}
		m.warn().Printf("task %s: could not promote its unproven delivery to %q: %v", taskID, title, err)
		return promotionRetry
	}
	if !applied {
		return promotionRetry
	}
	m.info().Printf("task %s: %s delivery to %q promoted to sent; the agent's turn began after the send (#6590, #6819)", taskID, promoted, title)
	return promotionApplied
}

// unverifiedSendWatchPending reports whether the session has a task send still
// re-verifying its recorded verdict — the paused backstop feeds it the way it
// feeds an in-flight run, so an attached pane's turn cannot expire the watch
// unanswered (#6592 review).
func (m *Manager) unverifiedSendWatchPending(key string) bool {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	return len(m.unverifiedSendWatches[key]) > 0
}

// sweepUnverifiedSendWatches drops re-verification watches whose session no
// longer exists and retires watches past their evidence window — the poll is
// the only feed, so a watch it can no longer reach is a permanent leak.
// Called once per sweep from RefreshStatuses beside sweepTaskRunTurnWatches;
// m.mu and pausedMu are taken separately, never nested.
func (m *Manager) sweepUnverifiedSendWatches() {
	// The watch is memory-only while its verdict row is durable: a restart
	// dropped every watch it was still feeding, so rows still inside their
	// window re-arm once here (#6590).
	m.sendReverifyRearmOnce.Do(m.rearmUnverifiedSendWatches)
	m.pausedMu.Lock()
	size := len(m.unverifiedSendWatches) + len(m.pendingSwapNoticeWatches)
	m.pausedMu.Unlock()
	if size == 0 {
		return // nothing armed: skip the instance walk in the common case
	}
	m.mu.Lock()
	live := make(map[string]struct{}, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		live[stableSessionKey(repoID, inst)] = struct{}{}
	}
	m.mu.Unlock()
	now := nowFunc()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	for key, byTask := range m.unverifiedSendWatches {
		if _, ok := live[key]; !ok {
			delete(m.unverifiedSendWatches, key)
			continue
		}
		for taskID, entry := range byTask {
			if !now.Before(entry.expiresAt) {
				delete(byTask, taskID)
			}
		}
		if len(byTask) == 0 {
			delete(m.unverifiedSendWatches, key)
		}
	}
	// The swap-notice watches share the poll feed (#6767): same liveness and
	// expiry hygiene. Lazy re-arm in notePendingSwapNoticeEvidence covers
	// restart, so no rearm pass is needed for these.
	for key, entry := range m.pendingSwapNoticeWatches {
		if _, ok := live[key]; !ok || !now.Before(entry.expiresAt) {
			delete(m.pendingSwapNoticeWatches, key)
		}
	}
}

// rearmUnverifiedSendWatches rebuilds re-verification after a restart for task
// sends whose durable verdict still reads unproven — sent-unverified or
// sent-unconfirmed — inside the evidence window (#6590, #6819). The rebuilt
// contract can only be stricter than the lost watch's, never weaker:
//
//   - the boundary is the verdict's own last_run_at — the write, which lands
//     within the verdict slack of the attempt, so it sits strictly AFTER the
//     send's true instant and can only reject turns the original watch would
//     have accepted, never accept ones it would have rejected;
//   - the window is bounded the same way — last_run_at + window − slack is
//     inside the send's true window, so no evidence the original watch could
//     not have used is ever fed.
//
// A row whose recorded target no longer exists, whose verdict moved on, or
// whose window already closed stays frozen — and a row that cannot name its
// delivery's session at all stays frozen too, because arming the task's
// current (possibly retargeted) config could attribute another session's
// turn to this send (#6590 review).
func (m *Manager) rearmUnverifiedSendWatches() {
	tasks, err := task.LoadTasks()
	if err != nil {
		m.warn().Printf("send re-verification: could not load task rows for the boot re-arm: %v", err)
		return
	}
	now := nowFunc()
	type pendingRearm struct {
		key      string
		inst     *session.Instance
		taskID   string
		boundary time.Time
		expires  time.Time
	}
	var rearms []pendingRearm
	m.mu.Lock()
	for _, t := range tasks {
		if !reverifiableRunStatus(t.LastRunStatus) || t.LastRunAt == nil {
			continue
		}
		boundary := *t.LastRunAt
		expires := boundary.Add(sendReverifyWindow - unverifiedVerdictWriteSlack)
		if !now.Before(expires) {
			continue
		}
		// The verdict's own identity, not the task's current config: a targeted
		// send records LastRunTarget, a created run's key IS the session's
		// instance ID. A row that records neither cannot prove which session
		// its send went to and stays unverified (#6590 review).
		var inst *session.Instance
		switch {
		case t.LastRunTarget != "":
			inst = m.instances[daemonInstanceKey(t.RepoID, t.LastRunTarget)]
		case t.LastRunID != "":
			for _, candidate := range m.instances {
				if candidate.ID == t.LastRunID {
					inst = candidate
					break
				}
			}
		}
		if inst == nil {
			continue
		}
		rearms = append(rearms, pendingRearm{
			key:      stableSessionKey(t.RepoID, inst),
			inst:     inst,
			taskID:   t.ID,
			boundary: boundary,
			expires:  expires,
		})
	}
	m.mu.Unlock()
	for _, r := range rearms {
		m.armUnverifiedSendWatchUntil(r.key, r.inst, r.taskID, r.boundary, r.expires,
			task.NewTurnWatch(r.inst.ResolvedAgent(), r.boundary))
	}
}
