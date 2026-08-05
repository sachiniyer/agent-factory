package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2737: a pre-#2704 SSH tombstone recorded no host-key posture, so it restores
// as strict. When the host was only ever learned in the af-owned accept-new
// store, the reconnect can never verify it — and the daemon retried that once a
// second forever. The failure must say it cannot be fixed by retrying so the
// record is retired instead.
func TestSSHReapMarksLegacyTombstoneUnusable(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "legacy-tombstone"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/legacy.1234",
		// hostKeyVerification is EMPTY: the field #2704 added, absent here.
	}
	p.reapDial = func() error { return errors.New("ssh: handshake failed: knownhosts: key is unknown") }

	err := p.reap()
	require.Error(t, err)
	assert.True(t, CleanupHandleUnusable(err),
		"a legacy tombstone that cannot verify its host can never be completed by retrying")
	assert.True(t, TeardownStateUnknown(err),
		"but the workspace state is still unknown, so the record must still be RETAINED")
	assert.Contains(t, err.Error(), "known_hosts", "the error must tell the operator what would fix it")
	assert.Contains(t, err.Error(), "#2704")
}

// A tombstone that DID record its posture is an ordinary retryable failure: the
// host may simply be down, and an outage that ends must heal on the next attempt.
func TestSSHReapKeepsRetryingWhenPostureWasRecorded(t *testing.T) {
	for _, posture := range []string{"strict", "accept-new"} {
		t.Run(posture, func(t *testing.T) {
			p := &sshProvisioner{
				spec:                ProvisionSpec{Title: "modern-tombstone"},
				cfg:                 configSSHForReapTest(),
				sessionDir:          "/home/remote/.af-sessions/modern.1234",
				hostKeyVerification: posture,
			}
			p.reapDial = func() error { return errors.New("dial tcp: connect: connection refused") }

			err := p.reap()
			require.Error(t, err)
			assert.False(t, CleanupHandleUnusable(err),
				"a recorded posture with an unreachable host may heal, so it must keep being retried")
			assert.True(t, TeardownStateUnknown(err))
		})
	}
}

// The two sentinels compose: retiring stops the retrying, never the retention.
func TestUnusableHandleStillRetainsTheRecord(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrCleanupHandleUnusable,
		fmt.Errorf("%w: reconnect failed", ErrWorkspaceStateUnknown))
	assert.True(t, CleanupHandleUnusable(err))
	assert.True(t, TeardownStateUnknown(err),
		"deleteSessionRecord must still refuse, so nothing is silently orphaned")
}
