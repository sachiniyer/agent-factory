package upgradetxn

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Refreshing the metadata snapshot (#2212 gate 2).
//
// Prepare snapshots instances.json and tasks.json before the recovery actor is
// installed, and the daemon does not stop admitting mutations until it has
// authorized activation. Between those two points sits InstallAndStart plus
// AwaitSupervisorReady, whose grace is a full minute, and the daemon is entirely
// live for all of it — control RPCs admitted, scheduler and poll loops running.
// A rollback restores the older snapshot, so every write from that window is
// discarded: the data-loss shape #2572's manifest exists to prevent, reintroduced
// by ordering rather than by a missing guarantee.
//
// The fix is to re-capture once the daemon has stopped admitting, so the bytes the
// actor would restore are the bytes that were actually serving.

// metadataDirName is the generation-0 directory Prepare writes.
const metadataDirName = "metadata"

// RefreshMetadataSnapshot re-captures the transaction's metadata files and swaps
// the journal onto the new set.
//
// Callable only at PhaseSupervisorReady, and that is the whole safety argument
// rather than a formality. The actor has proven it holds the lease and is ready,
// so it exists and can be relied on — but it has NOT been authorized, and
// authorization is what licenses it to commit or roll back. So this runs in the one
// window where the snapshot can be rewritten with no possibility of a restore
// reading it concurrently. Refreshing after AuthorizeActivation would race the
// actor's own rollback against a half-written snapshot directory.
//
// The new generation is written to its own directory and the journal is swapped
// afterwards, never rewritten in place. A crash at any instant therefore leaves the
// journal naming exactly one complete set whose digests match its bytes: either the
// old generation or the new one. Rewriting the existing files would leave a window
// where the recorded SHA256s belong to bytes that are no longer there, and the
// integrity check on the rollback path would refuse to restore — turning a
// crash-during-upgrade into an unrecoverable home, which is strictly worse than the
// stale snapshot this is fixing.
//
// The superseded generation is deliberately left on disk. It costs a copy of two
// small JSON files inside a directory that Cleanup removes wholesale, and keeping it
// means a journal that somehow still names the old set can still be honoured.
func (t *Transaction) RefreshMetadataSnapshot() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	home := t.journal.HomeDir
	current, err := readJournal(activeJournalPath(home))
	if err != nil {
		return fmt.Errorf("read the active upgrade journal: %w", err)
	}
	if err := validateJournal(home, current); err != nil {
		return fmt.Errorf("validate the active upgrade journal: %w", err)
	}
	// Identity before phase: a journal for a DIFFERENT transaction that happens to
	// be at supervisor_ready would otherwise pass the phase check and have this
	// transaction's snapshot written over it.
	if current.ID != t.journal.ID {
		return fmt.Errorf(
			"active upgrade transaction is %q, not %q; refusing to refresh another transaction's metadata snapshot",
			current.ID, t.journal.ID,
		)
	}
	if current.Phase != PhaseSupervisorReady {
		return fmt.Errorf(
			"upgrade metadata can only be re-snapshotted while the actor is ready and not yet authorized (phase %s, need %s)",
			current.Phase, PhaseSupervisorReady,
		)
	}
	if len(current.Metadata) == 0 {
		return nil // nothing was snapshotted, so there is nothing to bring forward
	}

	// Rebuild the path list from the journal, in the recorded ORDER: the snapshot
	// filenames are index-derived, so a reordered list would still produce a
	// valid-looking set with each file's bytes filed under another file's name.
	// Journal paths are already home-relative, which is the form MetadataPaths takes.
	paths := make([]string, 0, len(current.Metadata))
	for _, entry := range current.Metadata {
		paths = append(paths, entry.Path)
	}

	txnDir := transactionDir(home, current.ID)
	generation := nextMetadataGeneration(current.Metadata)
	directory := filepath.Join(txnDir, metadataGenerationDir(generation))
	if err := createDurableDirectory(txnDir, directory, transactionDirMode); err != nil {
		return fmt.Errorf("create refreshed metadata snapshot directory: %w", err)
	}
	snapshots, err := snapshotMetadataInto(home, directory, paths)
	if err != nil {
		return fmt.Errorf("re-snapshot upgrade metadata: %w", err)
	}

	next := current
	next.Metadata = snapshots
	next.UpdatedAt = time.Now().UTC()
	if err := validateJournal(home, next); err != nil {
		return fmt.Errorf("validate the refreshed upgrade journal: %w", err)
	}
	// The swap. persistJournal writes and fsyncs atomically, so the journal names the
	// old generation until this returns and the new one afterwards, never a mix.
	if err := persistJournal(activeJournalPath(home), next); err != nil {
		return fmt.Errorf("publish the refreshed upgrade journal: %w", err)
	}
	t.journal = next
	return nil
}

// metadataGenerationDir names the directory holding one generation of snapshots.
// Generation 0 keeps Prepare's original "metadata" name so a journal written by an
// older binary still resolves.
func metadataGenerationDir(generation int) string {
	if generation == 0 {
		return metadataDirName
	}
	return metadataDirName + "-" + strconv.Itoa(generation)
}

// nextMetadataGeneration reads the generation in use from the snapshot paths the
// journal already names, rather than from a counter held in memory. A Transaction
// can be reconstructed from disk, so an in-memory counter would restart at zero and
// hand back a directory that is already populated.
func nextMetadataGeneration(snapshots []MetadataSnapshot) int {
	highest := 0
	for _, entry := range snapshots {
		if entry.SnapshotPath == "" {
			continue // the file did not exist when it was captured; no directory to read
		}
		base := filepath.Base(filepath.Dir(entry.SnapshotPath))
		if base == metadataDirName {
			continue // generation 0
		}
		suffix, found := strings.CutPrefix(base, metadataDirName+"-")
		if !found {
			continue
		}
		if value, err := strconv.Atoi(suffix); err == nil && value > highest {
			highest = value
		}
	}
	return highest + 1
}

// validMetadataSnapshotPath confines a journal's SnapshotPath to a location this
// transaction could itself have written.
//
// It replaces an equality check against one derived path, and the strictness is the
// point rather than the shape: the rollback path READS these files back over the
// live home, so a journal free to name any path would be a write-anywhere primitive
// on recovery. Widening it to accept refreshed generations must not widen it to
// accept anything else, so every component is still pinned — the parent must be this
// transaction's own directory, the filename must be the index-derived one, and the
// only freedom is which generation directory holds it.
func validMetadataSnapshotPath(txnDir string, index int, path string) bool {
	if path == "" {
		return false
	}
	if filepath.Base(path) != fmt.Sprintf("%04d.snapshot", index) {
		return false
	}
	directory := filepath.Dir(path)
	if filepath.Dir(directory) != txnDir {
		return false
	}
	base := filepath.Base(directory)
	if base == metadataDirName {
		return true // generation 0, written by Prepare
	}
	suffix, found := strings.CutPrefix(base, metadataDirName+"-")
	if !found {
		return false
	}
	generation, err := strconv.Atoi(suffix)
	// Atoi accepts "+1", "-1" and leading zeros, all of which would name a directory
	// other than the one metadataGenerationDir produces. Round-tripping rejects them.
	return err == nil && generation >= 1 && metadataGenerationDir(generation) == base
}
