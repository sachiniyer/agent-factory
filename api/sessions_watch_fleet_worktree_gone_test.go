package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A tracked worktree deleted outside af (#5102) is a stop a driver must hear,
// not idle: the agent still answers probes, but send-prompt refuses it.
func TestClassifyWatchStop_WorktreeGone(t *testing.T) {
	gone := func(lv session.Liveness) session.InstanceData {
		d := withLiveness("s", lv)
		d.Worktree.Missing = true
		d.Worktree.MissingReason = "tracked worktree path /w/s does not exist (deleted outside af)"
		return d
	}
	for _, tc := range []struct {
		name   string
		data   session.InstanceData
		reason watchStopReason
	}{
		{"ready is not idle", gone(session.LiveReady), watchStopWorktreeGone},
		{"running is not working", gone(session.LiveRunning), watchStopWorktreeGone},
		{"lost cannot be restored in place", gone(session.LiveLost), watchStopWorktreeGone},
		{"archived stays archived", gone(session.LiveArchived), watchStopArchived},
		{"unflagged ready is still idle", withLiveness("s", session.LiveReady), watchStopIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, detail := classifyWatchStop(tc.data)
			require.Equal(t, tc.reason, reason)
			require.NotEmpty(t, detail)
		})
	}

	t.Run("an in-flight op is still motion", func(t *testing.T) {
		d := gone(session.LiveReady)
		d.InFlightOp = session.OpArchiving
		reason, _ := classifyWatchStop(d)
		require.Equal(t, watchWorking, reason,
			"af's own archive of the row is mid-transition, not a stop")
	})

	t.Run("an unrecognized liveness still reaches the upgrade branch", func(t *testing.T) {
		reason, _ := classifyWatchStop(gone(session.Liveness(9999)))
		require.Equal(t, watchStopUnknown, reason)
	})
}
