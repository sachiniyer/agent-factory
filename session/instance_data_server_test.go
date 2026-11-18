package session

import (
	"encoding/json"
	"testing"
	"time"

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
		RuntimeServerPIDNS:     "pid:[4026531836]",
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
	require.Equal(t, data.RuntimeServerPIDNS, back.RuntimeServerPIDNS,
		"the recorded namespace must round-trip — restamping the restoring daemon's would defeat the namespace guards")

	client := data.ForClients()
	require.Empty(t, client.RuntimeServerSocket)
	require.Zero(t, client.RuntimeServerPID)
	require.Zero(t, client.RuntimeServerStartID)
	require.Empty(t, client.RuntimeServerSessionID)
	require.Empty(t, client.RuntimeServerPIDNS,
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

// #7469 R1: every launch boundary retires the row's server record BEFORE the
// new incarnation stamps — a respawn whose pane query then fails must persist
// ABSENT, never the previous incarnation's identity, so a stale (socket, old
// server pid) can never read the replacement's own survivors foreign.
func TestSetRuntimeLaunch_RetiresTheServerRecordFirst(t *testing.T) {
	i := &Instance{}
	i.runtimeServerSocket = "/tmp/tmux-1001/af"
	i.runtimeServerPID = 1111
	i.runtimeServerStartID = 555
	i.runtimeServerSessionID = "$3"
	i.runtimeServerPIDNS = "pid:[4026531836]"

	// ts == nil is the failed-stamp shape: the launch boundary still clears
	// the standing record rather than letting it ride into the new runtime.
	i.setRuntimeLaunch("claude", nil, time.Time{})

	i.mu.Lock()
	defer i.mu.Unlock()
	require.Empty(t, i.runtimeServerSocket, "a launch boundary retires the socket")
	require.Zero(t, i.runtimeServerPID, "a launch boundary retires the server pid")
	require.Zero(t, i.runtimeServerStartID)
	require.Empty(t, i.runtimeServerSessionID)
	require.Empty(t, i.runtimeServerPIDNS,
		"the previous incarnation's record never outlives the boundary — absent means undecidable")
}
