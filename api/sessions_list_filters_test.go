package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// TestSessionsList_FilterFlagsReachSnapshot pins #3362 at the CLI/RPC seam:
// flags must become additive Snapshot request fields so the daemon can remove
// rows before the response crosses the socket.
func TestSessionsList_FilterFlagsReachSnapshot(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	t.Chdir(mkRepo(t, "filtered"))

	setSessionsListFlag(t, "live", "true")
	setSessionsListFlag(t, "status", "archived")
	setSessionsListFlag(t, "status", "running")
	setSessionsListFlag(t, "max-age", "2h")
	setSessionsListFlag(t, "limit", "3")

	reqs := stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{}, nil
	})
	captureSessionsList(t)
	require.Len(t, *reqs, 1)

	raw, err := json.Marshal((*reqs)[0])
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, true, got["live"])
	require.Equal(t, []any{"archived", "running"}, got["statuses"])
	require.Equal(t, float64(3), got["limit"])

	createdAfter, ok := got["created_after"].(string)
	require.True(t, ok, "created_after must be an RFC3339 JSON timestamp")
	bound, err := time.Parse(time.RFC3339Nano, createdAfter)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(-2*time.Hour), bound, 5*time.Second)
}

// TestSessionsList_NoFilterFlagsKeepTheRequestAndOutputUnchanged guards the
// additive contract: the zero values are omitted and the daemon's full response
// remains byte-for-byte the list command's result.
func TestSessionsList_NoFilterFlagsKeepTheRequestAndOutputUnchanged(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)
	repo := mkRepo(t, "default")
	t.Chdir(repo)

	want := []session.InstanceData{{Title: "archived"}, {Title: "live"}}
	reqs := stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return want, nil
	})
	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	require.NoError(t, err)
	wantJSON = append(wantJSON, '\n')
	require.Equal(t, wantJSON, out, "the no-filter CLI payload must remain byte-identical")
	require.Len(t, *reqs, 1)

	raw, err := json.Marshal((*reqs)[0])
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, 1, "no new filter field may appear without its flag")
	require.Contains(t, got, "repo_id")
}

func setSessionsListFlag(t *testing.T, name, value string) {
	t.Helper()
	flag := sessionsListCmd.Flags().Lookup(name)
	require.NotNil(t, flag, "sessions list must expose --%s", name)
	require.NoError(t, flag.Value.Set(value))
	flag.Changed = true
	t.Cleanup(func() {
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			_ = slice.Replace(nil)
		} else {
			_ = flag.Value.Set(flag.DefValue)
		}
		flag.Changed = false
	})
}
