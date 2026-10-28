package tree

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The unreachable marker (#5571 W1.3) is deliberately STATIC text — a flagged
// row keeps full contrast like [lost], never an animated indicator — and the
// outage detail outranks branch and idle detail because it explains why every
// other fact on the row may be stale.
func TestInstanceRendererUnreachableMarkerAndDetail(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "feature", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.SetBackend(&session.HookBackend{})
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))

	r := NewInstanceRenderer()
	r.SetWidth(100)

	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	assert.NotContains(t, out, "[unreachable]",
		"a reachable remote row must not carry the marker")

	inst.SetUnreachable(errors.New("dial tcp: i/o timeout"))
	out = ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	assert.Contains(t, out, "[unreachable] [remote] feature",
		"a flagged row carries the static marker beside the [remote] badge")
	assert.Contains(t, out, "unreachable",
		"the outage detail must render even on an unselected row")
	assert.Contains(t, out, "i/o timeout",
		"the detail names the last transport error")
	assert.NotRegexp(t, brailleFrame, out,
		"the marker is static text — no animated frame may appear (#1766 discipline)")
}

func TestInstanceRendererUnreachableDetailOutranksBranchAndIdle(t *testing.T) {
	inst, err := session.FromInstanceData(session.InstanceData{
		ID: "unreachable-detail", Title: "feature", Branch: "af-feature",
		Status: session.Lost, Liveness: session.LiveLost,
		Program: "test", BackendType: "remote",
		Unreachable: &session.UnreachableData{
			Since: time.Now().Add(-3 * time.Minute),
			Error: "ssh: connection reset",
		},
	})
	require.NoError(t, err)

	r := NewInstanceRenderer()
	r.SetWidth(120)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	assert.Contains(t, out, "[unreachable] [lost] [remote] feature",
		"Lost+unreachable is the honest pair: the mark cannot be re-confirmed now")
	assert.Contains(t, out, "unreachable 3m ago · ssh: connection reset · af-feature",
		"the outage detail leads and the branch trails it")
}
