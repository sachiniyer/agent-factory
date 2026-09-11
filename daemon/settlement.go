package daemon

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// settleOwedEntry identifies a session whose settlement write did not land, so
// the poll can retry it. Keyed elsewhere by stable instance identity; the pointer
// is what proves the row has not been replaced since.
type settleOwedEntry struct {
	repoID   string
	key      string
	instance *session.Instance
}

// A SETTLEMENT is the write that records the outcome of an irreversible step —
// the class of persist in this package that may not be best-effort.
//
// persistInstance's contract is that a dropped write is a checkpoint "the next
// poll/tick will re-attempt". That holds for status, and only for status: the
// poll's change detection (persistPollChange) covers liveness and the limit reset
// time and nothing else. Any other fact a settlement carries is invisible to it,
// so a lost write survives every later writer and is repaired only by the
// whole-state shutdown checkpoint — which the unclean exit that makes the
// divergence matter is exactly what skips.
//
// What that costs depends on the fact:
//
//   - a handoff's PendingHandoffMission is a standing instruction, so losing its
//     clear leaves a stale replacement fence. Mission-scoped ambiguity now blocks
//     automatic replay, but an operator could still retry work the agent ran;
//   - a recovery's branchCreatedByUs says af created this branch and may delete
//     it, so losing the flip leaves an af-* branch nothing will ever clean up
//     (#2883, and the outcome #1841 named).
//
// So settlement writes are durable AND retried: the failure reaches the caller
// instead of a log line, and the row joins a retry set the poll drains until the
// write lands. It announces like every other committed change (#2782) — memory
// has already moved, whether or not disk agreed yet.
func (m *Manager) persistSettlement(repoID, key string, instance *session.Instance) error {
	// Bookkeeping belongs to the SAME repo-ordered critical section as the write.
	// If it happened after unlock, an older successful checkpoint could resume
	// late and erase the retry obligation from a newer failed settlement.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	m.publishEvent(agentproto.EventSessionUpdated, data)
	m.recordSettlementWrite(repoID, key, instance, err)
	repoStartLock.Unlock()
	if err != nil {
		return fmt.Errorf(
			"the settled state for %q could not be written to disk "+
				"(the daemon retries it on its poll; an unclean exit before it lands would lose this outcome): %w",
			instance.Title, err)
	}
	return nil
}

// prepareRuntimeReplacement retires facts owned by the predecessor at the
// replacement's ConfirmLive boundary. That includes classifying a task run whose
// prompted runtime was Lost: the replacement is not given the prompt, so the run
// closes as interrupted here instead of being misclassified as complete on its
// first idle observation. Production Recover and Respawn implementations confirm
// the fresh runtime live before returning, so clearing and persisting only after
// they return leaves a crash window: restart sees the new process, classifies it
// as a reattach, and reloads predecessor evidence.
//
// The ordinary post-operation noteRuntimeReplaced call remains necessary. A
// slow remote replacement can accumulate fresh transport observations after
// this boundary reset and before it returns; the post-success reset retires
// those in-memory observations, while this settlement makes the fence safe.
func (m *Manager) prepareRuntimeReplacement(repoID, key string, instance *session.Instance) error {
	run, interrupted := instance.InterruptTaskRunAtRestoreBoundary()
	m.noteRuntimeReplaced(repoID, instance)
	settlementErr := m.persistSettlement(repoID, key, instance)
	// The session settlement comes first. A task-file lock or disk fault must not
	// keep predecessor-owned remote-loss evidence live after the replacement is
	// already running, or a restart plus one blip could re-provision it again.
	if interrupted && run.TaskID != "" {
		m.recordInterruptedTaskRun(run)
	}
	if settlementErr != nil {
		return fmt.Errorf("the predecessor runtime could not be retired before its replacement became live: %w", settlementErr)
	}
	return nil
}

// recordInterruptedTaskRun makes a Lost task runtime's outcome visible at the
// replacement boundary. The live-boundary callback runs before ConfirmLive, so
// OpRestoring + TaskRunActive identifies the exact case: the departed runtime
// received the prompt, while the replacement did not. Limit resumes arrive as
// OpRespawning and are excluded because they re-deliver their queued prompt.
//
// A current record carries an exact run timestamp minted before publication.
// The outcome writer can therefore close the small race where recovery observes
// the session before its delivery caller records "started", while refusing any
// later timestamp. A pre-field record takes the conservative compatibility path
// below: it claims the current task timestamp only when it postdates this
// session's creation and no known session or pending create can be its successor,
// then uses an exact CAS so a concurrent delivery still wins.
func (m *Manager) recordInterruptedTaskRun(run session.TaskRunIdentity) {
	var (
		updated task.Task
		applied bool
		err     error
	)
	if run.RunAt.IsZero() {
		var legacyRunAt time.Time
		legacyRunAt, applied, err = m.legacyTaskRunAt(run)
		if err == nil && applied {
			updated, applied, err = task.UpdateTaskStatusIfLastRunAt(run.TaskID, legacyRunAt, TaskStatusInterrupted)
		}
	} else {
		updated, applied, err = task.UpdateTaskRunOutcome(run.TaskID, run.RunAt, TaskStatusInterrupted)
	}
	if err != nil {
		m.warn().Printf(
			"task %s: session %q lost the runtime that received its run prompt; restored it without replaying the prompt, skipped on_complete, and left the session in place for inspection; could not record last_run_status %q: %v",
			run.TaskID, run.Title, TaskStatusInterrupted, err)
		return
	}
	if !applied {
		m.warn().Printf(
			"task %s: session %q lost the runtime that received its run prompt; restored it without replaying the prompt, skipped on_complete, and left the session in place for inspection; did not replace last_run_status because the task row does not identify this run",
			run.TaskID, run.Title)
		return
	}
	m.publishEvent(agentproto.EventTaskUpdated, updated)
	m.warn().Printf(
		"task %s: session %q lost the runtime that received its run prompt; restored it without replaying the prompt, recorded last_run_status %q, skipped on_complete, and left the session in place for inspection",
		run.TaskID, run.Title, TaskStatusInterrupted)
}

func (m *Manager) legacyTaskRunAt(run session.TaskRunIdentity) (time.Time, bool, error) {
	m.mu.Lock()
	for _, candidate := range m.instances {
		if candidate.ID != run.SessionID && candidate.TaskID == run.TaskID &&
			!candidate.CreatedAt.Before(run.CreatedAt) {
			m.mu.Unlock()
			return time.Time{}, false, nil
		}
	}
	for _, candidate := range m.pendingCreates {
		if candidate.ID != run.SessionID && candidate.TaskID == run.TaskID &&
			!candidate.CreatedAt.Before(run.CreatedAt) {
			m.mu.Unlock()
			return time.Time{}, false, nil
		}
	}
	m.mu.Unlock()

	stored, err := task.GetTask(run.TaskID)
	if err != nil {
		return time.Time{}, false, err
	}
	// A pre-field session was published as "started". Any other status is an
	// explicit outcome owned by another writer, and missing legacy identity is
	// not authority to reopen it. Normal completion historically left "started"
	// in place, so this cannot identify every departed successor; that ambiguity
	// is irreducible for records that never stored a run identity.
	if stored.LastRunAt == nil || stored.LastRunAt.Before(run.CreatedAt) || stored.LastRunStatus != "started" {
		return time.Time{}, false, nil
	}
	return *stored.LastRunAt, true, nil
}

func (m *Manager) persistRuntimeReplacement(repoID, title string, instance *session.Instance) {
	if err := m.persistSettlement(repoID, daemonInstanceKey(repoID, title), instance); err != nil {
		m.warn().Printf("restored remote session %q with predecessor evidence cleared in memory but not yet on disk: %v", title, err)
	}
}

// recordSettlementWrite keeps a failed whole-row write eligible for poll retry,
// or retires an older obligation when a later whole-row write subsumes it. Every
// production caller invokes it before releasing the repo start lock that ordered
// the corresponding write, so completion scheduling cannot invert those facts.
func (m *Manager) recordSettlementWrite(repoID, key string, instance *session.Instance, err error) {
	owedKey := stableSessionKey(repoID, instance)
	m.mu.Lock()
	if err != nil {
		if m.settleOwed == nil {
			m.settleOwed = make(map[string]settleOwedEntry)
		}
		m.settleOwed[owedKey] = settleOwedEntry{repoID: repoID, key: key, instance: instance}
	} else {
		delete(m.settleOwed, owedKey)
	}
	m.mu.Unlock()
}

// FlushOwedSettlements retries settlement writes that did not land, so a
// transient failure self-heals within a poll tick instead of leaving the next
// daemon to load an outcome that never happened. Driven from the daemon poll, and
// a no-op in the overwhelmingly common case where no write ever failed.
func (m *Manager) FlushOwedSettlements() {
	m.mu.Lock()
	owed := make([]settleOwedEntry, 0, len(m.settleOwed))
	for _, entry := range m.settleOwed {
		owed = append(owed, entry)
	}
	m.mu.Unlock()

	for _, entry := range owed {
		m.mu.Lock()
		registered := m.instances[entry.key] == entry.instance
		if !registered {
			delete(m.settleOwed, stableSessionKey(entry.repoID, entry.instance))
		}
		m.mu.Unlock()
		// The row is gone (killed) or a successor took its key. Its record is being
		// deleted or belongs to another instance now, so writing this snapshot back
		// would fight whatever removed it — and there is nothing left to settle
		// either way, because a tombstone outranks every other marker on load
		// (session.FromInstanceData).
		if !registered {
			continue
		}
		if err := m.flushOneOwedSettlement(entry); err != nil {
			m.warn().Printf("settlement retry for %q: %v", entry.instance.Title, err)
		}
	}
}

// flushOneOwedSettlement retries one owed write, but only while the session
// is between operations.
//
// A retry is a WHOLE-ROW write of live memory, so running it inside another
// session transaction would checkpoint that transaction's half-built state. A
// second handoff is the concrete case: it raises OpReplacing and rewrites Program
// BEFORE it records its own mission marker, and disk strips the transient op — so
// a retry landing in that window stores the incoming agent as settled with no
// obligation at all, and a crash before the real checkpoint loses that takeover
// brief entirely. Trading a duplicated mission for a lost one is not a fix.
//
// The per-session op lock is what actually serializes this, because that is the
// lock every lifecycle operation holds for its whole transaction; the in-flight-op
// re-check under it is the same fence persistPollChange puts in front of the only
// other poll-driven whole-row write. TryLock, never Lock: the poll goroutine must
// not stall behind a slow teardown, and a skipped retry costs nothing — the
// obligation stays owed for the next tick, and a newer settlement on the same row
// discharges it outright.
func (m *Manager) flushOneOwedSettlement(entry settleOwedEntry) error {
	opLock := m.opLockFor(entry.key)
	if !opLock.TryLock() {
		return nil
	}
	defer opLock.Unlock()

	// Re-verify under the lock. Registration can change while the lock is acquired,
	// and an op raised outside it must not be flattened by this write.
	m.mu.Lock()
	registered := m.instances[entry.key] == entry.instance
	m.mu.Unlock()
	if !registered || entry.instance.GetInFlightOp() != session.OpNone {
		return nil
	}
	return m.persistSettlement(entry.repoID, entry.key, entry.instance)
}
