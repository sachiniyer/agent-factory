package upgradetxn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"syscall"
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
	// State the directory posture rather than inheriting it (#3465). The ledger's
	// mode is DERIVED from the directory's, so leaving the fixture at whatever
	// umask produced it tests the developer's environment, not the contract: under
	// 002 this asserted 0600 against the shared branch's 0660.
	executable := sharedInstallDir(t, 0o700)
	_, shared := directoryWriterGroup(filepath.Dir(executable))
	require.False(t, shared, "precondition: a privately-owned install directory")

	require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))

	info, err := os.Stat(rejectedLedgerPath(executable))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// The other branch, pinned rather than merely tolerated. The owner-only mode
// exists so a user who can write the ledger cannot re-enable a release this box
// refused — but on a directory the group can already install into, that group can
// replace the binary outright, so an owner-only ledger withholds nothing from
// them and instead blocks the authorized writers the widening exists to admit
// (install_lock.go:245-262).
//
// Scoped to the MODE. The group the ledger carries is a separate claim, and a
// single-user fixture cannot test it — the temp dir's group already equals the
// creator's primary group, so "file gid == dir gid" passes against unfixed code.
// TestExecutableLock_TakesTheDirectorysGroup owns that one, and retargets the
// directory to a SECONDARY group to give the assertion teeth.
//
// The sibling ledger tests cover the READ path (realign on
// readRejectedLedger); this is the initial WRITE, which goes through
// durableAtomicWriteFileInGroup instead.
func TestRejectedLedger_WidensOnAGroupInstallableDirectory(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)
	if _, shared := directoryWriterGroup(filepath.Dir(executable)); !shared {
		// Not a failure. Where ACL state cannot be read — macOS, and any filesystem
		// that presents one — the classifier declines and the ledger stays private,
		// which is the correct behaviour. Asserting the widening there would make a
		// red job out of a right answer, and teach the next reader to ignore it.
		t.Skip("this filesystem/uid does not present the directory as group-installable")
	}

	require.NoError(t, RecordRejectedCandidate(executable, digest([]byte("bad")), "1.0.207", "rolled back"))

	info, err := os.Stat(rejectedLedgerPath(executable))
	require.NoError(t, err)
	require.Equal(t, rejectedLedgerSharedMode, info.Mode().Perm(),
		"a ledger the authorized group cannot read leaves them unable to see what this box refused")
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
	// chmod, not the WriteFile mode: os.WriteFile applies the process UMASK, so on a
	// 022 runner the file lands 0640 and the fixture is not the widened ledger this
	// test is about.
	require.NoError(t, os.Chmod(path, rejectedLedgerSharedMode))

	// The directory is NOT group-writable, so the widened ledger no longer has a
	// justification.
	require.NoError(t, os.Chmod(dir, 0o750))
	_, err := readRejectedLedger(executable)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	if !directorySharednessIsKnowable(dir) {
		// macOS and anything else without readable ACL state. "Not shared" there is
		// "cannot tell", and narrowing on a guess would revoke a genuinely shared
		// ledger — so the contract is to leave it ALONE, and that is what this
		// asserts rather than skipping and testing nothing (#3011 review).
		require.Equal(t, rejectedLedgerSharedMode, info.Mode().Perm(),
			"sharedness is unknowable here, so the mode must be left untouched in both directions")
		return
	}
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
	require.NoError(t, os.Chmod(path, rejectedLedgerSharedMode)) // umask-proof; see above
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

// TestPrepareRefusesACandidateRejectedAfterTheCallersCheck is the #3043 sibling in
// the daemon's path: another home sharing this executable can be rolling back the
// same candidate, record the rejection, and CLEAN UP before this transaction reaches
// Prepare. Its artifacts are gone by then, so the foreign-transaction check cannot
// see it — and the executable fingerprint cannot either, because a rollback restores
// the same baseline bytes it started from. Only the ledger remembers, so only a read
// under the preparation lock is serialised against the write that put it there.
//
// Written in order rather than raced, so it is a regression test and not a timing
// experiment: the caller's check passes, the rejection lands, Prepare runs.
func TestPrepareRefusesACandidateRejectedAfterTheCallersCheck(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("running binary"), 0o755))
	candidate := []byte("candidate binary")

	plan := Plan{
		ID:             "upgrade-3043",
		HomeDir:        home,
		ExecutablePath: executable,
		FromVersion:    "v1.0.0",
		ToVersion:      "v9.9.9",
		Candidate:      candidate,
		RecoveryJob:    RecoveryJob{Kind: RecoveryJobDetached},
	}

	// The caller's own check, before the lock: clean.
	rejected, _, err := CandidateRejected(executable, candidate)
	require.NoError(t, err)
	require.False(t, rejected, "precondition: nothing is disqualified yet")

	// The window: a peer transaction disqualifies exactly these bytes and cleans up.
	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "v9.9.9",
		"the candidate failed validation and was rolled back"))

	txn, err := Prepare(plan)
	if txn != nil {
		t.Cleanup(func() { _ = txn.abort() })
	}
	require.Error(t, err, "Prepare must refuse a candidate the ledger disqualified after the caller looked")
	require.Contains(t, err.Error(), "rolled back")
	require.Nil(t, txn, "no transaction may be published for disqualified bytes")
}

// #3011 review: the CandidateInstalled field is omitempty and the schema version did
// not change, so a transaction left in flight by the PREVIOUS release decodes with
// it false. Resumed by this binary it would roll back without disqualifying a
// candidate whose bytes really did run — the safety mechanism silently skipped for
// exactly one upgrade, which is the boundary nobody tests by hand.
func TestLegacyJournalInAnInstalledPhaseCountsAsInstalled(t *testing.T) {
	for _, phase := range []Phase{PhaseCandidateInstalled, PhaseCandidateStarting, PhaseCandidateValidating} {
		require.True(t, phaseImpliesCandidateInstalled(phase),
			"%s is reached only after the candidate's bytes are on disk", phase)
	}
	// The phases that do NOT imply it must stay false, or a candidate that never
	// installed would be permanently blocked — the defect this whole gate prevents.
	for _, phase := range []Phase{PhaseDaemonStopping, PhaseRolledBack, PhaseRollbackFailed} {
		require.False(t, phaseImpliesCandidateInstalled(phase),
			"%s can be reached with nothing installed", phase)
	}
}

// #3011 review: the ledger read must not follow a symlink. This one fails OPEN if
// it is fooled — a link to a structurally VALID empty ledger parses fine, the list
// is empty, and every disqualified release becomes installable again. The
// corrupt-bytes guard cannot catch it, because nothing about it is corrupt.
func TestRejectedLedgerRefusesToFollowASymlink(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	candidate := []byte("bad candidate")
	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "v9.9.9", "rolled back"))

	rejected, _, err := CandidateRejected(executable, candidate)
	require.NoError(t, err)
	require.True(t, rejected, "precondition: the ledger disqualifies these bytes")

	// An attacker-controlled, structurally valid EMPTY ledger, reached by a link.
	path := rejectedLedgerPath(executable)
	decoy := filepath.Join(t.TempDir(), "decoy.json")
	require.NoError(t, os.WriteFile(decoy, []byte(`{"schema_version":1}`), 0o600))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(decoy, path))

	_, _, err = CandidateRejected(executable, candidate)
	require.Error(t, err, "following the link would re-arm every disqualified release, silently")
}

// #3011 review: a FIFO at the ledger path would make a blocking open wait forever
// for a writer — hanging launch-time updates after the download, and hanging Prepare
// WHILE IT HOLDS THE EXECUTABLE LOCK. A wrong answer would be bad; a hang holding a
// lock is worse. The read must refuse promptly instead.
func TestRejectedLedgerRefusesAFifoRatherThanBlocking(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	path := rejectedLedgerPath(executable)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := CandidateRejected(executable, []byte("any candidate"))
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "a FIFO is not a ledger; it must fail closed, not be read")
		require.Contains(t, err.Error(), "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("the read blocked on a FIFO — this is the hang that would freeze Prepare while it holds the executable lock")
	}
}

// #3011 review: a hard link passes Lstat AND IsRegular — it IS a regular file, just
// a second name for an inode somebody else still controls. One planted while the
// directory was group-writable outlives the tightening, because narrowing the
// directory does not touch the other name.
func TestRejectedLedgerRefusesAHardLinkedInode(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	candidate := []byte("bad candidate")
	require.NoError(t, RecordRejectedCandidate(executable, digest(candidate), "v9.9.9", "rolled back"))

	path := rejectedLedgerPath(executable)
	require.NoError(t, os.Link(path, filepath.Join(dir, "attacker-handle.json")),
		"a second name for the same inode")

	_, _, err := CandidateRejected(executable, candidate)
	require.Error(t, err, "an inode another path still names is not trustworthy evidence")
	require.Contains(t, err.Error(), "hard link")
}

// #3011 review: the realign must not chmod through a hard link. If the ledger path
// is a second name for the EXECUTABLE's inode, a path-based chmod turns the binary
// 0755 -> 0600 and breaks the installation this code exists to protect.
func TestRejectedLedgerRealignNeverRestylesALinkedExecutable(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("bin"), 0o755))
	path := rejectedLedgerPath(executable)
	require.NoError(t, os.Link(executable, path), "the ledger path is a second name for the executable")
	require.NoError(t, os.Chmod(dir, 0o750))

	alignRejectedLedgerWithDirectoryWriters(path)

	info, err := os.Stat(executable)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
		"the executable must still be executable; a chmod through the link would have bricked it")
}
