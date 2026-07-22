package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// Restore runs after the event-loop keypress. If the original row disappears
// before the tea.Cmd reaches the daemon, a title-only request can resurrect a
// different session that reused the display title.
func TestHandleRestore_DoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	original := archiveActionInstance(t, "worker", session.Lost)
	h.store.AddInstance(original)
	h.sidebar.SetSelectedInstance(0)

	var restored *session.Instance
	previous := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(title, repoID string) (string, error) {
		restored = h.store.GetInstanceByTitle(title)
		return "/restored", nil
	}
	t.Cleanup(func() { restoreSessionThroughDaemon = previous })

	_, cmd := h.handleRestore()
	require.NotNil(t, cmd)

	replacement := archiveActionInstance(t, original.Title, session.Lost)
	require.NotEqual(t, original.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstance(original, replacement))

	_ = cmd()
	require.NotSame(t, replacement, restored,
		"a queued restore must not resurrect a different same-title session")
}

// Restore completion is retained intent too. An old result must not confirm a
// replacement live or clear the replacement's own restore operation.
func TestHandleInstanceRestored_DoesNotMutateSameTitleReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		setup  func(*testing.T, *session.Instance)
		assert func(*testing.T, *session.Instance)
	}{
		{
			name: "success",
			assert: func(t *testing.T, replacement *session.Instance) {
				require.Equal(t, session.Lost, replacement.GetStatus(),
					"the old success must not mark the replacement live")
			},
		},
		{
			name: "failure",
			err:  errors.New("old restore failed"),
			setup: func(t *testing.T, replacement *session.Instance) {
				require.NoError(t, replacement.Transition(session.MarkRestoring()))
			},
			assert: func(t *testing.T, replacement *session.Instance) {
				require.Equal(t, session.OpRestoring, replacement.GetInFlightOp(),
					"the old failure must not clear the replacement's own restore")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			original := archiveActionInstance(t, "worker", session.Lost)
			h.store.AddInstance(original)

			replacement := archiveActionInstance(t, original.Title, session.Lost)
			require.NotEqual(t, original.ID, replacement.ID)
			if tc.setup != nil {
				tc.setup(t, replacement)
			}
			require.True(t, h.store.ReplaceInstance(original, replacement))

			_, _ = h.handleInstanceRestored(instanceRestoredMsg{title: original.Title, err: tc.err})
			tc.assert(t, replacement)
		})
	}
}
