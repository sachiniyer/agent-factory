package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestCreateNewTab_AttachCollisionRawError_BUG guards the fix for the
// "misleading attach error after a confirmed daemon create" defect.
// After createTabThroughDaemon succeeds, a name-collision in the stale local
// roster causes resolveAttachedTabLocked to return an error; createNewTab
// must surface a "created" notice (the daemon has the tab; the next snapshot
// reconciles the local roster), not a raw "cannot attach daemon tab …"
// failure that reads as total failure and invites a retry that spawns a
// second tab (#4820).
func TestCreateNewTab_AttachCollisionRawError_BUG(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "collision")
	selectInstance(h, inst)

	localTabs := inst.GetTabs()
	require.Equal(t, "shell", localTabs[1].Name, "helper creates one shell tab named shell")
	require.NotEmpty(t, localTabs[1].ID, "the local shell tab carries a non-empty ID")

	tmuxName := inst.TabTmuxName(0) + "__shell"
	if spawn := daemonSpawnHooks[inst]; spawn != nil {
		spawn(tmuxName)
	}

	var createCalls int
	t.Cleanup(SetTabCreatorForTest(func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		createCalls++
		return daemon.CreateTabResponse{
			ID:       "daemon-shell-2",
			Name:     "shell",
			TmuxName: tmuxName,
		}, nil
	}))

	_, _ = h.createNewTab(inst, session.TabKindShell)

	require.Equal(t, 1, createCalls, "the daemon CreateTab RPC must have been called once")
	assert.NotContains(t, h.errBox.FullError(), "cannot attach daemon tab",
		"the daemon created the tab; the message must not read as a total failure that invites retry")
	assert.Contains(t, h.errBox.FullError(), "created",
		"the message should acknowledge the daemon created the tab so the user does not retry")
}
