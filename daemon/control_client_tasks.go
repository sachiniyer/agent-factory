package daemon

// The task control-plane client verbs: the callDaemon wrappers behind
// `af tasks` and the TUI's task CRUD, split out of control_client.go alongside
// their request/response types (control_types_tasks.go) and handlers
// (control_server_tasks.go) so the file stays under the length limit (#1145)
// and the six task RPCs read as one surface, as they do on the server side.

import (
	"github.com/sachiniyer/agent-factory/task"
)

// ListTasksNoSpawn returns the daemon's authoritative task list WITHOUT
// starting a daemon (#1029 PR 3). It dials the existing control socket only if
// it is already serving and returns ErrDaemonUnavailable otherwise, so a
// read-only CLI command (tasks list/get) falls back to reading tasks.json off
// disk rather than ever launching a daemon. Task reads do not depend on the
// instance restore, so there is no warm-up starting-error window to wait out
// here.
func ListTasksNoSpawn() ([]task.Task, error) {
	var resp ListTasksResponse
	if err := callDaemonNoEnsure("ListTasks", ListTasksRequest{}, &resp); err != nil {
		return nil, ErrDaemonUnavailable
	}
	return resp.Tasks, nil
}

// AddTask asks the daemon to append a task and re-arm its schedule set. Like
// every callDaemon path it ensures the daemon is running first, so adding a
// task also brings the scheduler up — a task is not schedulable without a
// running daemon.
// actor names the calling surface for the task's audit trail (#3623); pass
// task.ActorUnknown from a caller that has no surface to name.
func AddTask(t task.Task, actor task.Actor) error {
	var resp AddTaskResponse
	return callDaemon("AddTask", AddTaskRequest{Task: t, Actor: string(actor)}, &resp)
}

// UpdateTask asks the daemon to apply a field-level patch to the task with the
// given id and re-arm its schedule, returning the merged record (#1700). Only
// the patch's non-nil fields are written, so a single-field edit never clobbers
// a concurrent edit another client made to a different field.
// actor names the calling surface for the task's audit trail (#3623).
func UpdateTask(id string, update task.TaskUpdate, expect task.ProjectExpectation, actor task.Actor) (task.Task, error) {
	var resp UpdateTaskResponse
	if err := callDaemon("UpdateTask", UpdateTaskRequest{ID: id, Update: update, Expect: expect, Actor: string(actor)}, &resp); err != nil {
		return task.Task{}, err
	}
	return resp.Task, nil
}

// RemoveTask asks the daemon to delete a task and re-arm its schedule.
func RemoveTask(id string, expect task.ProjectExpectation) error {
	var resp RemoveTaskResponse
	return callDaemon("RemoveTask", RemoveTaskRequest{ID: id, Expect: expect}, &resp)
}

// RestartTask asks the daemon to stop and replace one enabled watch command,
// waiting until the old process tree is gone and the replacement has started.
func RestartTask(id string, expect task.ProjectExpectation) error {
	var resp RestartTaskResponse
	return callDaemon("RestartTask", RestartTaskRequest{ID: id, Expect: expect}, &resp)
}

// TriggerTask asks the daemon to fire a task now through the shared RunTask
// firing path (the same entrypoint the in-daemon scheduler uses). Replaces the
// old in-process daemon.RunTask CLI call so CLI, TUI, and scheduler triggers all
// converge on one daemon-owned firing path (#1169-class fix).
// actor names the calling surface for the task's audit trail (#6318); pass
// task.ActorUnknown from a caller that has no surface to name.
func TriggerTask(id string, expect task.ProjectExpectation, actor task.Actor) error {
	var resp TriggerTaskResponse
	return callDaemon("TriggerTask", TriggerTaskRequest{ID: id, Expect: expect, Actor: string(actor)}, &resp)
}
