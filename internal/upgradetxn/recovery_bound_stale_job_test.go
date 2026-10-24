package upgradetxn

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestActiveTransactionStillNamesInvocationChecksRawIDWhenValidationFails pins
// the P2 fix for the superseding-journal-whose-validation-fails shape: when
// transaction B has superseded a stale job for A but B's journal fails full
// validation (for example, because B's transaction directory is missing in a
// nonterminal phase), Load returns an error even though active.json can still be
// decoded and identifies B. Before the fix, activeTransactionStillNamesInvocation
// returned true for every non-ErrNoActiveTransaction error, so A's pre-record
// path recreated transactions/A and ran repeatedly against B instead of
// immediately disabling the stale job and leaving orphaned A storage. The raw
// journal ID is compared before treating a validation failure as a match: a
// readable journal that names a different transaction yields false (the stale
// job stands down), while a corrupt or unreadable journal that cannot reveal a
// different id still yields true (the budget must bound the corrupt-journal
// persistent trigger).
func TestActiveTransactionStillNamesInvocationChecksRawIDWhenValidationFails(t *testing.T) {
	invocation := launchdBoundFixture(t)
	loaded, err := Load(invocation.HomeDir)
	require.NoError(t, err)
	require.Equal(t, PhasePrepared, loaded.Journal().Phase,
		"the fixture must be in a nonterminal phase so a missing transaction directory fails validation")
	activeID := loaded.Journal().ID

	// Remove the active transaction's directory so Load fails storage validation
	// (a nonterminal phase with a missing transaction directory), but active.json
	// still decodes and identifies the active transaction. A stale job for a
	// different transaction id must not be treated as the active invocation:
	// the raw journal id is readable and names a different transaction, so the
	// pre-record must not recreate the stale job's directory.
	require.NoError(t, os.RemoveAll(transactionDir(invocation.HomeDir, activeID)))

	stale := RecoveryInvocation{HomeDir: invocation.HomeDir, TransactionID: activeID + "-stale"}
	require.False(t, activeTransactionStillNamesInvocation(stale),
		"a superseding journal whose id is readable but whose full validation fails must still disable the stale job, not recreate its directory")

	// The active transaction that names this invocation (even though its full
	// validation fails) must still be bounded: readJournal decodes it and the id
	// matches, so the budget applies (ensureRecoveryRetryCounterDir handles the
	// missing nonterminal storage separately).
	require.True(t, activeTransactionStillNamesInvocation(invocation),
		"a readable journal that names this invocation but fails full validation must still be bounded")

	// A corrupt journal (the bug report's primary persistent trigger) cannot
	// reveal a different id, so the budget still applies even though Load fails
	// with a non-ErrNoActiveTransaction error.
	require.NoError(t, os.WriteFile(activeJournalPath(invocation.HomeDir), []byte("{not-valid-json"), 0o600))
	require.True(t, activeTransactionStillNamesInvocation(stale),
		"a corrupt journal must still be bounded even for a stale id (the budget cannot tell it apart from the active invocation)")
}

// TestBoundLaunchdRecoveryStaleJobRemovesOrphanedRecoveryLog pins the P2 fix
// for the orphaned recovery log a stale launchd invocation leaves behind: when
// a delayed or final launchd restart fires after terminal cleanup removed
// active.json, launchd has already recreated the per-transaction recovery log
// at upgrade/recovery-<id>.log (it opens StandardOutPath/StandardErrorPath,
// creating them when absent, before spawning the actor), so the stand-down that
// disables the stale job must also unlink that log — no transaction remains to
// invoke cleanupInactiveArtifacts again, so without this removal the tail
// restart permanently leaves the orphaned log the new cleanup code is intended
// to eliminate. The removal is best-effort (a missing log is a no-op) and uses
// removeDurableFile (the same durable path the cleanup uses) so a power loss
// after the unlink cannot leave the log reappearing.
func TestBoundLaunchdRecoveryStaleJobRemovesOrphanedRecoveryLog(t *testing.T) {
	// Force the Darwin fallback so the stale-launchd-job budget path runs on a
	// Linux CI runner.
	origFallback := launchdFallbackOnCorruptJournal
	launchdFallbackOnCorruptJournal = func() bool { return true }
	t.Cleanup(func() { launchdFallbackOnCorruptJournal = origFallback })

	invocation := launchdBoundFixture(t)
	home := invocation.HomeDir
	rt := &fakeRecoveryJobRuntime{launchdUserID: 501}
	controller := rt.controller()

	// Terminal cleanup removed active.json (the completed transaction is gone),
	// and launchd recreated the per-transaction recovery log at
	// upgrade/recovery-<id>.log when it spawned this stale invocation. The
	// cleanup that would remove the log (cleanupInactiveArtifacts) already ran
	// for the completed transaction and will not run again (no transaction
	// remains), so the stand-down is the last chance to unlink the orphaned log
	// launchd recreated.
	require.NoError(t, os.Remove(activeJournalPath(home)))
	logPath := launchdRecoveryLogPath(home, invocation.TransactionID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte("launchd recreated stale recovery log"), 0o600))
	require.FileExists(t, logPath)

	ran := false
	err := boundLaunchdRecovery(context.Background(), invocation, controller, time.Now, func() error {
		ran = true
		return nil
	})
	require.NoError(t, err, "a stale job with no active transaction must stand down (exit 0)")
	require.False(t, ran, "the actor must not run for a stale job with no active transaction")
	require.Equal(t, []recoveryCommandCall{{
		name: "launchctl",
		args: []string{"disable", "gui/501/" + launchdRecoveryJobName(invocation.TransactionID)},
	}}, rt.commands, "the stale job must be disabled so RunAtLoad cannot re-launch it")

	_, statErr := os.Stat(logPath)
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"the orphaned recovery log launchd recreated for the stale invocation must be removed by the stand-down")

	// A stale job whose log launchd did not recreate (the job never reached the
	// invocation, or cleanup already removed it) must clean up without error:
	// removeDurableFile treats os.ErrNotExist as nil, the way the cleanup path
	// does for a job that never ran. active.json is already gone (removed above)
	// and the log is absent, so the stand-down is a clean no-op on the log.
	staleDir := transactionDir(home, invocation.TransactionID)
	require.NoError(t, os.RemoveAll(staleDir))
	err = boundLaunchdRecovery(context.Background(), invocation, controller, time.Now, func() error {
		return nil
	})
	require.NoError(t, err, "a stale job whose log is absent must stand down without a spurious removal error")
}

// TestReadRecoveryRetryCounterRefusesSpecialFile pins the P2 fix for a special
// file at the counter path: a FIFO (or a symlink to one) at recovery.attempts
// would block os.ReadFile indefinitely for a writer, hanging the budget
// pre-check before the actor can run recovery or disable itself, so the
// unbounded relaunch loop this budget exists to stop could not stand down. The
// counter is opened no-follow and non-blocking and the opened inode is verified
// to be a regular file; a FIFO, a device, or a symlink is treated as an invalid
// counter (the budget resets) instead of blocking.
func TestReadRecoveryRetryCounterRefusesSpecialFile(t *testing.T) {
	invocation := launchdBoundFixture(t)
	counterPath := recoveryRetryCounterPath(invocation.HomeDir, invocation.TransactionID)
	require.NoError(t, os.MkdirAll(filepath.Dir(counterPath), 0o700))

	// A FIFO at the counter path must not block the read or be treated as a
	// valid counter; the budget resets (returns false) the way a corrupt or
	// missing counter does.
	require.NoError(t, syscall.Mkfifo(counterPath, 0o600))
	_, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "a FIFO at the counter path must not block or be treated as a valid counter")

	// A symlink at the counter path (to a regular file or a FIFO) is rejected
	// by O_NOFOLLOW: the budget resets instead of following the symlink or
	// blocking on the target.
	require.NoError(t, os.Remove(counterPath))
	target := filepath.Join(filepath.Dir(counterPath), "counter-target")
	require.NoError(t, os.WriteFile(target, []byte(`{"attempts":3}`), 0o600))
	require.NoError(t, os.Symlink(target, counterPath))
	_, ok = readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.False(t, ok, "a symlink at the counter path must not be followed by the counter read")

	// A regular counter file still reads correctly so the budget is not
	// regressed for the normal path.
	require.NoError(t, os.Remove(counterPath))
	require.NoError(t, os.WriteFile(counterPath, []byte(`{"attempts":2,"first_attempt_at":"2026-01-01T00:00:00Z"}`), 0o600))
	counter, ok := readRecoveryRetryCounter(invocation.HomeDir, invocation.TransactionID)
	require.True(t, ok, "a regular counter file must still read correctly")
	require.Equal(t, 2, counter.Attempts, "a regular counter file must still decode the attempts")
}
