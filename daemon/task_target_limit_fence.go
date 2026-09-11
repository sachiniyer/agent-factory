package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// testHookTaskPromptBeforeLimitFence lets tests put a limit publication in the
// old check-to-send race. Production never replaces this no-op.
var testHookTaskPromptBeforeLimitFence = func() {}

// testHookTaskPromptBeforeObservationFence proves a task send has reached the
// boundary while an older pane snapshot is still in flight.
var testHookTaskPromptBeforeObservationFence = func() {}

// testHookTaskLimitObserveBeforeObservationFence proves queue admission cannot
// overtake a pane snapshot whose derived limit state is still settling.
var testHookTaskLimitObserveBeforeObservationFence = func() {}

// observeTaskTargetLimit orders a watch event's retention admission against
// limit publication. A clean observation admits this event to ordinary queue
// bounds before a later transition; a known limit protects the backlog before
// those bounds can evict it. The lock is released before queue filesystem I/O,
// so a slow AF home cannot stall the status poll's limit publication.
//
// RepoID is retained when the task is bound. Resolving a missing legacy value
// here would put an unbounded Git probe on a watch reader, so legacy identity is
// unknown instead. The drainer still retries delivery and clears a conservative
// marker after success.
func (m *Manager) observeTaskTargetLimit(taskID string) (bool, error) {
	t, err := task.GetTask(taskID)
	if err != nil {
		return false, fmt.Errorf("load task: %w", err)
	}
	target := task.CanonicalTargetSession(t.TargetSession)
	if target == "" {
		return false, nil
	}
	if t.RepoID == "" {
		return false, fmt.Errorf("task has no retained repository identity")
	}

	key := daemonInstanceKey(t.RepoID, target)
	for {
		m.mu.Lock()
		instance := m.instances[key]
		m.mu.Unlock()
		if instance == nil {
			// Serialize absence with startup-limit publication. A limited create
			// holds accountLimitMu through registration; if any instance appeared
			// while we entered the fence, retry so its observation fence participates.
			m.accountLimitMu.Lock()
			m.mu.Lock()
			current := m.instances[key]
			m.mu.Unlock()
			m.accountLimitMu.Unlock()
			if current == nil {
				return false, nil
			}
			continue
		}

		testHookTaskLimitObserveBeforeObservationFence()
		releaseObservationFence := instance.HoldAgentObservationSettlement()
		m.accountLimitMu.Lock()
		m.mu.Lock()
		current := m.instances[key]
		if current != instance {
			m.mu.Unlock()
			m.accountLimitMu.Unlock()
			releaseObservationFence()
			// The result depends on the registered runtime, not just its title.
			// A replacement during admission is unknown until observed in full.
			return false, fmt.Errorf("target session changed during usage-limit observation")
		}
		limited := instance.GetLiveness() == session.LiveLimitReached
		m.mu.Unlock()
		m.accountLimitMu.Unlock()
		releaseObservationFence()
		return limited, nil
	}
}
