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
// A missing or temporarily unreadable task row deliberately does not introduce
// a new create refusal: later status publication already reports that condition,
// and a sequence below the stored floor is conservatively rejected.
func (m *Manager) nextTaskRunAdmission(taskID, expectedGenerationID string) (taskRunAdmission, error) {
	if taskID == "" {
		return taskRunAdmission{}, nil
	}

	admission := taskRunAdmission{generationID: expectedGenerationID}
	if storedTask, err := task.GetTask(taskID); err == nil {
		if storedTask.GenerationID != expectedGenerationID {
			return taskRunAdmission{}, fmt.Errorf("task %s was replaced before its run was admitted", taskID)
		}
		admission.generationID = storedTask.GenerationID
		admission.sequence = storedTask.LastRunSequence
		admission.revision = storedTask.LastRunRevision
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
