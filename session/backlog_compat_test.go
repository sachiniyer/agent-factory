package session

// Compatibility test for session records the removed Backlog feature (#6662)
// wrote: an item session's record carries backlog_item plus the run/archive
// keys that no longer exist as fields. The record must still load; the item id
// survives the round-trip so the file remains meaningful to an older af that
// reads it later (additive + rollforward), while the dead keys drop quietly.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstanceData_BacklogEraRecordLoads(t *testing.T) {
	const record = `{
		"title": "B-7 pick a db",
		"program": "claude",
		"backlog_item": 7,
		"backlog_run_active": true,
		"backlog_run_ended_at": "2026-11-01T10:00:00Z",
		"backlog_archive": {"reason": "ask", "filed_at": "2026-11-01T10:05:00Z", "archived_at": "2026-11-01T10:06:00Z"}
	}`

	var data InstanceData
	require.NoError(t, json.Unmarshal([]byte(record), &data), "a Backlog-era record must still decode")
	data.Path = t.TempDir()
	// Leftover rows whose worktree never registered carry this marker; it lets
	// FromInstanceData skip the strict worktree-restore check the way it does
	// for those real stragglers.
	data.StartupStateUnknown = true
	require.Equal(t, 7, data.BacklogItem)

	inst, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, 7, inst.BacklogItem, "the item link the record carries survives the load")

	out, err := json.Marshal(inst.ToInstanceData())
	require.NoError(t, err)
	require.Contains(t, string(out), `"backlog_item":7`, "the identity field round-trips")
	require.NotContains(t, string(out), "backlog_run_active")
	require.NotContains(t, string(out), "backlog_archive")
}
