package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// validateDirectoryNoSymlink must accept a real directory reached through a
// symlinked ANCESTOR (a symlinked home, macOS /var -> /private/var) — rejecting that
// wedged the whole upgrade engine, all 14 durable-fs call sites, on those boxes —
// while still rejecting a symlinked LEAF, which is the swap defense (#2110 class).
func TestValidateDirectoryNoSymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))

	linkAncestor := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(base, linkAncestor)) // link -> base
	require.NoError(t, validateDirectoryNoSymlink(filepath.Join(linkAncestor, "real")),
		"a real directory reached through a symlinked ancestor must be accepted")

	leaf := filepath.Join(base, "leaflink")
	require.NoError(t, os.Symlink(real, leaf))
	require.Error(t, validateDirectoryNoSymlink(leaf),
		"a directory that is itself a symlink must be rejected (the swap defense)")

	require.ErrorIs(t, validateDirectoryNoSymlink(filepath.Join(base, "absent")), os.ErrNotExist,
		"a missing directory must surface ErrNotExist for the create/skip callers")

	file := filepath.Join(base, "afile")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	require.Error(t, validateDirectoryNoSymlink(file), "a non-directory must be rejected")
}

// The abort must succeed when the AF home is reached through a symlink — the exact
// darwin failure, reproduced on linux with an explicit symlink. Before the shared
// validator fix, removeRequiredDurableFile refused the symlinked directory path and
// the un-wedge primitive was itself wedged.
func TestAbortPreparedTransaction_SymlinkedHome(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(realHome, 0o755))
	linkHome := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realHome, linkHome)) // like /var -> /private/var

	exe := filepath.Join(base, "af")
	require.NoError(t, os.WriteFile(exe, []byte("previous-af-binary"), 0o755))

	txn, err := Prepare(Plan{
		ID: "upgrade-symlink", HomeDir: linkHome, ExecutablePath: exe,
		FromVersion: "1.0.100", ToVersion: "1.0.200", Candidate: []byte("candidate-symlink"),
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	})
	require.NoError(t, err)
	require.NoError(t, AbortPreparedTransaction(linkHome, txn.Journal().ID),
		"abort must succeed when the home is reached through a symlink")
	_, loadErr := Load(linkHome)
	require.ErrorIs(t, loadErr, ErrNoActiveTransaction)
}

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
