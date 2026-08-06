package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCandidateRejected_SurvivesARestart is the whole point of the ledger. #2908's
// quarantine is a map on the update driver, so a restart clears it and the six-hour
// check offers the same broken release again — an unattended box re-breaking itself
// on a loop. The read here goes to disk with no warm state in between, which is the
// only way to tell a durable record from a remembered one.
func TestCandidateRejected_SurvivesARestart(t *testing.T) {
	home := t.TempDir()
	candidate := []byte("the binary that failed validation")

	rejected, _, err := CandidateRejected(home, candidate)
	require.NoError(t, err, "a home that has never rolled anything back has no ledger, which is not an error")
	require.False(t, rejected, "premise: nothing is rejected yet")

	require.NoError(t, RecordRejectedCandidate(home, digest(candidate), "1.0.207", "rolled back"))

	// No shared handle, no cache: this is what a later boot sees.
	rejected, entry, err := CandidateRejected(home, candidate)
	require.NoError(t, err)
	require.True(t, rejected, "a candidate that reached rollback must stay refused across restarts")
	require.Equal(t, "1.0.207", entry.Version, "the operator needs to know WHICH release, not just that one was refused")
	require.False(t, entry.RejectedAt.IsZero())
}

// TestCandidateRejected_AdmitsARecutTagWithDifferentBytes is why the ledger is keyed
// on the digest rather than the tag, and it is the assertion that would fail if
// someone "simplified" it back to the gate's original wording. A tag-keyed ledger
// refuses the FIX for a bad release for the life of the box — a safety mechanism
// turned into a permanent block, which is the unoverridable shape #2859 was bitten
// by. Publishing a corrected build under the same tag must be the way out.
func TestCandidateRejected_AdmitsARecutTagWithDifferentBytes(t *testing.T) {
	home := t.TempDir()
	broken := []byte("1.0.207 as first published — broken")
	fixed := []byte("1.0.207 re-cut with the fix")

	require.NoError(t, RecordRejectedCandidate(home, digest(broken), "1.0.207", "rolled back"))

	rejected, _, err := CandidateRejected(home, broken)
	require.NoError(t, err)
	require.True(t, rejected, "the exact bytes that failed stay refused")

	rejected, _, err = CandidateRejected(home, fixed)
	require.NoError(t, err)
	require.False(t, rejected,
		"a corrected build under the SAME tag must be installable, or the box is stuck on that release forever")
}

// TestCandidateRejected_UnreadableLedgerErrorsRatherThanAdmitting pins the failure
// direction. Returning "nothing is rejected" from a damaged ledger would silently
// re-arm every release this box has rolled back — and a damaged file is exactly when
// it can least afford to reinstall a broken binary.
func TestCandidateRejected_UnreadableLedgerErrorsRatherThanAdmitting(t *testing.T) {
	t.Run("corrupt bytes", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, RecordRejectedCandidate(home, digest([]byte("bad")), "1.0.207", "rolled back"))
		require.NoError(t, os.WriteFile(rejectedLedgerPath(home), []byte("{not json"), 0o600))

		_, _, err := CandidateRejected(home, []byte("anything"))
		require.Error(t, err, "a corrupt ledger must not read as an empty one")
	})

	t.Run("newer schema", func(t *testing.T) {
		home := t.TempDir()
		require.NoError(t, RecordRejectedCandidate(home, digest([]byte("bad")), "1.0.207", "rolled back"))
		require.NoError(t, os.WriteFile(rejectedLedgerPath(home),
			[]byte(`{"schema_version":9999,"candidates":[]}`), 0o600))

		_, _, err := CandidateRejected(home, []byte("anything"))
		require.Error(t, err,
			"an older binary must not decode a newer ledger on a guess and activate what a newer one disqualified")
	})
}

// TestRecordRejectedCandidate_IsIdempotentAndBounded covers the two ways the ledger
// could rot: the supervisor re-enters its phases after an actor crash, so the same
// candidate is recorded repeatedly; and a long-lived box must not grow the file
// without limit.
func TestRecordRejectedCandidate_IsIdempotentAndBounded(t *testing.T) {
	home := t.TempDir()
	candidate := []byte("rolled back twice")

	require.NoError(t, RecordRejectedCandidate(home, digest(candidate), "1.0.207", "rolled back"))
	require.NoError(t, RecordRejectedCandidate(home, digest(candidate), "1.0.207", "rolled back again"))

	ledger, err := readRejectedLedger(home)
	require.NoError(t, err)
	require.Len(t, ledger.Candidates, 1, "re-entry must refresh the entry, not duplicate it")
	require.Equal(t, "rolled back again", ledger.Candidates[0].Reason)

	for i := 0; i < maxRejectedCandidates+5; i++ {
		require.NoError(t, RecordRejectedCandidate(
			home, digest([]byte{byte(i), 'x'}), "1.0.2", "rolled back"))
	}
	ledger, err = readRejectedLedger(home)
	require.NoError(t, err)
	require.Len(t, ledger.Candidates, maxRejectedCandidates, "the ledger must stay bounded")

	// The most recent rejection is the one that matters most, so it must be the last
	// thing evicted — dropping newest-first would forget the release breaking you now.
	newest := []byte{byte(maxRejectedCandidates + 4), 'x'}
	rejected, _, err := CandidateRejected(home, newest)
	require.NoError(t, err)
	require.True(t, rejected, "the newest rejection must survive the cap")
}

// TestRejectedLedger_IsOwnerOnly — the ledger decides whether a binary may be
// activated, so a user who can write it can re-enable a release this box refused.
func TestRejectedLedger_IsOwnerOnly(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, RecordRejectedCandidate(home, digest([]byte("bad")), "1.0.207", "rolled back"))

	info, err := os.Stat(filepath.Join(upgradeRoot(home), rejectedLedgerName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
