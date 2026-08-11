package tree

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleReasonDetailUsesOnlyProjectedFactAndPaneAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	churnAt := now.Add(-2*time.Hour - 10*time.Minute)
	assert.Equal(t,
		"pane changed · 2h ago",
		idleReasonDetail(session.IdleReasonSettledAfterPaneChange, churnAt, now))
	assert.Empty(t, idleReasonDetail(session.IdleReason("future-value"), churnAt, now))
}

func TestInstanceRendererSurfacesIdleReasonBeforeBranch(t *testing.T) {
	t.Parallel()

	attemptedAt := time.Now().Add(-time.Hour)
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "worker",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	inst.Branch = "feature"
	inst.SetStatusForTest(session.Ready)
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, attemptedAt))

	r := NewInstanceRenderer()
	r.SetWidth(80)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	var secondary string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, branchIcon) {
			secondary = line
			break
		}
	}
	require.NotEmpty(t, secondary)
	assert.Contains(t, secondary, "no change after delivery")
	assert.Less(t, strings.Index(secondary, "no change"), strings.Index(secondary, "feature"))
}
