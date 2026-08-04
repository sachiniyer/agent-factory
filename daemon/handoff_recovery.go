package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// A failed readiness capture must not turn the daemon poll into a hot loop.
// Handoff recovery is rare and the pending mission is durable, so a modest
// fixed delay is preferable to hammering a pane whose agent is still starting.
const pendingHandoffRetryDelay = 30 * time.Second

type pendingHandoffEntry struct {
	repoID   string
	key      string
	instance *session.Instance
}

// ResumePendingHandoffs retries takeover briefs that survived the post-swap
// checkpoint but not a confirmed prompt delivery. It is deliberately driven by
// the ordinary daemon poll. A crash-restored OpReplacing row stays hidden from
// status settlement and this path performs the readiness check itself; a legacy
// settled row waits for RefreshStatuses to provide LiveReady first. The same
// target-before-op locks as handoff/send-prompt prevent a recovery paste from
// racing a newer mutation.
//
// Every settlement below persists AND publishes (#2782). The events plane matters
// more here than on the interactive path, not less: no client asked for this work,
// so not even the window that started the handoff has a reason to re-Snapshot
// afterwards — and the status poll is precisely what cannot report it, because it
// skips a fenced row and then takes the settled state as its own baseline. Without
// the publish, a row this loop finishes stays "working" on every open rail until
// something unrelated republishes it.
func (m *Manager) ResumePendingHandoffs() {
	// Repair any settlement write that did not land before scanning for missions
	// to deliver. A stale obligation on disk is exactly what this pass would
	// replay after a restart, so retiring it is the same job as finishing one
	// (#2781).
	m.flushHandoffSettlements()

	m.mu.Lock()
	entries := make([]pendingHandoffEntry, 0, len(m.instances))
	for key, instance := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, pendingHandoffEntry{repoID: repoID, key: key, instance: instance})
	}
	m.mu.Unlock()

	for _, entry := range entries {
		mission := entry.instance.PendingHandoffMission()
		if mission == "" || entry.instance.UserKilled() || entry.instance.StartupStateUnknown() {
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
			continue
		}
		if err := m.resumePendingHandoff(entry, mission); err != nil {
			log.WarningLog.Printf("handoff %q: pending mission retry did not complete: %v", entry.instance.Title, err)
		}
	}
}

func (m *Manager) resumePendingHandoff(entry pendingHandoffEntry, mission string) error {
	unlock := m.lockTarget(entry.repoID, entry.instance.Title)
	defer unlock()

	opLock := m.opLockFor(entry.key)
	if !opLock.TryLock() {
		return nil
	}
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[entry.key]
	_, killing := m.killsInFlight[entry.key]
	m.mu.Unlock()
	op := entry.instance.GetInFlightOp()
	if killing || current != entry.instance || entry.instance.IsTearingDown() ||
		(op != session.OpNone && op != session.OpReplacing) ||
		entry.instance.PendingHandoffMission() != mission || entry.instance.UserKilled() || entry.instance.StartupStateUnknown() {
		return nil
	}

	switch entry.instance.GetLiveness() {
	case session.LiveLimitReached:
		// The incoming provider reached its wall before the crash-recovered
		// mission could land. Transfer the exact brief into the established limit
		// retry mechanism; manual/auto resume now owns it.
		entry.instance.SetPrompt(mission)
		if op == session.OpReplacing {
			resetAt, _ := entry.instance.LimitResetAt()
			if err := entry.instance.Transition(session.ParkHandoff(resetAt)); err != nil {
				return err
			}
		}
		if !entry.instance.ClearPendingHandoffMission(mission) {
			return fmt.Errorf("pending mission changed while transferring it to limit retry")
		}
		perr := m.persistHandoffSettlement(entry.repoID, entry.key, entry.instance)
		m.clearPendingHandoffRetry(entry.repoID, entry.instance)
		return perr
	case session.LiveReady:
		// Positive readiness is the authorization to paste. LiveRunning is not:
		// startup output and an already-delivered mission both look Running, so
		// guessing from it would either lose the brief or duplicate it.
	default:
		if op == session.OpReplacing {
			break
		}
		return nil
	}

	if !m.pendingHandoffRetryAllowed(entry.repoID, entry.instance) {
		return nil
	}
	if err := task.WaitForReadyAndSendPrompt(context.Background(), entry.instance, mission); err != nil {
		var limitErr *task.LimitReachedError
		if errors.As(err, &limitErr) {
			entry.instance.SetPrompt(mission)
			if op == session.OpReplacing {
				if terr := entry.instance.Transition(session.ParkHandoff(limitErr.ResetAt)); terr != nil {
					return errors.Join(err, terr)
				}
			} else {
				entry.instance.SetLimitReached(limitErr.ResetAt)
			}
			if !entry.instance.ClearPendingHandoffMission(mission) {
				return fmt.Errorf("pending mission changed while parking its usage limit")
			}
			perr := m.persistHandoffSettlement(entry.repoID, entry.key, entry.instance)
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
			if perr != nil {
				return errors.Join(err, perr)
			}
			return nil
		}
		// Best-effort, unlike the settlement writes above (#2781): this raises a
		// suppression marker over a mission that was never delivered, so losing it
		// costs another recovery attempt, never a duplicate execution.
		if op == session.OpReplacing && errors.Is(err, task.ErrAgentReadiness) {
			entry.instance.MarkStartupStateUnknown()
			m.persistAndPublishInstance(entry.repoID, entry.instance)
			m.clearPendingHandoffRetry(entry.repoID, entry.instance)
		}
		return err
	}
	if op == session.OpReplacing {
		if err := entry.instance.Transition(session.CommitHandoff()); err != nil {
			return err
		}
	}
	if !entry.instance.ClearPendingHandoffMission(mission) {
		return fmt.Errorf("pending mission changed after delivery")
	}
	perr := m.persistHandoffSettlement(entry.repoID, entry.key, entry.instance)
	m.clearPendingHandoffRetry(entry.repoID, entry.instance)
	if perr != nil {
		return perr
	}
	log.InfoLog.Printf("handoff %q: delivered pending mission", entry.instance.Title)
	return nil
}

func (m *Manager) pendingHandoffRetryAllowed(repoID string, instance *session.Instance) bool {
	key := remoteLossKey(repoID, instance)
	now := nowFunc()
	m.mu.Lock()
	defer m.mu.Unlock()
	if due := m.handoffRetryDue[key]; now.Before(due) {
		return false
	}
	m.handoffRetryDue[key] = now.Add(pendingHandoffRetryDelay)
	return true
}

func (m *Manager) clearPendingHandoffRetry(repoID string, instance *session.Instance) {
	m.mu.Lock()
	delete(m.handoffRetryDue, remoteLossKey(repoID, instance))
	m.mu.Unlock()
}
