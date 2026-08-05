package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"
)

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

// Review finding on #2855: a legacy tombstone whose reconnect fails for a
// TRANSIENT reason must not be retired. Retiring on any dial error turns a host
// outage into a permanent stop, which is the failure #1122 is about — only the
// unknown-host-key signal proves the missing posture is what blocks the cleanup.
func TestSSHReapKeepsRetryingLegacyTombstoneOnTransientDialFailure(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"connection refused", errors.New("dial tcp 10.0.0.9:22: connect: connection refused")},
		{"timeout", errors.New("dial tcp 10.0.0.9:22: i/o timeout")},
		{"dns failure", errors.New("dial tcp: lookup remote.invalid: no such host")},
		{"auth failure", errors.New("ssh: unable to authenticate")},
	}
	for _, test := range transient {
		t.Run(test.name, func(t *testing.T) {
			p := &sshProvisioner{
				spec:       ProvisionSpec{Title: "legacy-transient"},
				cfg:        configSSHForReapTest(),
				sessionDir: "/home/remote/.af-sessions/legacy.1234",
			}
			p.reapDial = func() error { return test.err }

			err := p.reap()
			require.Error(t, err)
			assert.False(t, CleanupHandleUnusable(err),
				"a transient reconnect failure may heal, so a legacy record must keep being retried")
			assert.True(t, TeardownStateUnknown(err))
		})
	}
}

// The permanent case is specifically knownhosts' "host not present" signal.
func TestSSHReapRetiresLegacyTombstoneOnUnknownHostKey(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "legacy-unknown-host"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/legacy.1234",
	}
	p.reapDial = func() error { return &knownhosts.KeyError{} }

	err := p.reap()
	require.Error(t, err)
	assert.True(t, CleanupHandleUnusable(err),
		"an unrecorded posture plus an unverifiable host can never be completed by retrying")
	assert.True(t, TeardownStateUnknown(err), "and the record is still retained")
	assert.Contains(t, err.Error(), "known_hosts", "the error must tell the operator what would fix it")
	assert.Contains(t, err.Error(), "#2704")
}

// A key MISMATCH is not the same claim: the host is present and its key changed,
// which is a security event to surface, not a missing-field record to retire.
func TestSSHReapDoesNotRetireLegacyTombstoneOnKeyMismatch(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "legacy-mismatch"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/legacy.1234",
	}
	p.reapDial = func() error { return &knownhosts.KeyError{Want: []knownhosts.KnownKey{{}}} }

	err := p.reap()
	require.Error(t, err)
	assert.False(t, CleanupHandleUnusable(err), "a changed host key is not a missing-posture record")
}
