package upgradetxn

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AwaitSupervisorReady is the old-daemon trigger's POSITIVE observation that the
// recovery actor took the lease AND reached supervisor_ready (#2212 R2b). It must
// return nil ONLY on that proof — the recovery lock held (actor alive), the
// readiness lock published, the status at supervisor_ready, and a live deadline —
// and a loud error otherwise, never a silent pass on an unobservable readiness.
func TestAwaitSupervisorReady(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	released := false
	defer func() {
		if !released {
			_ = lease.Release()
		}
	}()

	ctx := context.Background()
	restore := awaitSupervisorReadyPoll
	awaitSupervisorReadyPoll = time.Millisecond
	defer func() { awaitSupervisorReadyPoll = restore }()

	// The actor holds the lease but has published NO status yet (tryAcquireRecoveryAs
	// invalidated the predecessor's): the missing proof is a loud error.
	require.Error(t, AwaitSupervisorReady(ctx, home, time.Now().Add(30*time.Millisecond)),
		"a lease with no published status must not be read as ready")

	// A status that EXISTS but reports a phase short of supervisor_ready must ALSO be
	// rejected — this exercises the phase check, distinct from the missing-status path
	// above (Heartbeat writes whatever phase it is given, so no Advance is needed).
	require.NoError(t, lease.Heartbeat(PhasePrepared, time.Now().Add(time.Minute)))
	require.Error(t, AwaitSupervisorReady(ctx, home, time.Now().Add(30*time.Millisecond)),
		"a published status short of supervisor_ready must be rejected on the phase check")

	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(time.Minute)))
	// The full positive proof now exists.
	require.NoError(t, AwaitSupervisorReady(ctx, home, time.Now().Add(2*time.Second)))

	// An expired readiness heartbeat is stale, not proof: the actor may be wedged.
	require.NoError(t, lease.Heartbeat(PhaseSupervisorReady, time.Now().Add(-time.Second)))
	require.Error(t, AwaitSupervisorReady(ctx, home, time.Now().Add(30*time.Millisecond)),
		"a supervisor_ready proof past its deadline must not authorize activation")

	// A dead actor (lease released) leaves the recovery lock free — not ready, even
	// though the last-published status still says supervisor_ready.
	require.NoError(t, lease.Release())
	released = true
	require.Error(t, AwaitSupervisorReady(ctx, home, time.Now().Add(30*time.Millisecond)),
		"a released lease means no live actor; readiness must not be assumed from a stale status")
}

// A cancelled context ends the wait promptly instead of blocking to the deadline.
func TestAwaitSupervisorReady_ContextCancel(t *testing.T) {
	_, home, _ := prepareFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, AwaitSupervisorReady(ctx, home, time.Now().Add(time.Hour)), context.Canceled)
}
