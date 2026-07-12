package ui

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// startedRemoteInstance builds a started remote (hook-backed) instance carrying
// exactly the single agent tab — the daemon-side tab model for a remote session
// after the #1592 Phase 4 PR7 provision-and-expose migration (identical to the
// docker/ssh baseline: no terminal/shell tab). The backend is the inert
// zero-value HookBackend, which reports full remote parity.
func startedRemoteInstance(t *testing.T) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: "remote-tabbar", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(&session.HookBackend{})
	require.True(t, inst.Capabilities().Workspace == session.WorkspaceRemote)
	inst.SetStartedForTest(true)
	inst.AddTabForTest("agent", session.TabKindAgent)
	return inst
}
