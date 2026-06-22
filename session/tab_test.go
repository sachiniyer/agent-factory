package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAgentTab_WrapsSession confirms the agent-tab constructor produces an
// Agent-kind tab named "agent" that wraps the given tmux session — the single
// default tab introduced in PR 1 of #930.
func TestNewAgentTab_WrapsSession(t *testing.T) {
	ts := tmux.NewTmuxSession("agent-tab", "claude")
	tab := newAgentTab(ts)

	assert.Equal(t, agentTabName, tab.Name)
	assert.Equal(t, TabKindAgent, tab.Kind)
	assert.Empty(t, tab.Command)
	assert.Same(t, ts, tab.tmux)
}

// TestInstanceTmuxAccessors_MaterializeSingleAgentTab verifies the Instance
// tmux accessors route the single agent session through Tabs[0]: the first
// assignment materializes exactly one Agent tab, later reads/writes reuse it,
// and clearing leaves the tab in place with a nil session (so a restored
// instance still has its agent tab, matching the old i.tmuxSession == nil
// semantics).
func TestInstanceTmuxAccessors_MaterializeSingleAgentTab(t *testing.T) {
	i := &Instance{Title: "accessor"}

	// Before any assignment there is no agent tab and no session.
	assert.Empty(t, i.Tabs)
	assert.Nil(t, i.tmuxLocked())

	// Clearing before the tab exists is a no-op (no empty tab is created).
	i.setTmuxLocked(nil)
	assert.Empty(t, i.Tabs)

	// First non-nil assignment materializes exactly one Agent tab.
	ts := tmux.NewTmuxSession("accessor", "claude")
	i.setTmuxLocked(ts)
	require.Len(t, i.Tabs, 1)
	assert.Equal(t, TabKindAgent, i.Tabs[0].Kind)
	assert.Same(t, ts, i.tmuxLocked())

	// Reassignment reuses the same tab rather than appending a second one.
	ts2 := tmux.NewTmuxSession("accessor", "claude")
	i.setTmuxLocked(ts2)
	require.Len(t, i.Tabs, 1)
	assert.Same(t, ts2, i.tmuxLocked())

	// Clearing leaves the agent tab in place but drops the session.
	i.setTmuxLocked(nil)
	require.Len(t, i.Tabs, 1)
	assert.Nil(t, i.tmuxLocked())
}

// TestSetTmuxSession_RoutesThroughAgentTab confirms the public test helper
// SetTmuxSession routes through the agent tab so existing white-box tests and
// UI tests keep working after the field was replaced by Tabs[0].
func TestSetTmuxSession_RoutesThroughAgentTab(t *testing.T) {
	i := &Instance{Title: "helper"}
	ts := tmux.NewTmuxSession("helper", "claude")

	i.SetTmuxSession(ts)
	require.Len(t, i.Tabs, 1)
	assert.Equal(t, TabKindAgent, i.Tabs[0].Kind)
	assert.Same(t, ts, i.tmuxLocked())
}

// TestToInstanceData_PersistsAgentTabSessionName confirms the agent tab's tmux
// session name is what ToInstanceData serializes as the single TmuxName — the
// on-disk format is unchanged in PR 1 (no Tabs field yet).
func TestToInstanceData_PersistsAgentTabSessionName(t *testing.T) {
	i := &Instance{Title: "persist", backend: &LocalBackend{}}
	ts := tmux.NewTmuxSessionForRepo("persist", t.TempDir(), "claude")
	i.SetTmuxSession(ts)

	data := i.ToInstanceData()
	assert.Equal(t, ts.SanitizedName(), data.TmuxName)
}
