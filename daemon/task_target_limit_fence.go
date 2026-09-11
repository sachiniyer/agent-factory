package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// testHookTaskPromptBeforeLimitFence lets tests put a limit publication in the
// old check-to-send race. Production never replaces this no-op.
var testHookTaskPromptBeforeLimitFence = func() {}

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

	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	m.mu.Lock()
	instance := m.instances[daemonInstanceKey(t.RepoID, target)]
	m.mu.Unlock()
	limited := instance != nil && instance.GetLiveness() == session.LiveLimitReached
	return limited, nil
}
