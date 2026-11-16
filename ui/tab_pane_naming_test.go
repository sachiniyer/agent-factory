package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestCreatingFallbackReadsAsTheFormForTheRowBeingNamed pins #7407. The create
// form raises OpCreating on its own row the moment the form opens, so the agent
// slot said "Setting up workspace…" while the user was still typing a name, and
// kept saying it beside the refusal of a taken name. That copy now belongs only
// to a create actually in flight: the row being named reads as the form, on
// both the capture path and the scroll-exit path.
func TestCreatingFallbackReadsAsTheFormForTheRowBeingNamed(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "alpha", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	require.NoError(t, inst.Transition(session.BeginCreate()))
	require.True(t, inst.IsCreating(), "precondition: the row carries the create op")

	var formRow *session.Instance
	p := NewTabPane(previewFromInstance)
	p.SetSize(80, 30)
	p.SetNamingCheck(func(i *session.Instance) bool { return i == formRow })

	// The form is open on this row: no request exists yet.
	formRow = inst
	require.NoError(t, p.UpdateContent(inst, 0))
	require.Contains(t, p.String(), namingFallback)
	require.NotContains(t, p.String(), "Setting up workspace…",
		"a row that is only being named has no create in flight")

	// Leaving scroll mode re-derives the fallback synchronously (#823); it must
	// agree with the capture path.
	enableHostHistory(p, inst, 0)
	require.NoError(t, p.ScrollUp(inst, 0))
	require.True(t, p.scroll.Active(), "precondition: ScrollUp enters scroll mode")
	require.NoError(t, p.ResetToNormalMode(inst, 0))
	require.Contains(t, p.String(), namingFallback)
	require.NotContains(t, p.String(), "Setting up workspace…")

	// The form submitted: the row is no longer being named and its create is
	// in flight, which is exactly what the optimistic copy stands for.
	formRow = nil
	require.NoError(t, p.UpdateContent(inst, 0))
	require.Contains(t, p.String(), "Setting up workspace…")

	enableHostHistory(p, inst, 0)
	require.NoError(t, p.ScrollUp(inst, 0))
	require.True(t, p.scroll.Active(), "precondition: ScrollUp enters scroll mode")
	require.NoError(t, p.ResetToNormalMode(inst, 0))
	require.Contains(t, p.String(), "Setting up workspace…")
}
