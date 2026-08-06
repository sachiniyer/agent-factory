package upgradetxn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The durable rejected-candidate ledger (#2212).
//
// #2908 quarantines a release whose hand-off failed, but only in a map on the update
// driver, so a restart clears it. The dangerous case is not a failed hand-off though
// — it is a candidate that was installed, failed validation, and had to be ROLLED
// BACK. Without a durable record the six-hour check finds the same release again on
// the next boot and re-breaks the box, unattended, forever. That is the "every daemon
// in the world breaks itself with no human present" hazard this epic exists to avoid,
// arriving on a loop.
//
// The ledger lives beside the rollback path that writes it rather than in the driver
// that reads it: the driver is one caller, and a rollback is authoritative about its
// own candidate whether or not any driver is running.

// rejectedLedgerName is the ledger file inside the upgrade root.
const rejectedLedgerName = "rejected-candidates.json"

// maxRejectedCandidates bounds the ledger. It is a disqualification list for
// binaries this box actually installed and rolled back, so it grows only when an
// upgrade genuinely fails; a box that hits 32 distinct bad releases has a problem no
// ledger will fix. Oldest entries are dropped first, which is the right direction:
// the release that broke you yesterday matters more than one from a year ago that
// nothing will offer again.
const maxRejectedCandidates = 32

// rejectedLedgerMode is 0600. The ledger decides whether a binary may be activated,
// so a user who can write it can re-enable a release this box rejected.
const rejectedLedgerMode os.FileMode = 0o600

// RejectedCandidate is one disqualified upgrade candidate.
//
// Keyed on SHA256 rather than on the version string, and that is a deliberate
// departure from the gate's "rejected tag" wording. A tag can be re-cut with
// corrected bytes, and a tag-keyed ledger would refuse the FIX for that release for
// the life of the box — a safety mechanism turned into a permanent block, which is
// the unoverridable shape #2859 was bitten by. The digest disqualifies exactly the
// bytes that failed and admits a genuine re-release under the same tag.
//
// Version and Reason are carried for the operator, not for the decision: a log line
// saying "1.0.207 was rolled back here on the 3rd" is what makes an unattended box
// diagnosable.
type RejectedCandidate struct {
	SHA256     string    `json:"sha256"`
	Version    string    `json:"version,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	RejectedAt time.Time `json:"rejected_at"`
}

type rejectedLedger struct {
	SchemaVersion int                 `json:"schema_version"`
	Candidates    []RejectedCandidate `json:"candidates"`
}

func rejectedLedgerPath(home string) string {
	return filepath.Join(upgradeRoot(home), rejectedLedgerName)
}

// CandidateRejected reports whether these exact candidate bytes already reached a
// rollback on this box.
//
// Takes the bytes rather than a digest so no caller can compute the identity a
// different way than the ledger records it — the two must agree or the check silently
// passes everything.
//
// A ledger that cannot be read returns the error rather than false. "I could not tell"
// is not "it is fine": answering false on a read failure would re-activate the release
// that broke the box, which is the whole failure this prevents.
func CandidateRejected(home string, candidate []byte) (bool, RejectedCandidate, error) {
	ledger, err := readRejectedLedger(home)
	if err != nil {
		return false, RejectedCandidate{}, err
	}
	wanted := digest(candidate)
	for _, entry := range ledger.Candidates {
		if entry.SHA256 == wanted {
			return true, entry, nil
		}
	}
	return false, RejectedCandidate{}, nil
}

// RecordRejectedCandidate disqualifies a candidate for every later boot.
//
// Idempotent on the digest, because the supervisor re-enters its phases after an
// actor crash and would otherwise write the same entry repeatedly, evicting the
// history the cap exists to keep. A repeat refreshes the existing entry's timestamp
// and reason instead: the record moves to the front, so the most recently offending
// release is the last one the cap discards.
func RecordRejectedCandidate(home, sha256, version, reason string) error {
	if sha256 == "" {
		return errors.New("cannot reject an upgrade candidate with no digest")
	}
	root := upgradeRoot(home)
	if err := ensureDurableDirectory(home, root, transactionDirMode); err != nil {
		return fmt.Errorf("prepare the upgrade root for the rejected-candidate ledger: %w", err)
	}
	ledger, err := readRejectedLedger(home)
	if err != nil {
		return err
	}
	kept := make([]RejectedCandidate, 0, len(ledger.Candidates)+1)
	for _, entry := range ledger.Candidates {
		if entry.SHA256 != sha256 {
			kept = append(kept, entry)
		}
	}
	kept = append(kept, RejectedCandidate{
		SHA256:     sha256,
		Version:    version,
		Reason:     reason,
		RejectedAt: time.Now().UTC(),
	})
	// Newest last, so dropping from the front discards the oldest.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].RejectedAt.Before(kept[j].RejectedAt) })
	if len(kept) > maxRejectedCandidates {
		kept = kept[len(kept)-maxRejectedCandidates:]
	}
	encoded, err := json.MarshalIndent(rejectedLedger{SchemaVersion: journalSchemaVersion, Candidates: kept}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the rejected-candidate ledger: %w", err)
	}
	if err := durableAtomicWriteFile(rejectedLedgerPath(home), encoded, rejectedLedgerMode); err != nil {
		return fmt.Errorf("write the rejected-candidate ledger: %w", err)
	}
	return nil
}

// readRejectedLedger returns an empty ledger when the file does not exist, and an
// ERROR for anything else.
//
// Corrupt bytes are not treated as "no rejections". Doing so would silently re-arm
// every release this box has ever rolled back, and the one way the file gets damaged
// — a truncated write, a full disk — is also a moment when the box is least able to
// survive re-installing a broken binary. The ledger is written atomically, so a
// partial file means something outside this code touched it.
func readRejectedLedger(home string) (rejectedLedger, error) {
	path := rejectedLedgerPath(home)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rejectedLedger{SchemaVersion: journalSchemaVersion}, nil
	}
	if err != nil {
		return rejectedLedger{}, fmt.Errorf("read the rejected-candidate ledger: %w", err)
	}
	var ledger rejectedLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return rejectedLedger{}, fmt.Errorf("the rejected-candidate ledger at %s is unreadable: %w", path, err)
	}
	// A newer schema is not decoded on a guess. An older daemon that silently ignored
	// fields it did not understand could activate a release a newer one disqualified.
	if ledger.SchemaVersion > journalSchemaVersion {
		return rejectedLedger{}, fmt.Errorf(
			"the rejected-candidate ledger at %s is schema %d, newer than this binary understands (%d)",
			path, ledger.SchemaVersion, journalSchemaVersion,
		)
	}
	// Decoding successfully is not the same as being a ledger. `null`, `{}`, and
	// `{"candidates":[{}]}` are all valid JSON that unmarshal without error into a
	// zero value or a digest-less entry, and every one of them would read as "this
	// box has rejected nothing" — silently re-arming releases it rolled back. That is
	// the same outcome as a corrupt file, so it gets the same fail-closed answer
	// rather than a different one that happens to parse.
	if ledger.SchemaVersion < 1 {
		return rejectedLedger{}, fmt.Errorf(
			"the rejected-candidate ledger at %s has no schema version; refusing to read it as an empty ledger", path)
	}
	for i, entry := range ledger.Candidates {
		if !validDigest(entry.SHA256) {
			return rejectedLedger{}, fmt.Errorf(
				"the rejected-candidate ledger at %s has an entry (#%d) with no usable digest; refusing to read it as an empty ledger",
				path, i+1)
		}
	}
	return ledger, nil
}
