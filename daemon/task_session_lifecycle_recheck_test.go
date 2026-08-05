package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// The post-worktree hook wait can last minutes with the session visible and
// Ready. A user who prompts it in that window has adopted the work, and an
// on_complete owed to the finished run must not land on it — otherwise
// kill|archive destroys work started after the task completed (#2948, from
// #2861's review).
func TestStillTheFinishedRun(t *testing.T) {
	newManager := func(inst *session.Instance, repoID, title string) *Manager {
		m := &Manager{instances: map[string]*session.Instance{}}
		if inst != nil {
			m.instances[daemonInstanceKey(repoID, title)] = inst
		}
		return m
	}

	t.Run("unchanged idle run is still ours", func(t *testing.T) {
		inst := &session.Instance{ID: "sess-1", Title: "t"}
		inst.SetStatusForTest(session.Ready)
		m := newManager(inst, "repo", "t")
		require.True(t, m.stillTheFinishedRun("repo", "sess-1", "t", inst.StateEpoch(), inst.PromptDeliveries()))
	})

	// The case a liveness level check cannot see, and the reason this uses an
	// epoch: the user prompts the finished session during the hook wait and the
	// turn SETTLES BACK to Ready before the hooks end. Liveness reads Ready
	// again and TaskRunActive is permanently false, so both look exactly like an
	// untouched finished run — but the work is now the user's.
	t.Run("work adopted and settled back to Ready is not ours", func(t *testing.T) {
		inst := &session.Instance{ID: "sess-1", Title: "t"}
		inst.SetStatusForTest(session.Ready)
		m := newManager(inst, "repo", "t")
		atCompletion := inst.StateEpoch()
		atDeliveries := inst.PromptDeliveries()

		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))

		require.Equal(t, session.LiveReady, inst.GetLiveness(), "it looks idle again")
		require.False(t, inst.TaskRunActive(), "and the task run is long over")
		require.False(t, m.stillTheFinishedRun("repo", "sess-1", "t", atCompletion, atDeliveries),
			"a turn that started and finished still moved the epoch, so the verb must not land")
	})

	t.Run("a same-titled replacement is not ours", func(t *testing.T) {
		inst := &session.Instance{ID: "sess-2", Title: "t"}
		inst.SetStatusForTest(session.Ready)
		m := newManager(inst, "repo", "t")
		require.False(t, m.stillTheFinishedRun("repo", "sess-1", "t", inst.StateEpoch(), inst.PromptDeliveries()),
			"a verb owed to one session must never land on its replacement")
	})

	t.Run("a session still working is not ours", func(t *testing.T) {
		inst := &session.Instance{ID: "sess-1", Title: "t"}
		inst.SetStatusForTest(session.Ready)
		m := newManager(inst, "repo", "t")
		atCompletion := inst.StateEpoch()
		atDeliveries := inst.PromptDeliveries()
		require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveRunning)))
		require.False(t, m.stillTheFinishedRun("repo", "sess-1", "t", atCompletion, atDeliveries),
			"work adopted during the hook wait is the user's, not the task's")
	})

	t.Run("a vanished session is not ours", func(t *testing.T) {
		m := newManager(nil, "repo", "t")
		require.False(t, m.stillTheFinishedRun("repo", "sess-1", "t", 0, 0))
	})
}

// The case neither the liveness level NOR the epoch can see: a prompt delivered
// during the hook wait whose liveness edge the poll has not observed yet. The
// session still reads Ready, TaskRunActive is still false, and the epoch is
// unchanged — every lifecycle signal says nothing happened, and the work is
// nonetheless the user's now.
func TestStillTheFinishedRun_APromptDeliveredDuringTheWaitIsAdoption(t *testing.T) {
	inst := &session.Instance{ID: "sess-1", Title: "t"}
	inst.SetStatusForTest(session.Ready)
	m := &Manager{instances: map[string]*session.Instance{
		daemonInstanceKey("repo", "t"): inst,
	}}
	atEpoch, atDeliveries := inst.StateEpoch(), inst.PromptDeliveries()

	inst.NotePromptDelivery()

	require.Equal(t, session.LiveReady, inst.GetLiveness(), "it still reads idle")
	require.False(t, inst.TaskRunActive(), "and the task run is still over")
	require.Equal(t, atEpoch, inst.StateEpoch(), "and the epoch has not moved")
	require.False(t, m.stillTheFinishedRun("repo", "sess-1", "t", atEpoch, atDeliveries),
		"a delivered prompt is adoption even before its liveness edge is observed")
}
