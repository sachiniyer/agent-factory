package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A daemon that does not affirm a surface's bit must be refused on that surface,
// whatever its other bits say: it would drop the field and run the work elsewhere.
func TestHostPickSurfaceRequire(t *testing.T) {
	createOnly := PingResponse{HostPick: true, Version: "1.0.310"}
	require.NoError(t, HostPickOnCreate.Require("--host", createOnly, nil, "box"))

	for name, s := range map[string]HostPickSurface{"deliver": HostPickOnDeliver, "task": HostPickOnTask} {
		err := s.Require("the host pick", createOnly, nil, "box")
		require.Errorf(t, err, "%s", name)
		assert.Contains(t, err.Error(), "daemon (v1.0.310)")
		assert.Contains(t, err.Error(), "af daemon restart")
		assert.Contains(t, err.Error(), "nothing was created or changed")
	}

	// A daemon old enough to report no version still gets an actionable message.
	err := HostPickOnTask.Require("the host pick", PingResponse{}, nil, "box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates version reporting")

	// Unverifiable is refused, not allowed through.
	err = HostPickOnTask.Require("--host", PingResponse{}, errors.New("connection refused"), "box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was not sent")

	// The probe error is quoted, not wrapped: it is a read failure, so a caller
	// classifying the result as a mutation outcome must not see its cause (#5968).
	probeErr := errors.New("i/o timeout")
	err = HostPickOnTask.Require("the host pick", PingResponse{}, probeErr, "box")
	assert.NotErrorIs(t, err, probeErr)

	full := PingResponse{HostPick: true, DeliverHostPick: true, TaskHost: true}
	for _, s := range []HostPickSurface{HostPickOnCreate, HostPickOnDeliver, HostPickOnTask} {
		require.NoError(t, s.Require("--host", full, nil, "box"))
	}
}
