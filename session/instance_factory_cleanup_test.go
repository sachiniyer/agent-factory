package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewInstance_BindFailureSurfacesCleanupOutcome covers the create-time twin
// of restore binding failure. No Instance exists to retain a retry handle here,
// so every cleanup error must remain in the returned chain, including the
// distinct unknown-state sentinel.
func TestNewInstance_BindFailureSurfacesCleanupOutcome(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cleanupErr error
	}{
		{name: "known failure", cleanupErr: errors.New("runtime answered with a cleanup failure")},
		{name: "unknown", cleanupErr: fmt.Errorf("%w: cleanup timed out", ErrWorkspaceStateUnknown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousFactory := backendFactory
			backendFactory = func(InstanceOptions, string) (ProvisionResult, error) {
				return ProvisionResult{
					Backend:  newInertSandboxBackend("docker"),
					Endpoint: &AgentServerEndpoint{URL: "://invalid", Token: "tok"},
					Teardown: func() error { return tc.cleanupErr },
				}, nil
			}
			defer func() { backendFactory = previousFactory }()

			_, err := NewInstance(InstanceOptions{Title: "s", Path: t.TempDir(), Program: "claude"})
			require.ErrorContains(t, err, "failed to build remote agent-server client")
			require.ErrorIs(t, err, tc.cleanupErr, "cleanup outcome must not disappear from the create error")
			if TeardownStateUnknown(tc.cleanupErr) {
				require.ErrorIs(t, err, ErrWorkspaceStateUnknown)
			}
		})
	}
}
