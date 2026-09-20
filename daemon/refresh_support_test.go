package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/session"
)

// An unloadable row is counted against max_concurrent_runs by the same rule
// session.FromInstanceData applies when it can load the row: a pending
// interruption means the run already ended (#4224 review, F3).
//
// At 108540d8 the pending row is counted, so the second assertion fails.
func TestRawTaskRunHoldsSlotMatchesLoadedRunActivity(t *testing.T) {
	active := session.InstanceData{Title: "run", TaskID: "task1", TaskRunActive: true}
	assert.True(t, rawTaskRunHoldsSlot(active))

	pending := active
	pending.TaskRunInterruptionPending = true
	assert.False(t, rawTaskRunHoldsSlot(pending),
		"a row whose run was closed as interrupted must not hold a slot while it fails to load")
}
