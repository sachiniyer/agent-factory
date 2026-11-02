package daemon

import (
	"context"
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

// sendReverifyWindow bounds the asynchronous re-verification a sent-unverified
// task send keeps (#6590). The bounded Confirm window exists to answer "did the
// agent take this send" within seconds; the poll-fed watch exists because the
// common miss — a queued draft waiting out a long-running turn — answers the
// same question minutes later. A turn beginning this far after the send can no
// longer be attributed to it, so the pending watch retires unfired and the
// recorded verdict stays sent-unverified. Package var so tests compress it.
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
}

// armUnverifiedSendAfter keeps a sent-unverified TASK send's evidence contract
// armed past the bounded Confirm window (#6590): the status poll feeds the
// send's bound watch, and a turn the agent demonstrably began after the send
// promotes the task's recorded verdict. Only task provenance arms — a manual
// send-prompt owns no task row its verdict could be promoted on — and a send
// already delivered, refused, or errored has a verdict there is nothing left
// to re-verify.
func (m *Manager) armUnverifiedSendAfter(repoID string, instance *session.Instance, taskID string, status session.PromptDeliveryStatus, sendErr error, c *task.SendConfirmation, attemptedAt time.Time) {
	if sendErr != nil || status != session.PromptSentUnverified || taskID == "" {
		return
	}
	m.armUnverifiedSendWatch(stableSessionKey(repoID, instance), instance, taskID, attemptedAt, c.ReverifyWatch())
}

// armUnverifiedSendWatch registers a sent-unverified task send for the poll-fed
// re-verification the bounded Confirm window could not finish (#6590). A task
// whose verdict is already delivered or never attempted never arms — the send
// either provably landed or provably did not.
func (m *Manager) armUnverifiedSendWatch(key string, instance *session.Instance, taskID string, attemptedAt time.Time, watch *task.TurnWatch) {
	if watch == nil || taskID == "" || attemptedAt.IsZero() {
		return
	}
	agent := instance.ResolvedAgent()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	byTask := m.unverifiedSendWatches[key]
	if byTask == nil {
		byTask = map[string]*unverifiedSendWatch{}
		m.unverifiedSendWatches[key] = byTask
	}
	// The task's run status carries its LATEST send's verdict, so a re-send
	// re-bounds the watch rather than stacking: only the newest unverified
	// send's evidence may promote, and the replaced send's verdict is already
	// off the record.
	byTask[taskID] = &unverifiedSendWatch{
		attemptedAt: attemptedAt,
		expiresAt:   attemptedAt.Add(sendReverifyWindow),
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
	var fired []string
	m.pausedMu.Lock()
	byTask := m.unverifiedSendWatches[key]
	for taskID, entry := range byTask {
		if !now.Before(entry.expiresAt) {
			// Past the window a turn can no longer be attributed to the send;
			// the watch retires unfired and the verdict stays sent-unverified.
			delete(byTask, taskID)
			continue
		}
		// A frame captured before the send cannot feed it — the same
		// timestamp fence noteTaskRunTurnEvidence applies to its own watch.
		if entry.attemptedAt.After(capturedAt) {
			continue
		}
		if entry.agent != agent {
			entry.agent = agent
			entry.watch = task.NewTurnWatch(agent, entry.attemptedAt)
		}
		if entry.watch.Observe(obs.Content, capturedAt) {
			fired = append(fired, taskID)
			delete(byTask, taskID)
		}
	}
	if len(byTask) == 0 {
		delete(m.unverifiedSendWatches, key)
	}
	m.pausedMu.Unlock()
	for _, taskID := range fired {
		m.promoteUnverifiedTaskSend(taskID, instance.Title)
	}
}

// promoteUnverifiedTaskSend applies a fired re-verification watch: the task's
// recorded sent-unverified verdict becomes sent. The compare-and-set IS the
// safety property — it re-reads the row under the store lock, so a newer
// outcome already on the record (another delivery's verdict, an error, a drop
// count) is never clobbered by a stale fire, and a fire for a verdict that was
// never recorded unverified promotes nothing (#6590).
func (m *Manager) promoteUnverifiedTaskSend(taskID, title string) {
	now := nowFunc()
	_, applied, err := task.UpdateTaskStatusIf(taskID, &now, taskSendStatus(session.PromptDelivered), func(cur task.Task) bool {
		return cur.LastRunStatus == TaskStatusSentUnverified
	})
	if err != nil {
		m.warn().Printf("task %s: could not promote its unverified delivery to %q: %v", taskID, title, err)
		return
	}
	if applied {
		m.info().Printf("task %s: sent-unverified delivery to %q promoted to sent; the agent's turn began after the send (#6590)", taskID, title)
	}
}

// sweepUnverifiedSendWatches drops re-verification watches whose session no
// longer exists and retires watches past their evidence window — the poll is
// the only feed, so a watch it can no longer reach is a permanent leak.
// Called once per sweep from RefreshStatuses beside sweepTaskRunTurnWatches;
// m.mu and pausedMu are taken separately, never nested.
func (m *Manager) sweepUnverifiedSendWatches() {
	m.pausedMu.Lock()
	size := len(m.unverifiedSendWatches)
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
}
