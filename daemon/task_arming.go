package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
		if candidate.Enabled && target != "" {
			validation := m.prepareTaskTargetValidation(candidate.RepoID, target, true)
			if err := m.validateEnabledTaskTarget(candidate, validation); err != nil {
				snapshot.refused = append(snapshot.refused, fmt.Errorf("persisted task %q was not armed because its target relationship is unsafe: %w", candidate.ID, err))
				m.recordArmingStatus(candidate, notArmedStatus(err))
				continue
			}
		}
		// Everything reaching here is armed, or deliberately not enabled. Either
		// way a previous not-armed refusal no longer describes it — including the
		// repair of DROPPING the target, which turns the task into an ordinary
		// cron/watch task and never reaches the validation above.
		m.clearStaleNotArmedStatus(candidate)
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

// notArmedPrefix marks a LastRunStatus written by arming rather than by a run.
// It carries the "errored:" prefix the TUI already keys on to render a task as
// failed (ui/task_pane.go), so a refusal shows up where people look without a
// new surface to build or discover.
const notArmedPrefix = "errored: not armed — "

func notArmedStatus(cause error) string {
	return notArmedPrefix + cause.Error()
}

// recordArmingStatus writes a refusal onto the task's own status.
//
// Without this a refused task is INDISTINGUISHABLE FROM A HEALTHY ONE: it stays
// enabled on disk, keeps whatever LastRunStatus its last successful run wrote,
// and is absent from both cron and watch. The only trace is one joined warning
// at daemon startup, and armed-ness is exposed on no surface —
// scheduledTaskIDs/watchingTaskIDs have no production callers. On a box nobody
// opens the TUI on, which is the case this daemon exists to serve, a nightly
// task can stop running forever while every surface says it ran fine (#2929).
//
// LastRunAt is deliberately left alone (UpdateTaskStatus's nil mode): arming is
// a supervision decision, not a run, so the timestamp of the last real delivery
// must survive it.
//
// Written only when the status actually changes. Arming runs on every task CRUD
// as well as at startup, and a persistently refused task must not rewrite
// tasks.json each time.
func (m *Manager) recordArmingStatus(t task.Task, status string) {
	if t.LastRunStatus == status {
		return
	}
	if err := task.UpdateTaskStatus(t.ID, nil, status); err != nil {
		log.WarningLog.Printf("could not record the arming status for task %q: %v", t.ID, err)
		return
	}
	t.LastRunStatus = status
	m.publishEvent(agentproto.EventTaskUpdated, t)
}

// clearStaleNotArmedStatus removes a not-armed status from a task that is armed
// again, so the refusal does not outlive the condition that caused it. A task
// whose target came back would otherwise keep claiming it is unarmed until its
// next run writes a real status — up to a full day for a nightly schedule, and
// indefinitely for a watch task that has not fired.
//
// Only statuses this file wrote are cleared; a genuine "errored:" from a failed
// run is left exactly as the run recorded it.
func (m *Manager) clearStaleNotArmedStatus(t task.Task) {
	if !strings.HasPrefix(t.LastRunStatus, notArmedPrefix) {
		return
	}
	m.recordArmingStatus(t, "")
}
