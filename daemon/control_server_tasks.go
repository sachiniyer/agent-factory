package daemon

// The task CRUD control plane. Split out of control_server.go alongside its
// request/response types (control_types_tasks.go) so the handlers, the audit
// actor each mutating one now records, and the schedule-health annotation on
// every task the daemon hands back read as one surface (#3623).

import (
	"context"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/task"
)

// ListTasks returns the full task list read from tasks.json (#1029 PR 3).
// Deliberately NOT gated on requireManagerReady: task state lives on disk,
// independent of the instance restore, so a read is always safe and always
// current even while the daemon is warming up.
//
// Every record carries its schedule health: LoadTasks derives overdue/missed
// from the record itself, and withLiveArming adds what only this process can
// see — whether the task is actually armed, and what the armed entry will fire
// next (#3623).
func (s *controlServer) ListTasks(_ ListTasksRequest, resp *ListTasksResponse) error {
	// The two halves are taken under ONE hold of the task-control lock, which is
	// what every task mutation holds across its write AND its scheduler reload.
	// Without it the read straddles a mutation: a removal committing between the
	// load and the arming lookup returns the deleted task paired with "not
	// armed", and doctor raises an actionable alarm about a task that no longer
	// exists. armingSnapshot's own lock cannot help — it makes the scheduler's two
	// structures agree with each other, not with a task list loaded a moment
	// earlier. Publishing task events before the reload makes an event-driven
	// refetch land in exactly this window.
	//
	// It stays ungated on manager readiness (see above): the lock serializes this
	// read against task writes only, so a read during instance restore still
	// answers.
	if s.scheduler != nil {
		unlock, err := s.lockTaskControl()
		if err != nil {
			return err
		}
		defer unlock()
	}
	tasks, err := task.LoadTasks()
	if err != nil {
		return err
	}
	resp.Tasks = s.withLiveArming(tasks)
	return nil
}

// AddTask is the net/rpc entry point; the HTTP route calls addTask directly so
// the request context (and with it the transport the actor falls back to)
// survives. Same split as CloseTab/KillSession.
func (s *controlServer) AddTask(req AddTaskRequest, resp *AddTaskResponse) error {
	return s.addTask(context.Background(), req, resp)
}

// UpdateTask is the net/rpc entry point; see AddTask.
func (s *controlServer) UpdateTask(req UpdateTaskRequest, resp *UpdateTaskResponse) error {
	return s.updateTask(context.Background(), req, resp)
}

// AddTask persists a new task and re-arms the schedule set (#1029 PR 3). The
// write goes through task.AddTask (config.WithFileLock + saveTasks) — the same
// path clients used to call directly — so the on-disk format is unchanged; the
// difference is the daemon now owns it and refreshes its own scheduler/watchers
// in the same call.
func (s *controlServer) addTask(ctx context.Context, req AddTaskRequest, resp *AddTaskResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	unlock, err := s.lockTaskControl()
	if err != nil {
		return err
	}
	defer unlock()
	var validate func(task.Task) error
	targetLocked := false
	if s.manager != nil {
		s.manager.taskTargetMu.Lock()
		targetLocked = true
		defer func() {
			if targetLocked {
				s.manager.taskTargetMu.Unlock()
			}
		}()
		repoID := taskRepoIDForValidation(req.Task.ProjectPath)
		validation := s.manager.prepareTaskTargetValidation(
			repoID, req.Task.TargetSession, req.Task.Enabled,
		)
		validate = func(candidate task.Task) error {
			return s.manager.validateEnabledTaskTarget(candidate, validation)
		}
	}
	created, err := task.AddTaskChecked(req.Task, taskActor(ctx, req.Actor), validate)
	if targetLocked {
		s.manager.taskTargetMu.Unlock()
		targetLocked = false
	}
	if err != nil {
		return err
	}
	resp.OK = true
	// The task-file write is the durable commit. Publish it before the
	// non-transactional scheduler/watch reload so other clients never remain
	// stale when that follow-up fails.
	s.manager.publishEvent(agentproto.EventTaskCreated, created)
	// Scoped to the row just written (#3837). A create names its own task and
	// nothing else, so it must not perform the store-wide re-arm that would
	// resurrect every watcher another task's breaker had stopped for good.
	if reloadErr := s.reloadTaskSchedulesLocked(oneWatchTask(created.ID, true)); reloadErr != nil {
		// Returned as an ERROR, not through the envelope: net/rpc does not send
		// the response body when a handler errors, and the HTTP route turns this
		// into an error envelope carrying the shared committed code — which is
		// what every existing consumer reads. The envelope covers the handlers
		// that answer OK; these answer with an error (#3036).
		return &mutationCommittedError{err: fmt.Errorf(
			"%s %w", taskAddCommittedErrorPrefix, reloadErr)}
	}
	return nil
}

func (s *controlServer) updateTask(ctx context.Context, req UpdateTaskRequest, resp *UpdateTaskResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	unlock, err := s.lockTaskControl()
	if err != nil {
		return err
	}
	defer unlock()
	var validate func(task.Task) (string, error)
	targetLocked := false
	if s.manager != nil {
		s.manager.taskTargetMu.Lock()
		targetLocked = true
		defer func() {
			if targetLocked {
				s.manager.taskTargetMu.Unlock()
			}
		}()
		// Legacy rows have no retained RepoID. Resolve the current authoritative
		// ProjectPath before UpdateTaskChecked takes the tasks-file lock (git must
		// never run under that lock), then lend that identity to validation. The
		// task-control lock makes this stable against every supported writer; the
		// path equality check fails closed if an out-of-band edit races us.
		existing, getErr := task.GetTask(req.ID)
		if getErr != nil {
			return getErr
		}
		legacyPath := ""
		legacyRepoID := ""
		if existing.RepoID == "" && req.Update.ProjectPath == nil {
			legacyPath = existing.ProjectPath
			legacyRepoID = taskRepoIDForValidation(legacyPath)
		}
		validationRepoID := existing.RepoID
		if req.Update.ProjectPath != nil {
			validationRepoID = taskRepoIDForValidation(*req.Update.ProjectPath)
		} else if validationRepoID == "" {
			validationRepoID = legacyRepoID
		}
		validationTarget := existing.TargetSession
		if req.Update.TargetSession != nil {
			validationTarget = *req.Update.TargetSession
		}
		validationEnabled := existing.Enabled
		if req.Update.Enabled != nil {
			validationEnabled = *req.Update.Enabled
		}
		validation := s.manager.prepareTaskTargetValidation(
			validationRepoID, validationTarget, validationEnabled,
		)
		validateTargetRelationship := req.Update.Enabled != nil ||
			req.Update.TargetSession != nil || req.Update.ProjectPath != nil
		validate = func(candidate task.Task) (string, error) {
			if candidate.RepoID == "" && candidate.ProjectPath == legacyPath {
				candidate.RepoID = legacyRepoID
			}
			if validateTargetRelationship {
				if err := s.manager.validateEnabledTaskTarget(candidate, validation); err != nil {
					return "", err
				}
			}
			return candidate.RepoID, nil
		}
	}
	merged, err := task.UpdateTaskChecked(req.ID, req.Update, req.Expect, taskActor(ctx, req.Actor), validate)
	if targetLocked {
		s.manager.taskTargetMu.Unlock()
		targetLocked = false
	}
	if err != nil {
		return err
	}
	resp.OK = true
	resp.Task = merged
	// Publish at the durable commit boundary, before the non-transactional
	// schedule refresh. If refresh fails, the caller receives the committed
	// outcome below and every other client still learns to refetch this value.
	// The payload is the authoritative merged record, not the partial patch.
	s.manager.publishEvent(agentproto.EventTaskUpdated, merged)
	// Scoped to the row just written, and a re-arm of it only when the patch
	// carried Enabled — the enable/disable IS the gesture that re-arms a watcher
	// that stopped for good, while an ordinary edit (prompt, target) is not
	// (#3837). A watch_cmd/project_path/name edit restarts the watcher either
	// way: that is a signature change, not a re-arm.
	reloadErr := s.reloadTaskSchedulesLocked(oneWatchTask(req.ID, req.Update.Enabled != nil))
	if reloadErr == nil {
		// Answer with the record as it now stands, arming included: a caller who
		// just disabled a task learns that the disarm actually happened (#3623).
		resp.Task = s.liveTaskRecord(merged)
		return nil
	}
	// See AddTask: an erroring handler sends no response body, so the error is
	// the only channel that reaches both transports.
	return &mutationCommittedError{err: fmt.Errorf(
		"%s %w", taskUpdateCommittedErrorPrefix, reloadErr)}
}

func (s *controlServer) RemoveTask(req RemoveTaskRequest, resp *RemoveTaskResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	unlock, err := s.lockTaskControl()
	if err != nil {
		return err
	}
	defer unlock()
	if err := task.RemoveTask(req.ID, req.Expect); err != nil {
		return err
	}
	resp.OK = true
	// Removal is already durable at this point. Announce that commit even when
	// the scheduler/watch reload cannot apply it in-process.
	s.manager.publishEvent(agentproto.EventTaskRemoved, task.Task{ID: req.ID})
	// Scoped to the row just written (#3837): stop this task's watcher and drop
	// its event queue, leaving every other watcher exactly as it was.
	if reloadErr := s.reloadTaskSchedulesLocked(oneWatchTask(req.ID, true)); reloadErr != nil {
		// Returned as an ERROR, not through the envelope: net/rpc does not send
		// the response body when a handler errors, and the HTTP route turns this
		// into an error envelope carrying the shared committed code — which is
		// what every existing consumer reads. The envelope covers the handlers
		// that answer OK; these answer with an error (#3036).
		return &mutationCommittedError{err: fmt.Errorf(
			"%s %w", taskRemoveCommittedErrorPrefix, reloadErr)}
	}
	return nil
}

// TriggerTask fires a task NOW through the shared RunTask firing path — the same
// entrypoint the in-daemon scheduler uses (#1029 PR 3). This unifies the CLI
// `af tasks trigger`, the TUI run-now, and the cron scheduler on one
// daemon-owned firing path, replacing the old in-process daemon.RunTask CLI call
// (#1169-class fix). RunTask preserves the guards: watch tasks and disabled
// tasks are refused.
func (s *controlServer) TriggerTask(req TriggerTaskRequest, resp *TriggerTaskResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if err := RunTask(req.ID, req.Expect); err != nil {
		return err
	}
	resp.OK = true
	return nil
}
