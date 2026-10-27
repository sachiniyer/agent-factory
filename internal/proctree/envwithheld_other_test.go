//go:build !darwin

package proctree

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWithheldEnvCauseIsSilentOffDarwin pins the other half of #3584's
// contract: an empty environment off darwin is real, so nothing may attribute
// it to a redaction policy.
func TestWithheldEnvCauseIsSilentOffDarwin(t *testing.T) {
	c := exec.Command("env", "-i", "sleep", "60")
	require.NoError(t, c.Start())
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	pid := c.Process.Pid
	require.Eventually(t, func() bool {
		_, st := LookupEnv(pid, "PATH")
		return st == EnvUnknown
	}, 5*time.Second, 10*time.Millisecond)

	_, ok := WithheldEnvCause(pid)
	require.False(t, ok)
}
