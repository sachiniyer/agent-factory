package tmux

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// TestStartObservationHookFiresAtBothLivenessObservations pins the contract
// tests elsewhere rely on to drive a pane's exit to a chosen side of Start's
// observations (#4406): each observation fires once, in order, on Start's own
// goroutine, before the probe it names — and a restored hook stops firing.
func TestStartObservationHookFiresAtBothLivenessObservations(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	testguard.IsolateTmux(t)
	t.Cleanup(PinServerProbeForTest())

	session := NewTmuxSession(fmt.Sprintf("observe-%d", time.Now().UnixNano()), "sleep 300")
	var reached []StartObservation
	var existedAtAttachProbe bool
	restore := SetStartObservationHookForTest(func(name string, at StartObservation) {
		if name != session.SanitizedName() {
			return
		}
		reached = append(reached, at)
		if at == StartBeforeAttachProbe {
			exists, known := session.ProbeSession()
			existedAtAttachProbe = known && exists
		}
	})
	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() { _, _ = session.Close() })
	require.Equal(t, []StartObservation{StartBeforeExistencePoll, StartBeforeAttachProbe}, reached)
	require.True(t, existedAtAttachProbe,
		"the attach-probe observation must come after the existence poll saw the session")

	restore()
	_, _ = session.Close()
	require.NoError(t, session.Start(t.TempDir()))
	require.Len(t, reached, 2, "a restored hook must not observe later starts")
}
