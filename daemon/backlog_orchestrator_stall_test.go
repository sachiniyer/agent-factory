package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func TestOrchRunActivity(t *testing.T) {
	inst := &session.Instance{ID: "s"}
	cases := []struct {
		name    string
		view    session.LifecycleView
		missing bool
		want    backlog.RunActivity
		ok      bool
	}{
		{name: "killed record gone", missing: true, want: backlog.RunActivity{Stopped: true}, ok: true},
		{name: "user killed", view: session.LifecycleView{UserKilled: true}, want: backlog.RunActivity{Stopped: true}, ok: true},
		{name: "archived", view: session.LifecycleView{Liveness: session.LiveArchived}, want: backlog.RunActivity{Stopped: true}, ok: true},
		{name: "dead", view: session.LifecycleView{Liveness: session.LiveDead}, want: backlog.RunActivity{Stopped: true}, ok: true},
		{name: "lost, no restore", view: session.LifecycleView{Liveness: session.LiveLost}, want: backlog.RunActivity{Stopped: true}, ok: true},
		{name: "startup unknown", view: session.LifecycleView{StartupStateUnknown: true, Liveness: session.LiveDead}},
		{name: "op in flight", view: session.LifecycleView{InFlightOp: session.OpArchiving, Liveness: session.LiveReady}},
		{name: "usage limit", view: session.LifecycleView{Liveness: session.LiveLimitReached, BacklogRunActive: true}},
		{name: "live, no run", view: session.LifecycleView{Liveness: session.LiveReady}},
		{name: "working", view: session.LifecycleView{Liveness: session.LiveRunning, BacklogRunActive: true},
			want: backlog.RunActivity{Working: true, LastActive: daemonProcessStart}, ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := orchRunActivity(itemSession{instance: inst, view: c.view}, !c.missing)
			require.Equal(t, c.ok, ok)
			if ok {
				require.Equal(t, c.want, got)
			}
		})
	}
}

func TestOrchRunActivityCountsRuntimeReplacement(t *testing.T) {
	inst := &session.Instance{ID: "s"}
	inst.ClearIdleEvidence()
	got, ok := orchRunActivity(itemSession{instance: inst,
		view: session.LifecycleView{Liveness: session.LiveRunning, BacklogRunActive: true}}, true)
	require.True(t, ok)
	require.True(t, got.Working)
	require.True(t, got.LastActive.After(daemonProcessStart))
	require.Equal(t, inst.LastActivityAt(), got.LastActive)
}

func TestNoteOrchStallsQueuesKilledRun(t *testing.T) {
	m, repoID := orchestratedGroup(t)
	svc := m.backlogService()
	_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		it := b.Item(3)
		it.Status = backlog.StatusRunning
		it.Runs = []backlog.RunRef{{SessionID: "gone", State: backlog.RunStarted, StartedAt: svc.Now()}}
		b.Item(2).Orchestrator.Events = nil
		return []int{3}, nil
	})
	require.NoError(t, err)
	b, err := backlog.Load(repoID)
	require.NoError(t, err)

	now := svc.Now().Add(time.Minute)
	b = m.noteOrchStalls(repoID, b, now)
	ev := b.Item(2).Orchestrator.Events
	require.Len(t, ev, 1)
	require.Equal(t, backlog.EventStalled, ev[0].Kind)
	require.Equal(t, 3, ev[0].Item)

	stored, err := backlog.Load(repoID)
	require.NoError(t, err)
	require.Len(t, stored.Item(2).Orchestrator.Events, 1)
	require.Equal(t, 1, len(m.noteOrchStalls(repoID, stored, now.Add(time.Hour)).Item(2).Orchestrator.Events))
}
