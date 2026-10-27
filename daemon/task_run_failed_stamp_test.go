package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
)

// TestFailedRunOwnsStatus pins the failure stamp's compare-and-set (#5645): it
// writes over "started" (the stamp arriving after RunTask's own write), is
// idempotent once the failure is recorded, and yields to a newer session of the
// same task.
func TestFailedRunOwnsStatus(t *testing.T) {
	t0 := time.Now()
	failed := &session.Instance{TaskID: "t1", CreatedAt: t0}
	newer := &session.Instance{TaskID: "t1", CreatedAt: t0.Add(time.Minute)}
	otherTask := &session.Instance{TaskID: "t2", CreatedAt: t0.Add(time.Minute)}

	assert.True(t, failedRunOwnsStatus(failed, []*session.Instance{failed, otherTask}, task.Task{LastRunStatus: "started"}),
		"a stamp must replace the same run's 'started'")
	assert.False(t, failedRunOwnsStatus(failed, []*session.Instance{failed, newer}, task.Task{LastRunStatus: "started"}),
		"a stale stamp must not overwrite a newer run's status")
	assert.False(t, failedRunOwnsStatus(failed, []*session.Instance{failed}, task.Task{LastRunStatus: taskStatusErroredPromptUndelivered}),
		"an already-recorded failure must not be re-stamped")
}
