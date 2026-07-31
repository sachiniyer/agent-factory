package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/task"
)

// RestartTask synchronously replaces one enabled watch command. The task-control
// lock makes the scope re-check, target-lifecycle validation, stop/join, and
// replacement one operation with respect to Add/Update/Remove/ReloadTasks. The
// manager's target lock also prevents archive, restore, or project deletion from
// crossing the validation-to-arm interval.
func (s *controlServer) RestartTask(req RestartTaskRequest, resp *RestartTaskResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if s.watchers == nil {
		return fmt.Errorf("this daemon does not host a watch task supervisor")
	}
	unlock, err := s.lockTaskControl()
	if err != nil {
		return err
	}
	defer unlock()

	var tsk *task.Task
	if s.manager == nil {
		tsk, err = task.GetTask(req.ID)
	} else {
		s.manager.taskTargetMu.Lock()
		defer s.manager.taskTargetMu.Unlock()
		var all, bindingUpdates []task.Task
		all, bindingUpdates, err = task.LoadTasksWithStableRepoBindingUpdates()
		if err == nil {
			for i := range all {
				if all[i].ID == req.ID {
					tsk = &all[i]
					break
				}
			}
			for _, updated := range bindingUpdates {
				s.manager.publishEvent(agentproto.EventTaskUpdated, updated)
			}
			if tsk == nil {
				err = fmt.Errorf("task with id %q not found", req.ID)
			}
		}
	}
	if err != nil {
		return err
	}
	if err := req.Expect.Verify(*tsk); err != nil {
		return err
	}
	target := task.CanonicalTargetSession(tsk.TargetSession)
	if tsk.Enabled && target != "" && s.manager != nil {
		validation := s.manager.prepareTaskTargetValidation(tsk.RepoID, target, true)
		if err := s.manager.validateEnabledTaskTarget(*tsk, validation); err != nil {
			return fmt.Errorf("watch task %q was not restarted because its target relationship is unsafe: %w", tsk.ID, err)
		}
	}
	if err := s.watchers.restart(*tsk); err != nil {
		return err
	}
	resp.OK = true
	return nil
}
