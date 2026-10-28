package upgradetxn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/log"
)

// Deciding whether a binary staged beside the executable belongs to a LIVE
// transaction (#3864).
//
// A transaction is home-scoped; the executable is not. Two AF homes sharing one
// af binary each keep their own journal, each take their own per-home lock, and
// neither can see the other's — so both could hold an active transaction over
// the same file. binaryArtifactPaths stages the preserved-previous and candidate
// binaries NEXT TO the executable, which makes that directory the one place
// every home's transaction over that binary is visible, and the scan below is
// how each installer sees the others.
//
// What this file changes is the QUESTION the scan asks. It used to be "does a
// file matching the artifact name exist", and a filename is not evidence of a
// live transaction: a cleanup that died after removing active.json but before
// unlinking the preserved binary leaves an artifact that is inert in fact and
// live by that predicate. Nothing ages it out and nothing removes it — the
// transaction that owned it is over, so no recovery actor exists to clean it —
// so one stray dotfile refused every future daemon-owned upgrade of that binary,
// with no override on the unattended path (#2859's unoverridable-block shape).
//
// Live is now a positive answer, from one of two pieces of evidence:
//
//   - the owning home still has an active journal naming this transaction. The
//     artifact says which home that is: Prepare writes an owner record beside the
//     binaries it stages, so the pointer the filename lacked is on disk;
//   - the artifact is younger than stagedArtifactGrace. This covers the two
//     windows in which a live transaction legitimately has no journal to find —
//     a Prepare that has staged its binaries but not yet published active.json,
//     and a cleanup that has removed active.json and not yet unlinked them.
//
// Age is a necessary condition for calling something debris, never authority on
// its own: it can only ever make this scan refuse MORE. That is the difference
// between it and the rejected "age the artifact out" repair, which would have let
// a timestamp authorize overwriting a binary a live transaction was preserving.
//
// Everything else is debris or unreadable, and the two are not treated alike.
// Debris is set aside (below); an artifact whose owner record exists but whose
// home cannot be read keeps blocking, because an unreadable claim is still a
// claim. That block is overridable — the caller passes ClearUnverifiable — since
// a refusal an unattended daemon cannot get past is the failure this change
// exists to remove.

const (
	// stagedArtifactSuffix is the preserved-PREVIOUS binary, matched because that
	// is the rollback input a second transaction would put at risk.
	stagedArtifactSuffix = ".previous"
	// artifactOwnerSuffix is the record naming the home that staged the pair.
	artifactOwnerSuffix = ".owner"
	// artifactOwnerSchemaVersion guards the record's shape. A record from a newer
	// af is not decoded on a guess.
	artifactOwnerSchemaVersion = 1
	// artifactOwnerFileMode is deliberately world-readable: the record is READ by
	// other AF homes, which on a shared install (/usr/local/bin) can be other
	// users, and it carries a home path and a timestamp — nothing secret. A
	// private mode would make every cross-user scan unverifiable, which is the
	// blocking case this change is trying to shrink.
	artifactOwnerFileMode = 0o644
)

// stagedArtifactGrace is how long after staging an artifact is presumed live
// without any journal to prove it. It covers Prepare's pre-journal window and
// cleanup's post-journal window, both of which are seconds; an hour is generous
// for a slow copy-and-fsync on a loaded box.
//
// Lengthening it is always safe and only delays clearing debris. Shortening it
// is the direction that needs an argument, which is why it is not a config key.
var stagedArtifactGrace = time.Hour

// ArtifactEvidence is what the directory could actually establish about one
// staged artifact.
type ArtifactEvidence int

const (
	// ArtifactLive means a transaction still owns it: its home says so, or it is
	// too young for absence to mean anything.
	ArtifactLive ArtifactEvidence = iota + 1
	// ArtifactFinished means the owning home was reachable and is not running
	// this transaction any more.
	ArtifactFinished
	// ArtifactUnattributed means nothing records who staged it — a leftover from
	// an af that predates the owner record, which is every leftover in the wild
	// today — and it is older than the grace.
	ArtifactUnattributed
	// ArtifactUnverifiable means there IS a claim and it could not be read.
	ArtifactUnverifiable
)

// ArtifactOwner is the record Prepare writes beside the binaries it stages. It
// carries the one fact the filename cannot: which AF home to ask.
//
// Deliberately NOT a process identity. An earlier attempt at this recorded the
// staging pid, its start stamp, the boot id and the PID namespace, and every one
// of those turned out to be an unsound proxy across container and namespace
// boundaries (#2984). The home pointer plus the owning journal is the whole
// contract here; liveness of a process is never consulted.
type ArtifactOwner struct {
	SchemaVersion int       `json:"schema_version"`
	TransactionID string    `json:"transaction_id"`
	HomeDir       string    `json:"home_dir"`
	StagedAt      time.Time `json:"staged_at"`
}

// StagedArtifact is one preserved-previous binary found beside an executable,
// with the verdict the evidence supports and the reason in words a refusal can
// quote.
type StagedArtifact struct {
	ID            string
	PreviousPath  string
	CandidatePath string
	OwnerPath     string
	Evidence      ArtifactEvidence
	// Reason is a clause, not a sentence: "its transaction is still active in
	// /home/x/.agent-factory". Callers compose the refusal around it, because the
	// remedy differs between an interactive installer with a flag and an
	// unattended daemon with a config key.
	Reason string
}

// ArtifactScanOptions is the caller's policy. The scan itself has none: the same
// evidence produces the same verdict for both installers, which is what keeps
// them from disagreeing about the same directory.
type ArtifactScanOptions struct {
	// Clear allows debris to be set aside. Only a caller holding the executable
	// lock passes true — the unlocked launch probe reads, and never writes.
	Clear bool
	// ClearUnverifiable extends that to an artifact whose owner record cannot be
	// resolved. The operator's explicit "I have looked, clear it": the safe
	// default is to refuse with the path named, and this is the way past it.
	ClearUnverifiable bool
}

// ErrForeignStagedArtifact is what Prepare refuses with when another
// transaction's binaries are staged over the same executable. Exported so the
// unattended caller can recognise this refusal and tell an operator how to get
// past it — a daemon that only logs "prepare failed" every six hours is the loop
// this change exists to end.
var ErrForeignStagedArtifact = errors.New("another upgrade transaction is staging over this executable")

// artifactOwnerPath is the owner record for one transaction's staged pair.
func artifactOwnerPath(executable, id string) string {
	previous, _ := binaryArtifactPaths(executable, id)
	return artifactOwnerPathForPrevious(previous)
}

// artifactOwnerPathForPrevious derives the record's path from the preserved
// binary's, for callers that hold a journal rather than an id — the journal's
// recorded path is the one its artifacts were actually created under.
func artifactOwnerPathForPrevious(previousPath string) string {
	return strings.TrimSuffix(previousPath, stagedArtifactSuffix) + artifactOwnerSuffix
}

// writeArtifactOwner records the owner durably. Written BEFORE the binaries it
// describes and removed AFTER them, so a crash can leave a record with no
// artifacts — which blocks nothing — and never artifacts with no record, which
// is the leftover this whole file exists to stop being permanent.
func writeArtifactOwner(path string, owner ArtifactOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encode upgrade artifact owner: %w", err)
	}
	return durableAtomicWriteFile(path, append(data, '\n'), artifactOwnerFileMode)
}

// readArtifactOwner reads the record for id, refusing one that names a different
// transaction rather than trusting it. A record whose id does not match its own
// filename describes something this scan cannot reason about, and guessing which
// half is right is exactly the kind of inference this design removed.
func readArtifactOwner(path, id string) (ArtifactOwner, error) {
	// O_NOFOLLOW, and a regular-file check on the descriptor. This record decides
	// whether af may move a binary out of the way, so it is not read through a
	// symlink somebody dropped in the executable's directory. It is not a trust
	// boundary — a writer there can already replace the af binary itself, which is
	// the same argument the rejected-candidate ledger records (#3011) — but a read
	// that can be redirected by accident, or by an unprivileged tmp-file trick in a
	// world-writable install directory, is worth not having. Every failure here
	// lands on ArtifactUnverifiable, which blocks.
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return ArtifactOwner{}, fmt.Errorf("upgrade artifact owner %s is not a regular file", path)
		}
		return ArtifactOwner{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return ArtifactOwner{}, err
	}
	if !info.Mode().IsRegular() {
		return ArtifactOwner{}, fmt.Errorf("upgrade artifact owner %s is not a regular file", path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ArtifactOwner{}, err
	}
	var owner ArtifactOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return ArtifactOwner{}, fmt.Errorf("decode upgrade artifact owner %s: %w", path, err)
	}
	if owner.SchemaVersion != artifactOwnerSchemaVersion {
		return ArtifactOwner{}, fmt.Errorf("upgrade artifact owner %s has unsupported schema version %d", path, owner.SchemaVersion)
	}
	if owner.TransactionID != id {
		return ArtifactOwner{}, fmt.Errorf("upgrade artifact owner %s names transaction %q, not %q", path, owner.TransactionID, id)
	}
	if owner.HomeDir == "" || !filepath.IsAbs(owner.HomeDir) {
		return ArtifactOwner{}, fmt.Errorf("upgrade artifact owner %s names no absolute home", path)
	}
	return owner, nil
}

// BlockingStagedArtifact returns the artifact that must stop a caller from
// publishing over executable, or nil when none does. It is the ONE predicate
// both installers use: upgradetxn.Prepare on the transactional path, and the
// in-place swap in commands. Two scanners with two policies over the same
// directory would eventually disagree about the same file, and the one that said
// "proceed" would be the one that destroyed a rollback.
//
// Debris it is allowed to clear is set aside as it goes, so the answer reflects
// what the directory holds AFTER the sweep rather than before it.
func BlockingStagedArtifact(executable, selfID string, opts ArtifactScanOptions) (*StagedArtifact, error) {
	artifacts, err := scanStagedArtifacts(executable, selfID)
	if err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		if artifact.clearable(opts) {
			if err := setAsideStagedArtifact(artifact); err != nil {
				// Failing to clear debris is not permission to publish over it.
				// The caller gets the artifact back, with why, and refuses.
				artifact.Evidence = ArtifactUnverifiable
				artifact.Reason = fmt.Sprintf("%s, but it could not be set aside (%v)", artifact.Reason, err)
				return &artifact, nil
			}
			continue
		}
		if artifact.blocks(opts) {
			return &artifact, nil
		}
	}
	return nil, nil
}

// blocks reports whether this artifact refuses a second transaction.
func (a StagedArtifact) blocks(opts ArtifactScanOptions) bool {
	switch a.Evidence {
	case ArtifactLive:
		return true
	case ArtifactUnverifiable:
		return !opts.ClearUnverifiable
	default:
		return false
	}
}

// clearable reports whether this caller may set the artifact aside.
func (a StagedArtifact) clearable(opts ArtifactScanOptions) bool {
	if !opts.Clear {
		return false
	}
	switch a.Evidence {
	case ArtifactFinished, ArtifactUnattributed:
		return true
	case ArtifactUnverifiable:
		return opts.ClearUnverifiable
	default:
		return false
	}
}

// scanStagedArtifacts lists every preserved-previous binary staged beside
// executable by a transaction other than selfID, classified.
//
// Read with ReadDir and a literal prefix rather than filepath.Glob: an
// executable whose name contains a glob metacharacter would otherwise match the
// wrong set, and this decides whether to overwrite a binary.
func scanStagedArtifacts(executable, selfID string) ([]StagedArtifact, error) {
	dir := filepath.Dir(executable)
	prefix := "." + filepath.Base(executable) + ".af-upgrade-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect %s for other upgrade transactions: %w", dir, err)
	}
	var artifacts []StagedArtifact
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, stagedArtifactSuffix) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), stagedArtifactSuffix)
		if id == "" || id == selfID {
			continue
		}
		artifacts = append(artifacts, classifyStagedArtifact(executable, id, now))
	}
	return artifacts, nil
}

// classifyStagedArtifact decides what the directory can establish about one
// artifact: what its owner record says first, then youth, which can only ever
// move a verdict back towards live.
func classifyStagedArtifact(executable, id string, now time.Time) StagedArtifact {
	previous, candidate := binaryArtifactPaths(executable, id)
	artifact := StagedArtifact{
		ID:            id,
		PreviousPath:  previous,
		CandidatePath: candidate,
		OwnerPath:     artifactOwnerPath(executable, id),
	}
	artifact.Evidence, artifact.Reason = artifactEvidenceFromOwner(artifact.OwnerPath, id)
	if artifact.Evidence == ArtifactLive {
		return artifact
	}
	// Youth outranks every not-live verdict above, because both of the windows in
	// which a live transaction has no journal to find are short and recent: a
	// Prepare that has staged its binaries and not yet published active.json, and
	// a cleanup that has removed active.json and not yet unlinked them. Neither is
	// distinguishable from debris by any other signal.
	age, known := stagedArtifactAge(artifact, now)
	switch {
	case !known:
		artifact.Evidence = ArtifactLive
		artifact.Reason = "af cannot tell how long it has been staged, and an artifact of unknown age may belong to a transaction still publishing"
	case age < stagedArtifactGrace:
		artifact.Evidence = ArtifactLive
		artifact.Reason = fmt.Sprintf("it was staged %s ago, so a transaction may still be publishing or cleaning up", age.Round(time.Second))
	}
	return artifact
}

// artifactEvidenceFromOwner asks the home the artifact names.
//
// The four answers are deliberately distinct. "No record at all" is every
// leftover staged by an af that predates this contract, and treating it as a
// live claim is what made those permanent. "The home says another transaction is
// active" is not a licence for that transaction to hold THIS artifact — an
// earlier design read it as one, and a finished transaction's leftover then
// blocked every other home for the whole life of an unrelated one. And a home af
// cannot reach is NOT a finished transaction, because an unreachable filesystem
// reports exactly what a completed cleanup does.
func artifactEvidenceFromOwner(ownerPath, id string) (ArtifactEvidence, string) {
	owner, err := readArtifactOwner(ownerPath, id)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ArtifactUnattributed, "nothing beside it records which agent-factory home staged it"
	case err != nil:
		return ArtifactUnverifiable, fmt.Sprintf("its owner record cannot be read (%v)", err)
	}
	journal, err := readJournal(activeJournalPath(owner.HomeDir))
	switch {
	case err == nil && journal.ID == id:
		return ArtifactLive, fmt.Sprintf("still active in %s", owner.HomeDir)
	case err == nil && journal.ID == "":
		// It decoded, and it names nothing. That is not the home telling us it has
		// moved on to other work — it is a journal we cannot interpret, and the
		// only honest reading of "the transaction that owns this is not the one
		// named here" is one where something IS named there.
		return ArtifactUnverifiable, fmt.Sprintf("the upgrade journal in its home %s names no transaction", owner.HomeDir)
	case err == nil:
		return ArtifactFinished, fmt.Sprintf("its home %s is running transaction %s now, not this one", owner.HomeDir, journal.ID)
	case errors.Is(err, ErrNoActiveTransaction):
		// The home has to be THERE for the journal's absence to mean anything.
		// Checked on the home itself rather than its upgrade root, because a
		// transaction that finished cleanly takes that root with it — which is
		// precisely the state this is looking for.
		if _, statErr := os.Stat(owner.HomeDir); statErr != nil {
			return ArtifactUnverifiable, fmt.Sprintf(
				"its home %s cannot be read, so the absence of an upgrade journal there proves nothing (%v)", owner.HomeDir, statErr)
		}
		return ArtifactFinished, fmt.Sprintf("its home %s has no active upgrade journal", owner.HomeDir)
	default:
		return ArtifactUnverifiable, fmt.Sprintf("the upgrade journal in its home %s cannot be read (%v)", owner.HomeDir, err)
	}
}

// stagedArtifactAge is how long ago this transaction last touched the directory,
// taken as the NEWEST of the files it staged.
//
// Newest, not oldest: the question is whether anything recent has happened here,
// and the conservative answer to "one of these was written moments ago" is that
// a transaction may still be working. A file whose timestamp cannot be read at
// all makes the age unknown rather than old, and a timestamp in the future reads
// as brand new — both of which keep the artifact live.
func stagedArtifactAge(a StagedArtifact, now time.Time) (time.Duration, bool) {
	var newest time.Time
	seen := false
	for _, path := range []string{a.PreviousPath, a.CandidatePath, a.OwnerPath} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, false
		}
		seen = true
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if !seen {
		return 0, false
	}
	age := now.Sub(newest)
	if age < 0 {
		age = 0
	}
	return age, true
}

// setAsideStagedArtifact renames debris out of the scan's way rather than
// deleting it, and removes the owner record last.
//
// A rename, not an unlink, and the distinction is the whole safety argument for
// clearing at all. Every verdict here rests on absence — no journal, no record,
// no recent activity — and absence is the one kind of evidence a filesystem can
// misreport: an unmounted home leaves its mount point present and its journal
// unreadable, so a live transaction can look finished. Deleting its preserved
// binary would destroy the rollback it is holding. Renaming costs no disk (the
// bytes never move) and leaves them recoverable at a path this logs, while the
// interlock stops seeing a claim that nothing is making.
func setAsideStagedArtifact(a StagedArtifact) error {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	moved := make([]string, 0, 2)
	// The preserved binary goes LAST of the two, because it is the file the scan
	// keys on: a rename that fails partway then leaves an artifact the next sweep
	// still finds, rather than a half-cleared transaction that is invisible to it.
	for _, path := range []string{a.CandidatePath, a.PreviousPath} {
		aside, err := renameStagedArtifactAside(path, stamp)
		if err != nil {
			return err
		}
		if aside != "" {
			moved = append(moved, aside)
		}
	}
	// LAST, after the binaries it describes: a crash between the two leaves a
	// record with no artifacts, which blocks nothing, rather than artifacts with
	// no record, which is the leftover this exists to prevent.
	if err := removeDurableFile(a.OwnerPath); err != nil {
		return fmt.Errorf("remove upgrade artifact owner record %s: %w", a.OwnerPath, err)
	}
	log.WarningLog.Printf(
		"upgrade interlock: upgrade transaction %s left staged binaries beside the executable and %s; they are no longer treated as a live upgrade and have been set aside as %s — delete them once you are satisfied nothing needs that rollback",
		a.ID, a.Reason, strings.Join(moved, ", "),
	)
	return nil
}

// renameStagedArtifactAside moves one file out of the scanned name space,
// returning where it went. An already-absent file is not an error: a half-cleaned
// transaction is precisely what this is sweeping.
func renameStagedArtifactAside(path, stamp string) (string, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("inspect staged upgrade artifact %s: %w", path, err)
	}
	aside := fmt.Sprintf("%s.debris-%s", path, stamp)
	for i := 1; ; i++ {
		if _, err := os.Lstat(aside); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", fmt.Errorf("inspect set-aside path %s: %w", aside, err)
		}
		aside = fmt.Sprintf("%s.debris-%s-%d", path, stamp, i)
	}
	if err := os.Rename(path, aside); err != nil {
		return "", fmt.Errorf("set aside staged upgrade artifact %s: %w", path, err)
	}
	if err := syncTransactionDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("confirm the set-aside of %s: %w", path, err)
	}
	return aside, nil
}
