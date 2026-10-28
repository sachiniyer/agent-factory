package daemon

import (
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// stampTaskRunInterrupted records "aborted: runtime replaced" on the task whose
// run a runtime replacement interrupted (#4222) — only while the interrupted
// run is still the task's latest recorded run. LastRunStatus is task-wide, and
// a create-per-fire task may have run again since this session went Lost. Two
// proofs say the stamp is not this session's to write (#4222 review): a pending
// fire marker naming a DIFFERENT session — the occurrence in flight owns that
// status — and a registered session a newer fire already spawned for the same
// task. Writing over either would report a run this interrupt never touched as
// aborted, on the preserved newer LastRunAt. peers is a snapshot of the
// manager's registered sessions; the caller owns the lock discipline.
//
// Best-effort like the run path's own status write: the durable fact is the
// cleared run marker, which the replacement's settlement already persisted.
func stampTaskRunInterrupted(instance *session.Instance, peers []*session.Instance) {
	taskID := instance.TaskID
	if taskID == "" {
		return
	}
	if marker, err := readTaskFireMarker(taskID); err == nil && marker != nil &&
		marker.InstanceID != "" && marker.InstanceID != instance.ID {
		return
	}
	for _, peer := range peers {
		if peer != instance && peer.TaskID == taskID && peer.CreatedAt.After(instance.CreatedAt) {
			return
		}
	}
	if _, _, err := task.RecordRunStatus(taskID, taskRunKey(instance), nil, taskStatusAbortedRuntimeReplaced); err != nil {
		log.WarningLog.Printf("could not record task %s's interrupted run (session %q): %v", taskID, instance.Title, err)
	}
}

// taskRunPeerSnapshot copies the manager's registered sessions for
// stampTaskRunInterrupted's newer-run guard — a slice, so the caller can take
// the lock and the stamp can iterate after releasing it.
func (m *Manager) taskRunPeerSnapshot() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	peers := make([]*session.Instance, 0, len(m.instances))
	for _, peer := range m.instances {
		peers = append(peers, peer)
	}
	return peers
}
