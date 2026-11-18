package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// #7469: the recorded tmux-server identity rides the instance row exactly like
// the pane-root pair beside it — serialized, restored across a daemon restart,
// and withheld from client payloads (daemon-side process identity).
func TestRuntimeServerFields_SurviveSerializationAndStayDaemonSide(t *testing.T) {
	data := InstanceData{
		RuntimePID:             31337,
		RuntimeStartID:         123456789,
		RuntimeServerSocket:    "/tmp/tmux-1001/af",
		RuntimeServerPID:       4242,
		RuntimeServerStartID:   987654321,
		RuntimeServerSessionID: "$7",
		RuntimeProgram:         "claude",
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)

	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, data.RuntimeServerSocket, back.RuntimeServerSocket)
	require.Equal(t, data.RuntimeServerPID, back.RuntimeServerPID)
	require.Equal(t, data.RuntimeServerStartID, back.RuntimeServerStartID)
	require.Equal(t, data.RuntimeServerSessionID, back.RuntimeServerSessionID)

	client := data.ForClients()
	require.Empty(t, client.RuntimeServerSocket)
	require.Zero(t, client.RuntimeServerPID)
	require.Zero(t, client.RuntimeServerStartID)
	require.Empty(t, client.RuntimeServerSessionID,
		"server identity is daemon-side kill-path evidence — never shipped to clients")
	require.Equal(t, data.RuntimePID, client.RuntimePID, "the pane-root pair stays visible as before")
}

// A legacy row written before the server fields existed restores with an empty
// identity — undecidable, never a fabricated foreign verdict.
func TestRuntimeServerFields_LegacyRowRestoresEmpty(t *testing.T) {
	var back InstanceData
	require.NoError(t, json.Unmarshal([]byte(`{"runtime_pid":31337,"runtime_start_id":123}`), &back))
	require.Empty(t, back.RuntimeServerSocket)
	require.Zero(t, back.RuntimeServerPID)
}
