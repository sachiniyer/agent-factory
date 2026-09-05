package upgradetxn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// executableIn writes a plausible af binary and returns its path.
func executableIn(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(path, []byte("af binary"), 0o755))
	return path
}

// stageArtifactFor writes the preserved/candidate pair one transaction stages
// beside an executable, aged by the given duration. A zero age means "just now".
func stageArtifactFor(t *testing.T, executable, id string, age time.Duration) (string, string) {
	t.Helper()
	previous, candidate := binaryArtifactPaths(executable, id)
	require.NoError(t, os.WriteFile(previous, []byte("preserved previous binary"), 0o755))
	require.NoError(t, os.WriteFile(candidate, []byte("staged candidate binary"), 0o755))
	if age > 0 {
		stamp := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(previous, stamp, stamp))
		require.NoError(t, os.Chtimes(candidate, stamp, stamp))
	}
	return previous, candidate
}

// writeOwnerRecordFor writes the record Prepare leaves beside the artifacts.
func writeOwnerRecordFor(t *testing.T, executable, id, home string, age time.Duration) string {
	t.Helper()
	path := artifactOwnerPath(executable, id)
	require.NoError(t, writeArtifactOwner(path, ArtifactOwner{
		SchemaVersion: artifactOwnerSchemaVersion,
		TransactionID: id,
		HomeDir:       home,
		StagedAt:      time.Now().UTC().Add(-age),
	}))
	if age > 0 {
		stamp := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(path, stamp, stamp))
	}
	return path
}

// homeWithActiveTransaction builds an AF home whose active journal NAMES the
// given transaction. A journal of "{}" would satisfy a presence check while
// proving nothing, which is the weaker test this deliberately is not.
func homeWithActiveTransaction(t *testing.T, id string) string {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(upgradeRoot(home), 0o755))
	data, err := json.Marshal(Journal{ID: id, Phase: PhasePrepared})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(activeJournalPath(home), data, 0o600))
	return home
}

func clearingScan(t *testing.T, executable, selfID string) *StagedArtifact {
	t.Helper()
	blocking, err := BlockingStagedArtifact(executable, selfID, ArtifactScanOptions{Clear: true})
	require.NoError(t, err)
	return blocking
}

// The headline case: a cleanup that died after removing active.json and before
// unlinking the preserved binary. Nothing owns it, nothing will ever come back
// for it, and it blocked every future upgrade of this binary forever.
func TestStagedArtifact_InertLeftoverDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, candidate := stageArtifactFor(t, executable, "upgrade-leftover", 48*time.Hour)

	require.Nil(t, clearingScan(t, executable, "upgrade-mine"),
		"an artifact with no owner, no journal and no recent activity is debris, not a live transaction")

	require.NoFileExists(t, previous, "the debris must be out of the scanned name space")
	require.NoFileExists(t, candidate)

	// Set aside, never deleted: the bytes are still recoverable at a named path.
	matches, err := filepath.Glob(previous + ".debris-*")
	require.NoError(t, err)
	require.Len(t, matches, 1, "the preserved binary must be kept, under a name the scan ignores")
	kept, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.Equal(t, "preserved previous binary", string(kept))
}

// The interlock's whole purpose, which must survive the change: a transaction
// whose home still names it in an active journal keeps its claim.
func TestStagedArtifact_AJournalledTransactionStillBlocks(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-live", 48*time.Hour)
	home := homeWithActiveTransaction(t, "upgrade-live")
	writeOwnerRecordFor(t, executable, "upgrade-live", home, 48*time.Hour)

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "a transaction its home still names must block a second one")
	require.Equal(t, "upgrade-live", blocking.ID)
	require.Contains(t, blocking.Reason, home, "the refusal must say which home is still holding it")
	require.FileExists(t, previous, "a live transaction's rollback source must not be touched")
}

// Absence cannot mean "finished" during the two windows where a live transaction
// legitimately has no journal: Prepare before it publishes active.json, and
// cleanup after it removes it. Youth is what covers both.
func TestStagedArtifact_AYoungArtifactStillBlocks(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-inflight", 0)

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "an artifact staged moments ago may belong to a Prepare still running")
	require.FileExists(t, previous)
}

// A home that finished one transaction and started another over a DIFFERENT
// executable presented the new journal as proof the old artifact was live.
func TestStagedArtifact_ADifferentActiveTransactionDoesNotRevive(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-old", 48*time.Hour)
	home := homeWithActiveTransaction(t, "upgrade-new")
	writeOwnerRecordFor(t, executable, "upgrade-old", home, 48*time.Hour)

	require.Nil(t, clearingScan(t, executable, "upgrade-mine"),
		"another transaction being active says nothing about this one")
	require.NoFileExists(t, previous)
}

// An unreadable claim is still a claim. This is the case an earlier attempt got
// wrong in the catastrophic direction, so the default stays fail-closed — and
// overridable, because a refusal an unattended daemon cannot get past is the
// defect under repair.
func TestStagedArtifact_UnreadableOwnerHomeKeepsBlockingUntilTheOperatorSaysOtherwise(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-unreadable", 48*time.Hour)
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(upgradeRoot(home), 0o755))
	require.NoError(t, os.WriteFile(activeJournalPath(home), []byte("{not json"), 0o600))
	writeOwnerRecordFor(t, executable, "upgrade-unreadable", home, 48*time.Hour)

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "evidence that cannot be read is not evidence of death")
	require.Contains(t, blocking.Reason, home)
	require.FileExists(t, previous)

	cleared, err := BlockingStagedArtifact(executable, "upgrade-mine", ArtifactScanOptions{Clear: true, ClearUnverifiable: true})
	require.NoError(t, err)
	require.Nil(t, cleared, "the operator opt-in is the way past an unreadable claim")
	require.NoFileExists(t, previous)
}

// A journal that decodes but names nothing is not the home saying it moved on.
// "A different transaction is active there" is only a statement when there IS a
// different transaction; an empty id is a journal af cannot interpret, and
// clearing another home's rollback source on that reading would be acting on
// corruption.
func TestStagedArtifact_AJournalNamingNoTransactionIsNotAFinishedOne(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-blankjournal", 48*time.Hour)
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(upgradeRoot(home), 0o755))
	require.NoError(t, os.WriteFile(activeJournalPath(home), []byte("{}"), 0o600))
	writeOwnerRecordFor(t, executable, "upgrade-blankjournal", home, 48*time.Hour)

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "an uninterpretable journal is not evidence this transaction ended")
	require.FileExists(t, previous)
}

// An absent home reads exactly like a finished transaction to a bare journal
// stat, and an unmounted filesystem is the realistic way that happens (#2984).
func TestStagedArtifact_MissingOwnerHomeIsNotAFinishedTransaction(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-gonehome", 48*time.Hour)
	writeOwnerRecordFor(t, executable, "upgrade-gonehome", filepath.Join(t.TempDir(), "not-there"), 48*time.Hour)

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "a home af cannot reach has not been shown to be finished")
	require.FileExists(t, previous)
}

// The record has to describe THIS artifact. One naming another transaction is
// refused rather than trusted, in either direction.
func TestStagedArtifact_OwnerRecordNamingAnotherTransactionIsNotTrusted(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-mismatch", 48*time.Hour)
	path := artifactOwnerPath(executable, "upgrade-mismatch")
	require.NoError(t, writeArtifactOwner(path, ArtifactOwner{
		SchemaVersion: artifactOwnerSchemaVersion,
		TransactionID: "upgrade-somebodyelse",
		HomeDir:       homeWithActiveTransaction(t, "upgrade-somebodyelse"),
	}))
	stamp := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, stamp, stamp))

	blocking := clearingScan(t, executable, "upgrade-mine")
	require.NotNil(t, blocking, "a record that does not describe this artifact leaves it unattributed, not clear")
	require.FileExists(t, previous)
}

// The unlocked launch probe reads and never writes: clearing is a mutation of a
// directory it does not hold the lock for.
func TestStagedArtifact_AReadOnlyScanClearsNothing(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-leftover", 48*time.Hour)

	blocking, err := BlockingStagedArtifact(executable, "upgrade-mine", ArtifactScanOptions{})
	require.NoError(t, err)
	require.Nil(t, blocking, "debris must not suppress an update just because this caller cannot clear it")
	require.FileExists(t, previous, "a scan without the lock must not touch the directory")
}

// Only artifacts belonging to THIS executable count.
func TestStagedArtifact_OnlyMatchesThisExecutable(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	sibling := filepath.Join(dir, "other")
	require.NoError(t, os.WriteFile(sibling, []byte("other binary"), 0o755))
	stageArtifactFor(t, sibling, "upgrade-sibling", 0)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".af.af-upgrade-nope.txt"), []byte("x"), 0o600))

	require.Nil(t, clearingScan(t, executable, "upgrade-mine"),
		"another binary's upgrade says nothing about this one")
}

// A transaction never blocks itself: Prepare passes its own id, and a committed
// transaction whose cleanup has not finished must not refuse its own successor.
func TestStagedArtifact_SkipsOurOwnTransaction(t *testing.T) {
	dir := t.TempDir()
	executable := executableIn(t, dir)
	previous, _ := stageArtifactFor(t, executable, "upgrade-mine", 0)

	require.Nil(t, clearingScan(t, executable, "upgrade-mine"))
	require.FileExists(t, previous, "our own artifacts are not debris to be swept")
}
