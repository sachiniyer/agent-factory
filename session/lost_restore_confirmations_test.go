package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
)

// A persisted pending-swap row loads inert but stays scheduler-eligible, and
// the live swap flow deliberately leaves the confirmed-flap history alone.
// A restart inside that window must not retire the durable copy the running
// daemon would have kept (#5436 review).
func TestFromInstanceData_PendingAccountSwapKeepsFlapHistory(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	confs := []time.Time{time.Now().Add(-time.Minute), time.Now()}
	data := deadInstanceData(t, Running, "af_swap_flap", "af_swap_flap"+tmuxTabSeparator+shellTabName)
	data.PendingAccountSwap = &AccountSwapData{To: "work"}
	data.LostRestoreConfirmations = confs

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, confs, restored.LostRestoreConfirmationsSnapshot(),
		"the swap-pending load is the exception to inert clearing — the episode resumes, so its history must")
}

// The unreachable inert branches still drop the field: a row no automatic
// restore can reach can never feed the bound, and the explicit path back
// starts a fresh episode.
func TestFromInstanceData_StartupUnknownDropsFlapHistory(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := deadInstanceData(t, Running, "af_unknown_flap", "af_unknown_flap"+tmuxTabSeparator+shellTabName)
	data.StartupStateUnknown = true
	data.LostRestoreConfirmations = []time.Time{time.Now()}

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Empty(t, restored.LostRestoreConfirmationsSnapshot(),
		"an unreachable row must not keep confirmations no code path can consume")
}
