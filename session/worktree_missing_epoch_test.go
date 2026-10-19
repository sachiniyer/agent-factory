package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A probe that saw ENOENT while af's own move had the path empty must not be
// applied once that move has been rolled back to the same path. The undo of a
// committed archive is exactly that ABA: BeginRestore moves the worktree back to
// the path the probe asked about and AbortRestoreToLost clears the op, so the
// gw pointer, the path and OpNone all match again. Only the lifecycle epoch
// records that an operation began and finished while the probe was in flight
// (#5102).
func TestRefreshWorktreeMissing_ProbeSpanningARolledBackOperationIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	inst.SetStartedForTest(true)
	inst.liveness = LiveArchived // a committed archive, about to be undone

	previous := afterWorktreeMissingProbe
	t.Cleanup(func() { afterWorktreeMissingProbe = previous })
	afterWorktreeMissingProbe = func() {
		afterWorktreeMissingProbe = func() {}
		// The probe's ENOENT is in hand. The operation that made the path empty
		// now runs to its rollback: the worktree lands back at the same path.
		require.NoError(t, inst.Transition(BeginRestore()))
		require.NoError(t, os.MkdirAll(path, 0o755))
		require.NoError(t, inst.Transition(AbortRestoreToLost()))
	}

	missing, changed := inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a probe that spans a completed rollback must not flag a present worktree")
	assert.False(t, changed)
	gotMissing, reason := inst.WorktreeMissing()
	assert.False(t, gotMissing)
	assert.Empty(t, reason)
	assert.DirExists(t, path, "premise: the worktree is present again")

	// The next poll re-probes from scratch and answers truthfully.
	missing, _ = inst.RefreshWorktreeMissing()
	assert.False(t, missing, "a present path never sets the flag")
}
