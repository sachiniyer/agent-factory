package upgradetxn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// The staged binary artifacts beside an executable are the ONLY thing one af home
// can see of another's upgrade transaction, and #2212 gate 4 is about what they are
// allowed to mean.
//
// foreignTransactionOver used to decide from the FILENAME alone: a
// `.<base>.af-upgrade-<id>.previous` present meant "a live transaction owns this
// binary", so Prepare refused. Cleanup that died after removing active.json but
// before deleting PreviousBinaryPath leaves exactly that file with nothing behind
// it — and the inference then blocks every future upgrade over that executable,
// permanently. The asymmetry is what makes it a gate: the in-place installer has
// --ignore-active-upgrade, while a daemon-owned upgrade has no escape hatch at all,
// so one stray dotfile fails every unattended upgrade until a human deletes it. That
// is the unoverridable-block shape this epic has already been bitten by (#2859).
//
// The three obvious repairs are all wrong, and stay rejected:
//
//   - READ THE OWNING JOURNAL — impossible from the filename, which names a
//     transaction id and no home, with no registry to resolve one against. That is
//     precisely why the artifact scan exists.
//   - AGE THE ARTIFACT OUT — a timestamp is never authority for overwriting a binary
//     a live transaction may still be preserving as its rollback source.
//   - GIVE THE DAEMON AN OVERRIDE — an unattended actor silently stepping over a
//     safety interlock is worse than the block it replaces.
//
// So the artifact is made SELF-DESCRIBING instead: alongside the staged binaries,
// Prepare writes an owner sidecar naming the transaction's home and the process that
// staged it. "Inert leftover" becomes a positive observation rather than an
// inference, and the cross-home objection dissolves — the sidecar carries the very
// pointer the filename lacked.
//
// FAIL-CLOSED ON DOUBT. An artifact with no readable sidecar keeps blocking exactly
// as it does today: it may have been staged by an af that predates this contract, or
// by a live transaction whose sidecar we simply cannot read, and neither is a licence
// to overwrite a binary. Only a sidecar that positively shows a finished transaction
// AND a dead owner unblocks. The narrow window that leaves — an artifact staged
// between this change and an operator's next upgrade — is bounded by activation being
// off by default, so no unattended path can reach it.

// artifactOwnerExt is the sidecar's suffix, sharing the `.<base>.af-upgrade-<id>`
// stem with the binaries it describes so one glob finds a transaction's whole
// footprint and cleanup cannot forget a member.
const artifactOwnerExt = ".owner.json"

// artifactOwnerMode keeps the sidecar readable by other homes' daemons — it carries
// no secret, only a home path and process identity — while staying owner-writable.
const artifactOwnerMode os.FileMode = 0o644

// artifactOwner is what a staged artifact says about itself. Every field exists to
// answer one question a filename cannot.
type artifactOwner struct {
	// ID ties the sidecar to its artifacts, so a stale sidecar beside a different
	// transaction's binaries is detectable rather than silently authoritative.
	ID string `json:"id"`
	// HomeDir is the AF home whose journal governs this transaction. It is the
	// pointer the filename never had, and it is what lets another home ask the only
	// question that actually settles liveness: is that transaction still active?
	HomeDir string `json:"home_dir"`
	// PID, StartID and BootID identify the process that staged the artifacts, so a
	// crashed prepare is distinguishable from one still running. StartID is compared
	// for equality only; BootID scopes it, since neither pid nor start stamp carries
	// authority across a reboot.
	PID     int    `json:"pid"`
	StartID uint64 `json:"start_id"`
	BootID  string `json:"boot_id"`
	// PIDNamespace scopes PID and StartID. BootID is host-global while both of those
	// are interpreted in the OBSERVER's namespace, so without this a dead stager's
	// tuple can match an unrelated long-running process in a scanning namespace and
	// block the executable forever — af homes in different namespaces do share one
	// executable (container sessions).
	PIDNamespace string `json:"pid_namespace_id"`
	// Journalled records that the transaction reached the point where its journal is
	// the authority. Before it, "no active.json" means the prepare has not written one
	// YET and only the stager's liveness can tell live from crashed; after it, the
	// journal's absence is the transaction being OVER, whatever the stager is doing.
	// Without this distinction a daemon that survives a failed cleanup keeps its own
	// inert artifact blocking — the headline case of #2212 gate 4, half unfixed.
	Journalled bool `json:"journalled"`
}

// artifactOwnerPath is the sidecar beside the staged binaries for one transaction.
func artifactOwnerPath(executable, id string) string {
	previous, _ := binaryArtifactPaths(executable, id)
	return strings.TrimSuffix(previous, ".previous") + artifactOwnerExt
}

// captureArtifactOwner records the calling process as the stager of id's artifacts.
// An identity that cannot be captured is an error rather than a partial record: a
// sidecar missing its process proofs would read as "unknown" forever and block the
// executable, which is the failure this whole change removes.
func captureArtifactOwner(home, id string) (artifactOwner, error) {
	pid := os.Getpid()
	self, err := proctree.Lookup(pid)
	if err != nil {
		return artifactOwner{}, fmt.Errorf("determine upgrade artifact owner identity: %w", err)
	}
	bootID, err := proctree.BootID()
	if err != nil {
		return artifactOwner{}, fmt.Errorf("determine kernel boot identity for upgrade artifact owner: %w", err)
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		return artifactOwner{}, fmt.Errorf("determine PID namespace for upgrade artifact owner: %w", err)
	}
	return artifactOwner{
		ID: id, HomeDir: home, PID: pid, StartID: self.StartID,
		BootID: bootID, PIDNamespace: pidNamespace,
	}, nil
}

// writeArtifactOwner persists the sidecar durably, because it is read by processes
// that will decide whether they may overwrite an executable: a record that survived
// the crash only partially would be worse than none.
func writeArtifactOwner(path string, owner artifactOwner) error {
	raw, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade artifact owner: %w", err)
	}
	return durableAtomicWriteFile(path, append(raw, '\n'), artifactOwnerMode)
}

// readArtifactOwner loads the sidecar for id. A record missing any field it is meant
// to prove with is rejected rather than half-trusted, so a truncated write reads as
// "unknown" and keeps the artifact blocking.
func readArtifactOwner(path, id string) (artifactOwner, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a path this package composed from its own artifact naming
	if err != nil {
		return artifactOwner{}, err
	}
	var owner artifactOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return artifactOwner{}, fmt.Errorf("decode upgrade artifact owner %s: %w", path, err)
	}
	// PID 1 is legitimate: af runs as a container entrypoint, and rejecting it would
	// make that home's artifacts unreadable — hence permanently blocking.
	if owner.ID != id || owner.HomeDir == "" || owner.PID < 1 || owner.StartID == 0 ||
		owner.BootID == "" || owner.PIDNamespace == "" {
		return artifactOwner{}, fmt.Errorf("upgrade artifact owner %s is incomplete", path)
	}
	return owner, nil
}

// artifactIsInert reports whether the transaction behind an artifact is definitively
// OVER, so the executable is free. It answers false for anything it cannot establish.
//
// Two conditions, and BOTH must hold. They are separate because they rule out
// different survivors:
//
//  1. The owning home has no active journal. That is the authoritative statement
//     that the transaction finished — and it is reachable only because the sidecar
//     names the home. A transaction whose process died mid-flight but whose journal
//     is still active is RESUMABLE, and its `.previous` is the rollback source; a
//     second transaction publishing over that executable would leave the two
//     interleaved. Liveness of the process alone would miss exactly that case.
//  2. The staging process is gone. A prepare still running has not written its
//     journal yet, so an absent active.json would otherwise read as "finished" during
//     the very window the interlock exists for.
//
// A boot change makes the process half moot rather than ambiguous: no pid recorded
// under a previous boot identifies anything now, so the process is gone by
// construction and only the journal question remains.
func artifactIsInert(executable, id string) (bool, error) {
	owner, err := readArtifactOwner(artifactOwnerPath(executable, id), id)
	if err != nil {
		// Unreadable, absent, or incomplete: not a licence to overwrite a binary.
		// Pre-contract artifacts land here and keep blocking, exactly as today.
		return false, nil
	}
	// A missing HOME is not a missing journal. An unmounted or absent home makes Lstat
	// on the journal report ErrNotExist for a reason that says nothing about the
	// transaction, so the two are distinguished before either is trusted.
	//
	// The HOME is what must exist, not its upgrade root: a transaction that finished
	// cleanly may have taken the upgrade tree with it, and that is precisely the state
	// this function is looking for.
	if _, err := os.Lstat(owner.HomeDir); err != nil {
		return false, nil
	}
	// The active journal must be THIS artifact's transaction, not merely any. A home
	// that finished transaction A and later started B over a different executable
	// would otherwise present B's active.json as proof that A is still live, and A's
	// leftover would block that executable for every other home, forever.
	active, err := readJournal(activeJournalPath(owner.HomeDir))
	switch {
	case err == nil:
		if active.ID == owner.ID {
			return false, nil // this transaction really is still active
		}
		// A different transaction is active in that home; it says nothing about ours.
	case errors.Is(err, ErrNoActiveTransaction):
		// No active transaction at all — the state this function is looking for.
		// readJournal reports absence as this sentinel, not as os.ErrNotExist.
	default:
		return false, nil // could not read the owning home: unknown is not inert
	}
	if owner.Journalled {
		// Past the journal handoff, the JOURNAL is the authority and it says finished.
		// Stager liveness must not be consulted here: a daemon that survives a failed
		// cleanup is still running, and gating on it would keep its own inert artifact
		// blocking forever — which is the failure this whole change removes.
		return true, nil
	}
	// Before the handoff there is no journal to have been removed, so "absent" cannot
	// mean finished. Only the stager can distinguish a prepare still running from one
	// that crashed mid-staging.
	alive, err := artifactOwnerAlive(owner)
	if err != nil {
		return false, nil
	}
	return !alive, nil
}

// artifactOwnerAlive reports whether the recorded staging process is still running as
// the SAME process instance. It compares pid AND start stamp under the recorded boot
// identity, so a recycled pid is not mistaken for the original.
func artifactOwnerAlive(owner artifactOwner) (bool, error) {
	bootID, err := proctree.BootID()
	if err != nil {
		return false, err
	}
	if bootID != owner.BootID {
		// A mismatch normally means another boot, where the recorded pid names nothing.
		// But proctree.BootID DELIBERATELY falls back to the PID-namespace id under a
		// subset=pid procfs, so two containers sharing this executable get different
		// ids within one host boot. Reading that as death would declare a LIVE
		// pre-journal stager in a sibling container dead and let a second transaction
		// publish over the same binary — the interlock failing open, which is far worse
		// than the block it guards.
		if proctree.BootIDIsFallback(bootID) || proctree.BootIDIsFallback(owner.BootID) {
			return false, fmt.Errorf("upgrade artifact owner boot identity is namespace-scoped; liveness is undecidable from here")
		}
		return false, nil
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	if err != nil {
		return false, err
	}
	if pidNamespace != owner.PIDNamespace {
		// The recorded pid is not interpretable here, so it cannot be shown dead —
		// and a tuple that happens to match a local process would be a coincidence,
		// not the stager. Unknown, which the caller treats as live.
		return false, fmt.Errorf("upgrade artifact owner pid %d belongs to another PID namespace", owner.PID)
	}
	current, err := proctree.Lookup(owner.PID)
	if err != nil {
		// ErrProcessExited is a POSITIVE statement that the pid named a process which
		// has exited but not yet been reaped (darwin reports this where Linux simply
		// has no /proc entry). Treating it as an error made a plainly dead stager read
		// as unknown, so the leftover kept blocking — on macOS only.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, proctree.ErrProcessExited) {
			return false, nil
		}
		return false, err
	}
	return current.StartID == owner.StartID, nil
}

// removeArtifactOwner deletes the sidecar. Called wherever the staged binaries are
// removed, so the sidecar never outlives the artifacts it describes — a sidecar
// without binaries is harmless, but one that lingers is a record nobody can act on.
func removeArtifactOwner(executable, id string) error {
	if err := os.Remove(artifactOwnerPath(executable, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove upgrade artifact owner: %w", err)
	}
	return nil
}
