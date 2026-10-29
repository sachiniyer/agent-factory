package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

func stubDaemonHealth(t *testing.T, resp daemon.PingResponse, err error) *int {
	t.Helper()
	calls := 0
	prev := daemonHealth
	daemonHealth = func(*apiclient.Client) (daemon.PingResponse, error) { calls++; return resp, err }
	t.Cleanup(func() { daemonHealth = prev })
	return &calls
}

// A daemon that does not affirm the capability must never receive a host pick
// from the TUI: it would drop the field and the session or task the user pinned
// to a host would run on the repo's default backend (#5571 W6.3, Codex P1).
func TestGateHostPickRefusesADaemonWithoutTheCapability(t *testing.T) {
	stubDaemonHealth(t, daemon.PingResponse{Version: "1.0.300"}, nil)

	for name, surface := range map[string]daemon.HostPickSurface{
		"create": daemon.HostPickOnCreate,
		"task":   daemon.HostPickOnTask,
	} {
		sent := false
		err := gateHostPick(nil, surface, "box1", func() error { sent = true; return nil })
		require.Errorf(t, err, "%s", name)
		assert.Falsef(t, sent, "%s: the field must not be sent to a daemon that cannot honor it", name)
		assert.Contains(t, err.Error(), "daemon (v1.0.300)")
		assert.Contains(t, err.Error(), "af daemon restart")
		assert.Contains(t, err.Error(), "nothing was created or changed")
	}
}

// Only the bit for THIS surface counts: a daemon that honors the create field but
// predates task hosts is still refused for a task.
func TestGateHostPickNeedsTheSurfacesOwnBit(t *testing.T) {
	stubDaemonHealth(t, daemon.PingResponse{HostPick: true, Version: "1.0.310"}, nil)

	sent := false
	require.NoError(t, gateHostPick(nil, daemon.HostPickOnCreate, "box1", func() error { sent = true; return nil }))
	assert.True(t, sent)

	sent = false
	require.Error(t, gateHostPick(nil, daemon.HostPickOnTask, "box1", func() error { sent = true; return nil }))
	assert.False(t, sent)
}

// An unanswerable probe refuses too: "could not verify" must not mean "send it".
func TestGateHostPickFailsClosedWhenUnverifiable(t *testing.T) {
	stubDaemonHealth(t, daemon.PingResponse{}, errors.New("connection refused"))
	sent := false
	err := gateHostPick(nil, daemon.HostPickOnTask, "box1", func() error { sent = true; return nil })
	require.Error(t, err)
	assert.False(t, sent)
	assert.Contains(t, err.Error(), "was not sent")
}

// No host, no gate: every non-host write is exactly what it was, and an older
// daemon is never probed for one.
func TestGateHostPickIsInertWithoutAHost(t *testing.T) {
	calls := stubDaemonHealth(t, daemon.PingResponse{}, nil)
	sent := false
	require.NoError(t, gateHostPick(nil, daemon.HostPickOnTask, "", func() error { sent = true; return nil }))
	assert.True(t, sent)
	assert.Zero(t, *calls, "a write with no host must not cost a health round trip")

	stubDaemonHealth(t, daemon.PingResponse{TaskHost: true}, nil)
	sent = false
	require.NoError(t, gateHostPick(nil, daemon.HostPickOnTask, "box1", func() error { sent = true; return nil }))
	assert.True(t, sent, "a capable daemon receives the pick")
}

// #5968: the capability probe is a READ. Whatever uncertainty it carries (a
// timeout after connecting, a lost or malformed response) must not make the
// refusal classify as a mutation that may have landed — nothing was sent, so the
// form keeps its draft.
func TestGateHostPickProbeFailureIsNeverAnUncertainMutation(t *testing.T) {
	for name, probeErr := range map[string]error{
		"transport": &apiclient.TransportError{Err: errors.New("i/o timeout")},
		"deadline":  context.DeadlineExceeded,
		"cancelled": context.Canceled,
	} {
		stubDaemonHealth(t, daemon.PingResponse{}, probeErr)
		sent := false
		err := gateHostPick(nil, daemon.HostPickOnTask, "box1", func() error { sent = true; return nil })
		require.Errorf(t, err, "%s", name)
		assert.Falsef(t, sent, "%s", name)
		assert.Falsef(t, apiclient.IsMutationOutcomeUncertain(err), "%s: a probe failure is not an uncertain mutation", name)
	}
}
