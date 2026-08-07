package upgradetxn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCandidateRejected_SurvivesARestart is the whole point of the ledger. #2908's
// quarantine is a map on the update driver, so a restart clears it and the six-hour
// check offers the same broken release again — an unattended box re-breaking itself
// on a loop. The read here goes to disk with no warm state in between, which is the
// only way to tell a durable record from a remembered one.
func TestCandidateRejected_SurvivesARestart(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "af")
	candidate := []byte("the binary that failed validation")

	rejected, _, err := CandidateRejected(executable, candidate)
	require.NoError(t, err, "a home that has never rolled anything back has no ledger, which is not an error")
	require.False(t, rejected, "premise: nothing is rejected yet")

	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "1.0.207", "rolled back"))

	// No shared handle, no cache: this is what a later boot sees.
	rejected, entry, err := CandidateRejected(executable, candidate)
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
	executable := filepath.Join(t.TempDir(), "af")
	broken := []byte("1.0.207 as first published — broken")
	fixed := []byte("1.0.207 re-cut with the fix")

	require.NoError(t, RecordRejectedCandidate(executable, digest(broken), "1.0.207", "rolled back"))

	rejected, _, err := CandidateRejected(executable, broken)
	require.NoError(t, err)
	require.True(t, rejected, "the exact bytes that failed stay refused")

	rejected, _, err = CandidateRejected(executable, fixed)
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
		executable := filepath.Join(t.TempDir(), "af")
		require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))
		require.NoError(t, os.WriteFile(rejectedLedgerPath(executable), []byte("{not json"), 0o600))

		_, _, err := CandidateRejected(executable, []byte("anything"))
		require.Error(t, err, "a corrupt ledger must not read as an empty one")
	})

	t.Run("newer schema", func(t *testing.T) {
		executable := filepath.Join(t.TempDir(), "af")
		require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))
		require.NoError(t, os.WriteFile(rejectedLedgerPath(executable),
			[]byte(`{"schema_version":9999,"candidates":[]}`), 0o600))

		_, _, err := CandidateRejected(executable, []byte("anything"))
		require.Error(t, err,
			"an older binary must not decode a newer ledger on a guess and activate what a newer one disqualified")
	})
}

// TestRecordRejectedCandidate_IsIdempotentAndBounded covers the two ways the ledger
// could rot: the supervisor re-enters its phases after an actor crash, so the same
// candidate is recorded repeatedly; and a long-lived box must not grow the file
// without limit.
func TestRecordRejectedCandidate_IsIdempotentAndBounded(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "af")
	candidate := []byte("rolled back twice")

	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "1.0.207", "rolled back"))
	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "1.0.207", "rolled back again"))

	ledger, err := readRejectedLedger(executable)
	require.NoError(t, err)
	require.Len(t, ledger.Candidates, 1, "re-entry must refresh the entry, not duplicate it")
	require.Equal(t, "rolled back again", ledger.Candidates[0].Reason)

	for i := 0; i < maxRejectedCandidates+5; i++ {
		require.NoError(t, RecordRejectedCandidate(
			executable, digest([]byte{byte(i), 'x'}), "1.0.2", "rolled back"))
	}
	ledger, err = readRejectedLedger(executable)
	require.NoError(t, err)
	require.Len(t, ledger.Candidates, maxRejectedCandidates, "the ledger must stay bounded")

	// The most recent rejection is the one that matters most, so it must be the last
	// thing evicted — dropping newest-first would forget the release breaking you now.
	newest := []byte{byte(maxRejectedCandidates + 4), 'x'}
	rejected, _, err := CandidateRejected(executable, newest)
	require.NoError(t, err)
	require.True(t, rejected, "the newest rejection must survive the cap")
}

// TestRejectedLedger_IsOwnerOnly — the ledger decides whether a binary may be
// activated, so a user who can write it can re-enable a release this box refused.
func TestRejectedLedger_IsOwnerOnly(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))

	info, err := os.Stat(rejectedLedgerPath(executable))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestCandidateRejected_StructurallyInvalidLedgerErrors — decoding successfully is
// not the same as being a ledger. Each of these is valid JSON that unmarshals
// without error into a zero value or a digest-less entry, and each would otherwise
// read as "this box has rejected nothing", silently re-arming every release it
// rolled back. Same outcome as a corrupt file, so it must get the same fail-closed
// answer rather than a different one that happens to parse.
func TestCandidateRejected_StructurallyInvalidLedgerErrors(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"null", `null`},
		{"empty object", `{}`},
		{"no schema version", `{"candidates":[]}`},
		{"entry with no digest", `{"schema_version":1,"candidates":[{}]}`},
		{"entry with a junk digest", `{"schema_version":1,"candidates":[{"sha256":"not-a-digest"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "af")
			require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))
			require.NoError(t, os.WriteFile(rejectedLedgerPath(executable), []byte(tc.body), 0o600))

			_, _, err := CandidateRejected(executable, []byte("anything"))
			require.Errorf(t, err, "%s must not read as an empty ledger", tc.name)
		})
	}
}

// TestCandidateRejected_IsSharedAcrossHomesOnOneExecutable is why the ledger is keyed
// by executable rather than by AGENT_FACTORY_HOME. One installation can serve several
// homes — commands/upgrade_interlock.go takes an executable-scoped lock for exactly
// that reason — and the thing a bad candidate breaks is the shared binary. A per-home
// ledger would let home B reinstall the bytes home A had just rolled back, over the
// executable they share.
func TestCandidateRejected_IsSharedAcrossHomesOnOneExecutable(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "af")
	candidate := []byte("the build that failed validation")

	// Home A rolls it back. Homes are not even named here — that is the point.
	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "1.0.207", "rolled back"))

	// Home B, a different AGENT_FACTORY_HOME, asks about the same installation.
	rejected, entry, err := CandidateRejected(executable, candidate)
	require.NoError(t, err)
	require.True(t, rejected,
		"a second home sharing this executable must see the rejection; otherwise it reinstalls the broken bytes over the binary the first home just recovered")
	require.Equal(t, "1.0.207", entry.Version)

	// A DIFFERENT installation is genuinely unaffected — the scope is the binary,
	// not the machine.
	other := filepath.Join(t.TempDir(), "af")
	rejected, _, err = CandidateRejected(other, candidate)
	require.NoError(t, err)
	require.False(t, rejected, "an unrelated installation must not inherit another's quarantine")
}

// TestRecordRejectedCandidate_KeepsInsertionOrderUnderABackwardClock pins the cap
// against a clock that steps backwards — NTP correcting a drifted box, which is
// exactly the sort of box that has been rebooting into a bad release. Sorting by
// RejectedAt would file the newest entry first and the cap would then discard the
// very rejection just recorded, while the call still returned success and the
// supervisor went on to disable recovery.
func TestRecordRejectedCandidate_KeepsInsertionOrderUnderABackwardClock(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "af")
	for i := 0; i < maxRejectedCandidates; i++ {
		require.NoError(t, RecordRejectedCandidate(executable, digest([]byte{byte(i), 'a'}), "1.0.1", "rolled back"))
	}
	// Backdate every stored entry so the next append is the OLDEST by wall clock.
	ledger, err := readRejectedLedger(executable)
	require.NoError(t, err)
	for i := range ledger.Candidates {
		ledger.Candidates[i].RejectedAt = time.Now().UTC().Add(24 * time.Hour)
	}
	encoded, err := json.MarshalIndent(ledger, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rejectedLedgerPath(executable), encoded, 0o600))

	latest := []byte("the one that just failed")
	require.NoError(t, RecordRejectedCandidate(executable, digest(latest), "1.0.207", "rolled back"))

	rejected, _, err := CandidateRejected(executable, latest)
	require.NoError(t, err)
	require.True(t, rejected,
		"the rejection just recorded must survive the cap even when every other entry is stamped later than it")
}

// TestRejectedLedgerNarrowsWhenTheDirectoryStopsBeingShared is #3011 review: the
// 0660 widening is justified only while the install directory is group-writable,
// because the audience is exactly the set that can already replace the binary. Once
// the directory is tightened, former group writers keep SEARCH permission and can
// still rewrite the ledger — publishing a valid empty one makes the owner's
// unattended updater reinstall the bytes it had disqualified.
func TestRejectedLedgerNarrowsWhenTheDirectoryStopsBeingShared(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	path := rejectedLedgerPath(executable)
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1}`), rejectedLedgerSharedMode))

	// The directory is NOT group-writable, so the widened ledger no longer has a
	// justification: reading it must re-narrow it.
	require.NoError(t, os.Chmod(dir, 0o750))
	_, err := readRejectedLedger(executable)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, rejectedLedgerMode, info.Mode().Perm(),
		"a ledger left group-writable after its directory narrowed can be rewritten by users who can no longer replace the binary")
}

// The mirror case: while the directory IS shared the widened mode must survive, or
// the widening would undo itself on the first read and every other authorized
// writer's updater would fail closed again.
func TestRejectedLedgerKeepsItsSharedModeWhileTheDirectoryIsShared(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	path := rejectedLedgerPath(executable)
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":1}`), rejectedLedgerSharedMode))
	require.NoError(t, os.Chmod(dir, 0o770))

	if _, shared := directoryWriterGroup(dir); !shared {
		t.Skip("this filesystem/uid does not present the directory as group-writable")
	}
	_, err := readRejectedLedger(executable)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, rejectedLedgerSharedMode, info.Mode().Perm(),
		"narrowing a ledger whose directory is still shared would lock out the other authorized writers")
}
