package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
)

type taskArmingSnapshot struct {
	all            []task.Task
	safe           []task.Task
	bindingUpdates []task.Task
	refused        []error
}

// persistedTasksForArming checks every enabled targeted relationship before a
// reload arms cron tasks or watch processes. The caller holds taskTargetMu so
// validation, the returned snapshot, and the eventual scheduler/watch reload
// are one lifecycle decision. Unsafe tasks are excluded and returned as loud
// refusals; one stale relationship must not suppress unrelated automation.
func (m *Manager) persistedTasksForArming() (taskArmingSnapshot, error) {
	tasks, bindingUpdates, err := task.LoadTasksWithStableRepoBindingUpdates()
	if err != nil {
		return taskArmingSnapshot{}, fmt.Errorf("could not load and stabilize task bindings: %w", err)
	}
	snapshot := taskArmingSnapshot{
		all: tasks, safe: make([]task.Task, 0, len(tasks)), bindingUpdates: bindingUpdates,
	}
	for _, candidate := range tasks {
		target := task.CanonicalTargetSession(candidate.TargetSession)
		if !candidate.Enabled || target == "" {
			snapshot.safe = append(snapshot.safe, candidate)
			continue
		}
		validation := m.prepareTaskTargetValidation(candidate.RepoID, target, true)
		if err := m.validateEnabledTaskTarget(candidate, validation); err != nil {
			snapshot.refused = append(snapshot.refused, fmt.Errorf("persisted task %q was not armed because its target relationship is unsafe: %w", candidate.ID, err))
			continue
		}
		snapshot.safe = append(snapshot.safe, candidate)
	}
	return snapshot, nil
}

// armTaskAutomation serializes startup arming with every task control writer,
// validates the persisted relationships once, and reloads cron and watch as one
// decision. A validation/load failure leaves both subsystems unarmed; the caller
// emits the single operator-facing failure and still starts their empty hosts.
func armTaskAutomation(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor) error {
	scheduler.controlMu.Lock()
	defer scheduler.controlMu.Unlock()
	refused, err := reloadTaskAutomation(manager, scheduler, watchers)
	if err != nil {
		return err
	}
	return errors.Join(refused...)
}

// reloadTaskAutomation reloads cron and watch from the same validated snapshot.
// The caller holds scheduler.controlMu; this helper owns taskTargetMu so a task
// write, archive, restore, or project deletion cannot cross the validation-to-
// arm interval. Unsafe legacy rows stay enabled on disk for explicit repair but
// are absent from both live subsystems, while unrelated safe tasks still arm.
func reloadTaskAutomation(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor) ([]error, error) {
	manager.taskTargetMu.Lock()
	defer manager.taskTargetMu.Unlock()
	snapshot, err := manager.persistedTasksForArming()
	if err != nil {
		return nil, err
	}
	for _, updated := range snapshot.bindingUpdates {
		manager.publishEvent(agentproto.EventTaskUpdated, updated)
	}
	if err := scheduler.reloadTasks(snapshot.safe); err != nil {
		return nil, err
	}
	if watchers != nil {
		if err := watchers.reloadSnapshot(snapshot.safe, snapshot.all); err != nil {
			return nil, err
		}
	}
	return snapshot.refused, nil
}

// taskRepoIDForValidation mirrors task.AddTaskChecked's bind-time identity
// derivation outside the tasks-file lock. A second resolution inside the task
// package remains authoritative; any mismatch matters only for reserved-root
// reachability and is rejected as indeterminate by the final validator.
func taskRepoIDForValidation(projectPath string) string {
	repo, err := config.RepoFromPath(projectPath)
	if err != nil {
		return ""
	}
	return repo.ID
}
