package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestReconcile_AttentionMirrorsOntoLiveRow is the #5461 in-place-update
// regression gate: an already-open TUI must learn an agent's attention request
// from the ordinary snapshot poll — not only from a rebuild — and must drop it
// when the daemon's own clear rule fires. The request is a projection-only
// axis: it moves while liveness stays Running, so the same-row reconcile is
// the only path that ever sees it.
func TestReconcile_AttentionMirrorsOntoLiveRow(t *testing.T) {
	h := newTestHome(t)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "worker", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStatusForTest(session.Running)
	h.store.AddInstance(inst)

	live := inst.ToInstanceData()
	require.Equal(t, session.LiveRunning, live.Liveness)

	live.Attention = &session.Attention{Reason: "which file?", SetAt: time.Now()}
	h.reconcileSnapshot([]session.InstanceData{live})
	require.Same(t, inst, h.store.GetInstanceByTitle("worker"),
		"a same-session snapshot updates the row in place")
	require.NotNil(t, inst.Attention(), "the poll must carry the request onto the live row")
	require.Equal(t, "which file?", inst.Attention().Reason)

	live.Attention = nil
	h.reconcileSnapshot([]session.InstanceData{live})
	require.Nil(t, inst.Attention(), "the daemon's clear mirrors through the same poll")
}
