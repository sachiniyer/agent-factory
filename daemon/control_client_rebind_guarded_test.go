package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for #4822's mixed-version rule on the socket transport — the same
// fail-closed gate the HTTP client applies (apiclient/rebind_guarded_test.go
// covers that twin). A guarded send against a daemon that answers Ping but
// carries no guarded_rebind bit must be refused before the mutation goes out:
// that daemon would silently apply the move unconditionally, turning the
// compare-and-set into last writer wins.

func TestRebindProjectGuardedRefusesOnAPreCapabilityDaemon(t *testing.T) {
	old := pingGuardedRebindCapability
	pingGuardedRebindCapability = func() (PingResponse, error) {
		return PingResponse{Version: "0.9.0"}, nil // answered; no bit
	}
	t.Cleanup(func() { pingGuardedRebindCapability = old })

	_, err := RebindProject(RebindProjectRequest{
		ID:                 "prj_x",
		Path:               "/n",
		ExpectedRoot:       "/r",
		ExpectedCheckoutID: "chk-observed",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrGuardedRebindUnsupported),
		"a daemon that omits the capability must refuse the guarded send, got %v", err)
	assert.Contains(t, err.Error(), "daemon host")
}

func TestRebindProjectGuardedRefusesWhenTheProbeCannotAnswer(t *testing.T) {
	old := pingGuardedRebindCapability
	pingGuardedRebindCapability = func() (PingResponse, error) {
		return PingResponse{}, errors.New("socket unreadable")
	}
	t.Cleanup(func() { pingGuardedRebindCapability = old })

	_, err := RebindProject(RebindProjectRequest{
		ID:           "prj_x",
		Path:         "/n",
		ExpectedRoot: "/r",
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrGuardedRebindUnsupported),
		"a capability check that cannot answer is still fail-closed, got %v", err)
}

func TestRebindProjectGuardedSendProceedsPastACapablePing(t *testing.T) {
	old := pingGuardedRebindCapability
	pingGuardedRebindCapability = func() (PingResponse, error) {
		return PingResponse{Version: "9.9.9", GuardedRebind: true}, nil
	}
	t.Cleanup(func() { pingGuardedRebindCapability = old })

	// The gate passes and the send reaches the transport — in the sandboxed
	// home there is no daemon to answer, so whatever comes back is a
	// dial/ensure/registry error, never the unsupported sentinel.
	_, err := RebindProject(RebindProjectRequest{
		ID:                 "prj_x",
		Path:               "/n",
		ExpectedRoot:       "/r",
		ExpectedCheckoutID: "chk-observed",
	})
	require.Error(t, err, "no daemon answers in the test sandbox")
	assert.False(t, errors.Is(err, ErrGuardedRebindUnsupported),
		"a daemon that advertises the capability must not be refused, got %v", err)
}
