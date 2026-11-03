package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The record-delete choke point reclaims the session's daemon-side
// setup-payload staging dir at the exit — the ghost path's only reach, since
// a ghost has no live Instance whose kill drops it. A settled delete removes
// the dir; every direction that does not delete the record leaves it.
func stageSetupPayload(t *testing.T, id string) string {
	t.Helper()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	staging := filepath.Join(home, "af-setup", id, "payload")
	require.NoError(t, os.MkdirAll(staging, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "secret"), []byte("x"), 0o600))
	return filepath.Dir(staging)
}

func TestDeleteSessionRecordReclaimsSetupPayload(t *testing.T) {
	manager, repoID, inst := installRaceBackend(t, &raceBackend{}, "payload-ghost")
	staging := stageSetupPayload(t, inst.ID)

	deleted, err := manager.deleteSessionRecord(repoID, "payload-ghost", inst.ID, nil, session.InstanceData{})
	require.NoError(t, err)
	require.True(t, deleted)
	_, statErr := os.Stat(staging)
	assert.True(t, os.IsNotExist(statErr),
		"a settled record delete must reclaim the staging dir the ghost path could not reach")
}

func TestDeleteSessionRecordKeepsSetupPayloadOnUnknownTeardown(t *testing.T) {
	manager, repoID, inst := installRaceBackend(t, &raceBackend{}, "payload-kept")
	staging := stageSetupPayload(t, inst.ID)

	unknown := fmt.Errorf("kill %q: %w", "payload-kept", session.ErrPaneMayBeLive)
	deleted, err := manager.deleteSessionRecord(repoID, "payload-kept", inst.ID, unknown, session.InstanceData{})
	require.Error(t, err)
	require.False(t, deleted)
	_, statErr := os.Stat(staging)
	assert.NoError(t, statErr,
		"a refused delete keeps the record — and the payload a retry or sweep still owns")
}

func TestDeleteSessionRecordDifferentInstanceKeepsSetupPayload(t *testing.T) {
	manager, repoID, _ := installRaceBackend(t, &raceBackend{}, "payload-other")
	staging := stageSetupPayload(t, "other-stable-id")

	deleted, err := manager.deleteSessionRecord(repoID, "payload-other", "other-stable-id", nil, session.InstanceData{})
	require.NoError(t, err)
	require.False(t, deleted,
		"a same-titled record with a different instance identity must not delete")
	_, statErr := os.Stat(staging)
	assert.NoError(t, statErr,
		"the stale delete must not reclaim another session's payload")
}

// A legacy ghost row reaches the delete with an EMPTY stableID — the record
// delete falls back to title matching and succeeds, but with no id there is
// no staging dir to name, so the reclaim is skipped silently (the sweep owns
// whatever remains).
func TestDeleteSessionRecordEmptyStableIDSkipsReclaim(t *testing.T) {
	manager, repoID, inst := installRaceBackend(t, &raceBackend{}, "payload-legacy")
	staging := stageSetupPayload(t, inst.ID)

	deleted, err := manager.deleteSessionRecord(repoID, "payload-legacy", "", nil, session.InstanceData{})
	require.NoError(t, err)
	require.True(t, deleted, "title matching still settles a legacy ghost row")
	_, statErr := os.Stat(staging)
	assert.NoError(t, statErr,
		"an empty stable ID names no staging dir — the reclaim skips, and the sweep owns the leftover")
}
