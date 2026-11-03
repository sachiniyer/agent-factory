package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The setup-hook failure note is a durable, projection-only record like
// CallbackUnavailable and AgentConfigSkipped (#6667): a failing
// setup_commands entry does not block the session, but the row must say so —
// with the exit status and the retained output log — on every surface.

func TestSetupHookFailedNoteAndReconcile(t *testing.T) {
	i := &Instance{}
	assert.Equal(t, "", i.SetupHookFailed())

	i.noteSetupHookFailure("entry 1 failed")
	assert.Equal(t, "entry 1 failed", i.SetupHookFailed())

	// A second failure is joined, not dropped — the script can fail and the
	// later overlay can fail too; both belong on the row.
	i.noteSetupHookFailure("overlay failed")
	assert.Equal(t, "entry 1 failed; overlay failed", i.SetupHookFailed())

	assert.True(t, i.ReconcileSetupHookFailed("other"))
	assert.Equal(t, "other", i.SetupHookFailed())
	assert.False(t, i.ReconcileSetupHookFailed("other"))
}

// The note rides the same InstanceData projection + storage fields the other
// session-level warnings use, and survives the restore path.
func TestSetupHookFailedDataRoundTrip(t *testing.T) {
	i := &Instance{Title: "sess", Path: t.TempDir()}
	i.noteSetupHookFailure("entry 2 failed: exit status 3")

	data := i.ToInstanceData()
	assert.Equal(t, "entry 2 failed: exit status 3", data.SetupHookFailed)

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	assert.Equal(t, "entry 2 failed: exit status 3", restored.SetupHookFailed())
}
