package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/task"
)

// nextTaskRunSequence assigns the clock-independent delivery order before the
// create delegates its reservation boundary. Combining the persisted task floor
// with every sequence already admitted in memory gives restarts and concurrent
// creates one monotonic order.
//
// A missing or temporarily unreadable task row deliberately does not introduce
// a new create refusal: later status publication already reports that condition,
// and a sequence below the stored floor is conservatively rejected.
func (m *Manager) nextTaskRunSequence(taskID string) (uint64, error) {
	if taskID == "" {
		return 0, nil
	}

	var floor uint64
	if storedTask, err := task.GetTask(taskID); err == nil {
		floor = storedTask.LastRunSequence
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if floor > m.taskRunSequence {
		m.taskRunSequence = floor
	}
	if m.taskRunSequence == ^uint64(0) {
		return 0, fmt.Errorf("task run sequence exhausted")
	}
	m.taskRunSequence++
	return m.taskRunSequence, nil
}
