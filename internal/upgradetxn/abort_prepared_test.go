package upgradetxn

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// AbortPreparedTransaction lets the initiating daemon undo a published-but-unstarted
// transaction so one transient InstallAndStart failure does not wedge every future
// upgrade at "already active" (#2212 R2b). It must remove a PhasePrepared journal
// with no live actor, and refuse the moment an actor has taken authority.
func TestAbortPreparedTransaction(t *testing.T) {
	txn, home, executable := prepareFixture(t)
	id := txn.Journal().ID

	require.NoError(t, AbortPreparedTransaction(home, id))
	_, err := Load(home)
	require.ErrorIs(t, err, ErrNoActiveTransaction, "abort must remove the active journal")

	// The wedge is cleared: a fresh upgrade can be prepared again.
	_, err = Prepare(Plan{
		ID: "txn-after-abort", HomeDir: home, ExecutablePath: executable,
		FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate-after-abort"),
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	})
	require.NoError(t, err, "a fresh upgrade must be preparable after aborting the wedged one")

	// Idempotent for the original id: it is no longer the active transaction.
	require.NoError(t, AbortPreparedTransaction(home, id))
}

func TestAbortPreparedTransaction_RefusesWhenActorLive(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	id := txn.Journal().ID
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	defer func() { _ = lease.Release() }()

	require.Error(t, AbortPreparedTransaction(home, id),
		"a live recovery actor holds the lock; abort must refuse rather than remove under it")
	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, id, loaded.Journal().ID, "the journal must survive a refused abort")
}

func TestAbortPreparedTransaction_RefusesPastPrepared(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	id := txn.Journal().ID
	lease, err := txn.tryAcquireRecoveryAs(txn.Journal().PreviousBinaryPath)
	require.NoError(t, err)
	require.NoError(t, lease.Advance(PhaseSupervisorReady))
	require.NoError(t, lease.Release()) // lock free, but the phase advanced past prepared

	require.Error(t, AbortPreparedTransaction(home, id),
		"once an actor advanced the phase it owns teardown; the initiating daemon must not abort")
	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, PhaseSupervisorReady, loaded.Journal().Phase)
}

func TestAbortPreparedTransaction_DifferentIdIsNoOp(t *testing.T) {
	txn, home, _ := prepareFixture(t)
	require.NoError(t, AbortPreparedTransaction(home, "some-other-transaction"),
		"aborting an id that is not the active transaction is a no-op success")
	loaded, err := Load(home)
	require.NoError(t, err)
	require.Equal(t, txn.Journal().ID, loaded.Journal().ID, "the real active transaction must survive")
}
