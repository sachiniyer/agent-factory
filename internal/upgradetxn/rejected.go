package upgradetxn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"io"
	"syscall"
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
//
// It is keyed by EXECUTABLE, not by home. One installation can serve several
// AGENT_FACTORY_HOMEs (see commands/upgrade_interlock.go, which takes an
// executable-scoped lock for exactly this reason), and the thing a bad candidate
// breaks is the shared binary. A per-home ledger would let home B reinstall the
// bytes home A had just rolled back, over the executable they share — defeating A's
// quarantine and breaking every home using that installation. So the record sits
// beside the binary it disqualifies, in the same dotfile convention the staged
// upgrade artifacts already use.

// rejectedLedgerSuffix names the ledger beside the executable it governs, matching
// the ".<base>.af-upgrade-<id>.previous" convention binaryArtifactPaths already uses.
const rejectedLedgerSuffix = ".af-rejected-candidates.json"

// maxRejectedCandidates bounds the ledger. It is a disqualification list for
// binaries this box actually installed and rolled back, so it grows only when an
// upgrade genuinely fails; a box that hits 32 distinct bad releases has a problem no
// ledger will fix. Oldest entries are dropped first, which is the right direction:
// the release that broke you yesterday matters more than one from a year ago that
// nothing will offer again.
const maxRejectedCandidates = 32

// rejectedLedgerMode is 0600 on a privately-owned installation. The ledger decides
// whether a binary may be activated, so a user who can write it can re-enable a
// release this box rejected.
const rejectedLedgerMode os.FileMode = 0o600

// rejectedLedgerSharedMode is the mode on a SHARED install directory, and it is
// widened for the same reason the install lock is (#3011).
//
// The ledger is scoped to an executable, not to a home, so on a group-writable
// /usr/local/bin every authorized writer consults the same file. Owner-only, the
// first user's rollback creates a ledger nobody else can read — and every other
// member's updater then fails closed on EVERY release, while their own rollback
// fails mid-record and leaves recovery retrying in PhaseRolledBack with no
// cleanup. A safety mechanism that silently disables upgrades for everyone but
// its creator is worse than the risk it manages.
//
// Widening gives away nothing. The audience is exactly directoryWriterGroup — the
// set that can already REPLACE the binary this ledger is about. Someone who can
// swap the executable outright does not need to edit a ledger to install what
// they like, so the 0600 justification above simply does not describe them.
const rejectedLedgerSharedMode os.FileMode = 0o660

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

// rejectedLedgerPath places the ledger beside the executable, RESOLVED.
//
// Resolution is load-bearing rather than tidiness. Prepare canonicalizes the
// executable before storing it in the journal, so the supervisor writes the
// ledger beside the resolved target — while a daemon started through a symlink
// gets that symlink back from os.Executable and would look beside the LINK. The
// reader would then find no ledger and reactivate the exact candidate a rollback
// had just rejected. Doing it here means every caller agrees on the location by
// construction, instead of each one remembering to resolve first (#2212 review).
//
// A path that cannot be resolved — it does not exist yet, or the link is broken —
// falls back to the literal path. That is the pre-existing behaviour and is the
// safe direction: at worst the ledger is written somewhere a resolved reader
// also computes identically, since both sides run this same function.
func rejectedLedgerPath(executable string) string {
	resolved := executable
	if real, err := filepath.EvalSymlinks(executable); err == nil {
		resolved = real
	}
	return filepath.Join(filepath.Dir(resolved),
		"."+filepath.Base(resolved)+rejectedLedgerSuffix)
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
func CandidateRejected(executable string, candidate []byte) (bool, RejectedCandidate, error) {
	ledger, err := readRejectedLedger(executable)
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
func RecordRejectedCandidate(executable, sha256, version, reason string) error {
	if sha256 == "" {
		return errors.New("cannot reject an upgrade candidate with no digest")
	}
	if strings.TrimSpace(executable) == "" {
		return errors.New("cannot reject an upgrade candidate without the executable it applies to")
	}
	// The whole read-modify-write runs under the EXECUTABLE install lock (#3011
	// review). Moving the readers inside their locks was only half of it: a
	// publication that is not serialised against installs of the same executable can
	// land between a locked reader's read and its swap, and two concurrent rollbacks
	// can lose one another's entry outright.
	//
	// The executable lock rather than the full install lock, because that is the
	// scope the ledger has: it is keyed to the executable and shared by every home
	// that installs it, which is exactly the audience this must serialise against.
	// None of the three callers holds it — they are supervisor phases, and Prepare
	// releases it before the supervisor runs — so this cannot self-deadlock.
	// A FAILURE TO TAKE THE LOCK MUST NOT BLOCK THE RECORD, the same polarity the
	// in-place installer states for its own lock. The executable can be absent or its
	// lock storage broken, and neither says anything about whether this candidate
	// should be disqualified — while failing here strands recovery in
	// PhaseRolledBack, retrying forever, with the candidate still installable. So the
	// lock is an ordering guarantee when it can be had, not a precondition.
	//
	// recorded separates "the lock could not be taken" from "the write itself
	// failed": withExecutableLock returns fn's error too, so the error alone cannot
	// tell them apart, and retrying a genuine write failure unlocked would be
	// pointless while silently skipping the lock on every call would be worse.
	recorded := false
	var writeErr error
	lockErr := withExecutableLock(executable, false, func() error {
		recorded = true
		writeErr = recordRejectedCandidateLocked(executable, sha256, version, reason)
		return writeErr
	})
	if recorded {
		return writeErr
	}
	log.WarningLog.Printf("could not take the executable lock for %s to publish the rejected-candidate ledger, recording unserialised: %v", executable, lockErr)
	return recordRejectedCandidateLocked(executable, sha256, version, reason)
}

func recordRejectedCandidateLocked(executable, sha256, version, reason string) error {
	ledger, err := readRejectedLedger(executable)
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
	// Order is INSERTION order, deliberately not sorted by RejectedAt. A clock that
	// steps backwards — NTP correcting a drifted box, which is exactly the sort of
	// box that has been rebooting into a bad release — would sort the entry just
	// appended ahead of older ones, and the cap below would then discard it while
	// this function still returned success. The supervisor would disable recovery
	// believing the candidate was disqualified when it had just been dropped.
	if len(kept) > maxRejectedCandidates {
		kept = kept[len(kept)-maxRejectedCandidates:]
	}
	encoded, err := json.MarshalIndent(rejectedLedger{SchemaVersion: journalSchemaVersion, Candidates: kept}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the rejected-candidate ledger: %w", err)
	}
	path := rejectedLedgerPath(executable)
	mode := rejectedLedgerMode
	gid, shared := directoryWriterGroup(filepath.Dir(path))
	if !shared {
		if err := durableAtomicWriteFile(path, encoded, mode); err != nil {
			return fmt.Errorf("write the rejected-candidate ledger: %w", err)
		}
		return nil
	}
	// Shared install directory: the ledger must carry the DIRECTORY's group, not the
	// creator's primary group, or the other authorized writers cannot read it.
	//
	// Established on the temporary inode before the rename rather than chowned after
	// (#3011 review). A post-rename chown can fail with EPERM on a group-writable,
	// non-setgid directory whose owner is not in its group — and by then the ledger
	// is already published 0660 under the WRONG group: unreadable by the people who
	// need it and writable by an unrelated one. Falling back to the private mode
	// keeps the second half from happening; the first half is reported.
	grouped, err := durableAtomicWriteFileInGroup(path, encoded, rejectedLedgerSharedMode, mode, gid)
	if err != nil {
		return fmt.Errorf("write the rejected-candidate ledger: %w", err)
	}
	if !grouped {
		// Logged, not returned. The ledger IS durable and correct — it is simply
		// private to this writer — and this record is what lets recovery leave
		// PhaseRolledBack. Failing it would reproduce the stuck-in-rollback half of
		// the problem the widening fixes, in exchange for a permission bit.
		log.WarningLog.Printf(
			"rejected-candidate ledger %s was written private (mode %v) because its directory's group %d could not be set, so other authorized writers may not be able to read it",
			path, mode, gid)
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
// alignRejectedLedgerWithDirectoryWriters re-narrows an existing ledger whose
// directory has since been tightened (#3011 review).
//
// The widening to 0660 is justified only while the directory is group-writable:
// the audience is exactly the set that can already REPLACE the binary the ledger is
// about. Tighten the directory from 0770 to 0750 and that stops being true, but the
// ledger keeps its mode — and former group writers retain SEARCH permission on the
// directory, so they can still rewrite it. Publishing a valid empty ledger there
// makes the owner's unattended updater reinstall the very bytes it disqualified.
//
// So the mode is realigned where the ledger is consulted, which is the same place
// and the same reasoning as alignLockWithDirectoryWriters: a shared directory
// widens, an unshared one narrows. Best-effort — a failure leaves the ledger
// readable and the caller proceeds, because refusing to read it would fail every
// upgrade closed over a permission bit.
func alignRejectedLedgerWithDirectoryWriters(path string) {
	// Lstat, and refuse a symlink outright (#3011 review). A group writer can replace
	// the ledger with a link before an administrator tightens the directory; chmod
	// through it would then restyle whatever it points at — with this process's
	// privileges, on a path the attacker chose. Nothing here is worth that, and the
	// reader below fails on the contents anyway.
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		log.WarningLog.Printf("rejected-candidate ledger %s is a symlink; refusing to adjust its mode", path)
		return
	}
	gid, shared := directoryWriterGroup(filepath.Dir(path))
	want := rejectedLedgerMode
	if shared {
		want = rejectedLedgerSharedMode
		// The group, not just the mode (#3011 review). A private installation that
		// becomes shared — or one moved to a DIFFERENT collaboration group — leaves
		// the ledger under its creator's primary group, so 0660 widens it to the
		// wrong audience: still unreadable by the directory's writers, now writable
		// by an unrelated group. Group before mode, because chown clears setuid and
		// setgid bits and would undo a mode set first; and if the chown fails the
		// mode is left alone rather than widened to the wrong group, which is the
		// same rule alignLockWithDirectoryWriters follows.
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Gid) != gid {
			if err := os.Chown(path, -1, gid); err != nil {
				log.WarningLog.Printf("rejected-candidate ledger %s could not be given its directory's group %d, leaving its mode alone: %v", path, gid, err)
				return
			}
		}
	}
	if info.Mode().Perm() == want {
		return
	}
	// Both directions, not just narrowing. An installation that BECOMES shared leaves
	// a pre-existing 0600 ledger owner-only, and every other authorized writer's
	// updater then fails closed on every release — the exact harm the widening was
	// introduced to prevent, reached by a different route.
	if err := os.Chmod(path, want); err != nil {
		// Reported, not silent. A revoke that fails is the interesting case: the
		// directory has been tightened and the ledger is still group-writable, so
		// somebody who can no longer replace the binary can still rewrite the record
		// that decides which binaries get installed. The caller proceeds — refusing
		// to read would fail every upgrade closed over a permission bit — but this
		// must not pass unmentioned.
		log.WarningLog.Printf("rejected-candidate ledger %s could not be set to mode %v (its directory is shared=%v): %v",
			path, want, shared, err)
	}
}

func readRejectedLedger(executable string) (rejectedLedger, error) {
	path := rejectedLedgerPath(executable)
	alignRejectedLedgerWithDirectoryWriters(path)
	// O_NOFOLLOW, because this read FAILS OPEN if it is fooled (#3011 review). A
	// former shared-directory writer who replaces the ledger with a symlink to a
	// structurally valid empty one does not corrupt anything — the file parses, the
	// list is empty, and every disqualified release becomes installable again. The
	// corrupt-bytes path below cannot catch that: there is nothing corrupt about it.
	// Refusing to follow the link turns a silent re-arm into a read error, and a read
	// error fails closed at every caller.
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer func() { _ = handle.Close() }()
	}
	var data []byte
	if err == nil {
		data, err = io.ReadAll(handle)
	}
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
