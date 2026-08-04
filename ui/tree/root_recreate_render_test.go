package tree

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// The rail half of #2629. The whole complaint is that a root which came back
// without its history renders IDENTICALLY to one that resumed cleanly: same
// Ready dot, same title, same everything. So the assertion that matters is on
// the rendered row, not on the field behind it.

// TestRender_RootRecreateNote: a re-created root whose context did not survive
// says so on its row, in both of the two honest spellings.
func TestRender_RootRecreateNote(t *testing.T) {
	tests := []struct {
		name string
		ctx  session.RootRecreateContext
		want string
	}{
		{name: "context provably gone", ctx: session.RootRecreateContextFresh, want: "[fresh context] root"},
		{name: "continuity unknowable", ctx: session.RootRecreateContextUnknown, want: "[context unknown] root"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := session.NewInstance(session.InstanceOptions{Title: "root", Path: t.TempDir(), Program: "claude"})
			require.NoError(t, err)
			require.True(t, inst.ReconcileRootRecreateContext(tc.ctx))

			require.Contains(t, renderClean(t, inst), tc.want)
		})
	}
}

// TestRender_NoRootRecreateNoteByDefault: every ordinary row, and a root that
// resumed, must be unchanged. A note on rows that lost nothing is a note users
// learn to ignore.
func TestRender_NoRootRecreateNoteByDefault(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "worker", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)

	clean := renderClean(t, inst)
	require.NotContains(t, clean, "context")
	require.Contains(t, clean, "worker")
}

// TestRender_RootRecreateNoteClearsOnAcknowledgement: the row drops the note
// once the daemon reports it acknowledged. A rail that keeps rendering a
// cleared notice is the stale half of the same bug.
func TestRender_RootRecreateNoteClearsOnAcknowledgement(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{Title: "root", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	require.True(t, inst.ReconcileRootRecreateContext(session.RootRecreateContextFresh))
	require.Contains(t, renderClean(t, inst), "[fresh context] root")

	require.True(t, inst.ReconcileRootRecreateContext(session.RootRecreateContextNone))
	require.NotContains(t, renderClean(t, inst), "fresh context")
}
