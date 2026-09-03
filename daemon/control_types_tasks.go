package daemon

// Task control-plane request/response types. Split out of control_types.go so
// the task RPCs' shapes — and the audit actor every mutating one now carries
// (#3623) — read as one surface.

import (
	"github.com/sachiniyer/agent-factory/task"
)

type ReloadTasksRequest struct{}
type ReloadTasksResponse struct {
	OK bool `json:"ok"`
}

// Task CRUD RPCs (#1029 PR 3). They promote task writes to the daemon so it is
// the sole task writer among clients — exactly the model sessions already
// follow — and the write and the scheduler/watcher refresh happen atomically
// in-process (no separate ReloadTasks poke). The on-disk tasks.json format is
// unchanged; the daemon reuses the same task.AddTask/UpdateTask/RemoveTask
// (config.WithFileLock + saveTasks) that clients used to call directly.

// ListTasksRequest asks the daemon for the full task list. There is
// deliberately no repo filter: the daemon returns every repo's tasks and the
// CLI applies its --repo filter, matching the disk-read fallback
// (task.LoadTasks). It is the read side of the task single-writer model, mirror
// of Snapshot for sessions.
type ListTasksRequest struct{}
type ListTasksResponse struct {
	Tasks []task.Task `json:"tasks"`
}

// AddTaskRequest carries a fully-populated task.Task to append. The CLI/TUI
// still build and validate the struct (flag parsing, ID generation, program
// resolution); the daemon re-validates via task.AddTask and owns the write.
type AddTaskRequest struct {
	Task task.Task `json:"task"`
	// Actor is the SURFACE this create came from ("cli", "tui", "api", …),
	// recorded in the task's audit trail (#3623). A client that declares nothing
	// is resolved from the transport instead — see taskActor. It is a label for a
	// human reading the trail, not an authentication claim: every caller that can
	// reach this RPC can already write the task store.
	Actor string `json:"actor,omitempty"`
}
type AddTaskResponse struct {
	OK bool `json:"ok"`
	MutationOutcome
}

// UpdateTaskRequest carries a FIELD-LEVEL patch (#1700): the ID of the task to
// edit and a task.TaskUpdate holding only the field(s) the caller intends to
// change. The daemon merges the patch onto the freshly-loaded record under the
// file lock, leaving every unspecified field as-stored — so a single-field edit
// (the enable/disable toggle sends just Enabled) cannot clobber a concurrent
// edit another client made to a different field. This replaces the prior
// full-struct read-modify-write, which re-applied every user field from the
// caller's possibly-stale copy. Scheduler-owned fields (LastRunAt/LastRunStatus/
// CreatedAt) are never patchable — UpdateTaskStatus stays their writer.
//
// Expect optionally carries the project the caller authorized the id against,
// re-verified under the same lock — see task.ProjectExpectation.
type UpdateTaskRequest struct {
	ID     string                  `json:"id"`
	Update task.TaskUpdate         `json:"update"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
	// Actor is the surface this patch came from — see AddTaskRequest.Actor. It is
	// what makes "who disabled this task, and when?" answerable at all (#3623).
	Actor string `json:"actor,omitempty"`
}

// UpdateTaskResponse returns the merged record the write produced, so the CLI
// can print the authoritative post-edit task and the daemon publishes the full
// task (not the partial patch) on its EventTaskUpdated.
type UpdateTaskResponse struct {
	OK   bool      `json:"ok"`
	Task task.Task `json:"task"`
	MutationOutcome
}

// Expect optionally carries the project the caller authorized the id against,
// re-verified under the same lock — see task.ProjectExpectation.
type RemoveTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type RemoveTaskResponse struct {
	OK bool `json:"ok"`
	MutationOutcome
}

// RestartTaskRequest asks the daemon to stop and replace one enabled watch
// task from its current on-disk definition. Expect carries the same project
// compare-and-swap as the other id-addressed task mutations.
type RestartTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type RestartTaskResponse struct {
	OK bool `json:"ok"`
}

// TriggerTaskRequest asks the daemon to fire a task NOW through the same
// RunTask firing path the in-daemon scheduler uses (#1029 PR 3 / #1169-class
// fix). The handler preserves RunTask's guards: watch tasks and disabled tasks
// are refused.
// Expect optionally carries the project the caller authorized the id against,
// re-verified against the same load that produces the fired record — see
// task.ProjectExpectation.
type TriggerTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type TriggerTaskResponse struct {
	OK bool `json:"ok"`
}
