package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestPreviewCommitErrorRejectsArchived pins #1633: previewCommitError gates
// whether a split-pane preview can be committed (the `S` key) and whether the
// footer advertises split-pane as available. An archived session has no live
// tmux binding and pruneDeadPanes closes any pane bound to one, so committing a
// preview for it would flash a pane that immediately vanishes. It must be
// rejected exactly like Dead and Lost — the case that was originally omitted.
func TestPreviewCommitErrorRejectsArchived(t *testing.T) {
	inst := instanceWithFakeBackend(t, "archived-target")
	inst.SetArchived()
	require.Equal(t, session.LiveArchived, inst.GetLiveness())

	require.Error(t, previewCommitError(inst),
		"previewCommitError must reject an archived session so split-pane cannot commit a pane that pruneDeadPanes will immediately close")
}

// TestPreviewCommitErrorAcceptsRunning is the negative control: a live Running
// session (fake backend, no in-flight op) is a valid commit target, so the
// archived guard must not over-reject.
func TestPreviewCommitErrorAcceptsRunning(t *testing.T) {
	inst := instanceWithFakeBackend(t, "live-target")
	require.Equal(t, session.LiveRunning, inst.GetLiveness())

	require.NoError(t, previewCommitError(inst),
		"a live Running session is a valid split-pane commit target")
}
