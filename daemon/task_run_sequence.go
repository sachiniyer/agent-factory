package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/task"
)

type taskRunAdmission struct {
	generationID string
	sequence     uint64
	revision     uint64
}

// nextTaskRunAdmission captures the task incarnation and last-run revision,
// then assigns the clock-independent delivery order before the create delegates
// its reservation boundary. Combining the persisted task floor with every
// sequence already admitted in memory gives restarts and concurrent creates one
// monotonic order. The captured revision is the compare side of publication's
// CAS: watcher supervision written during provisioning must win.
//
// The row read is part of admission, not a best-effort hint. Its generation,
// sequence, and revision all have legitimate zero values, so a read failure
// cannot be converted to zeros without inventing a stale compare-and-set proof.
// Fail closed before reserving a sequence or creating any runtime.
func (m *Manager) nextTaskRunAdmission(taskID, expectedGenerationID string) (taskRunAdmission, error) {
	if taskID == "" {
		return taskRunAdmission{}, nil
	}

	storedTask, err := task.GetTask(taskID)
	if err != nil {
		return taskRunAdmission{}, fmt.Errorf("read task %s for run admission: %w", taskID, err)
	}
	if storedTask.GenerationID != expectedGenerationID {
		return taskRunAdmission{}, fmt.Errorf("task %s was replaced before its run was admitted", taskID)
	}
	admission := taskRunAdmission{
		generationID: storedTask.GenerationID,
		sequence:     storedTask.LastRunSequence,
		revision:     storedTask.LastRunRevision,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if admission.sequence > m.taskRunSequence {
		m.taskRunSequence = admission.sequence
	}
	if m.taskRunSequence == ^uint64(0) {
		return taskRunAdmission{}, fmt.Errorf("task run sequence exhausted")
	}
	m.taskRunSequence++
	admission.sequence = m.taskRunSequence
	return admission, nil
}
