package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7129: "the repair" is the request. Whatever set-conversation can or cannot
// see of the runtime, the record describes the moment the request began.

// N-4: a repair whose held query could not answer is still dated no later
// than the moment the request began — never after the op-lock wait and the
// store walk. With no observation and no follow to see the pane, that date is
// all the staleness bound has, and a thread the pane started while the
// request was waiting must not read as one the repair overrode.
func TestOperatorRepairWithNoObservationIsDatedNoLaterThanTheRequest(t *testing.T) {
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, inst.GetWorktreePath())
	var mu sync.Mutex
	var look time.Time
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		mu.Lock()
		defer mu.Unlock()
		if look.IsZero() {
			look = time.Now()
		}
		return nil, errors.New("the process walk failed")
	})

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	stored := persistedAgentConversation(t, repoID)
	require.False(t, stored.RepairStamped, "fixture: the held query could not answer, so nothing was observed")
	mu.Lock()
	requested := look
	mu.Unlock()
	require.False(t, requested.IsZero(), "fixture: the request must have asked the runtime")
	require.False(t, stored.CapturedAt.After(requested),
		"the repair is dated %s, after the request had already asked the runtime (%s)", stored.CapturedAt, requested)
}

// N-1 at the daemon: a runtime that replaced the observed one while the
// request ran was launched after the request began, so it held nothing then.
// The repair records that its runtime was replaced as of the request, rather
// than letting the follow's first look at the replacement become its
// baseline. What the request saw may be either runtime's, so it is not kept.
func TestOperatorRepairAcrossARuntimeReplacementRecordsTheReplacement(t *testing.T) {
	manager, inst, repoID, _, codexHome := codexLoopTestInstance(t)
	dir := inst.GetWorktreePath()
	fresh := writeHeldTestRollout(t, codexHome, heldFreshThreadID, dir)
	writeHeldTestRollout(t, codexHome, heldUserThreadID, dir)
	held := &heldOnly{}
	held.set(fresh)
	var once sync.Once
	inst.SetCodexRuntimeHeldOverrideForTest(func() (map[string]struct{}, error) {
		// The runtime is replaced while the request is looking at it.
		once.Do(func() { inst.MarkAgentRuntimeReplacedForTest(true) })
		return held.snapshot()
	})

	require.NoError(t, manager.setConversation(setConversationReq(inst, repoID, heldUserThreadID), nil))
	stored := persistedAgentConversation(t, repoID)
	require.Equal(t, heldUserThreadID, stored.ID)
	require.False(t, stored.RepairRuntimeReplacedAt.IsZero(),
		"the replacement's first answer must not become the repair's baseline")
	require.True(t, stored.RepairRuntimeReplacedAt.Equal(stored.CapturedAt),
		"replaced as of the request (%s), which dates the repair (%s)", stored.RepairRuntimeReplacedAt, stored.CapturedAt)
	require.False(t, stored.RepairStamped, "the request's answer may be the replacement's, so it is no stamp")
	require.Empty(t, stored.RepairHeldID)
	require.False(t, stored.RepairHeldObserved)
}
