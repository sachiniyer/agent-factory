package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// deadOwner builds a complete sidecar for a stager that is definitively gone, scoped
// to this boot and PID namespace so only its LIVENESS is what the test varies.
func deadOwner(t *testing.T, id, home string) artifactOwner {
	t.Helper()
	ns, err := proctree.PIDNamespaceID()
	if err != nil {
		t.Fatalf("PIDNamespaceID: %v", err)
	}
	return artifactOwner{
		ID: id, HomeDir: home, PID: 999999, StartID: 42,
		BootID: currentBootIDForTest(t), PIDNamespace: ns,
	}
}

// currentBootIDForTest returns this kernel's boot identity, so a fixture owner is
// scoped to the boot the test runs in.
func currentBootIDForTest(t *testing.T) string {
	t.Helper()
	id, err := proctree.BootID()
	if err != nil {
		t.Fatalf("BootID: %v", err)
	}
	return id
}

// #2212 gate 4: a staged artifact must say whether it is LIVE, so an inert leftover
// stops blocking every future upgrade over the executable.
//
// The old scan read the filename alone, so a `.previous` left behind when cleanup
// died after removing active.json but before deleting PreviousBinaryPath read as a
// live foreign transaction — and Prepare refused forever, with no escape hatch for a
// daemon-owned upgrade.

// stageArtifact writes a `.previous` for id beside executable, with an optional owner
// sidecar, and returns the executable path.
func stageArtifact(t *testing.T, dir, id string, owner *artifactOwner) string {
	t.Helper()
	executable := filepath.Join(dir, "af")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write executable: %v", err)
	}
	previous, _ := binaryArtifactPaths(executable, id)
	if err := os.WriteFile(previous, []byte("previous"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write previous artifact: %v", err)
	}
	if owner != nil {
		if err := writeArtifactOwner(artifactOwnerPath(executable, id), *owner); err != nil {
			t.Fatalf("write owner: %v", err)
		}
	}
	return executable
}

// TestForeignTransaction_InertLeftoverDoesNotBlock is the regression. The owning home
// has no active journal and the staging process is gone, so the artifact is a
// leftover and must not be reported as a live foreign transaction.
func TestForeignTransaction_InertLeftoverDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir() // no active.json: the transaction finished
	owner := deadOwner(t, "dead", home)
	owner.Journalled = true
	executable := stageArtifact(t, dir, "dead", &owner)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "" {
		t.Fatalf("an inert leftover must not block a new transaction; got foreign=%q", foreign)
	}
}

// TestForeignTransaction_ActiveJournalStillBlocks is the half that keeps the
// interlock real. The staging process is gone, but the owning home's journal is still
// active — so the transaction is RESUMABLE and its `.previous` is the rollback source.
// Process liveness alone would have missed this and let two transactions interleave
// over one executable.
func TestForeignTransaction_ActiveJournalStillBlocks(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	active := activeJournalPath(home)
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The active journal must NAME this artifact's transaction: a home running some
	// OTHER transaction says nothing about this one.
	if err := os.WriteFile(active, []byte(`{"id":"crashed"}`), 0o600); err != nil {
		t.Fatalf("write active journal: %v", err)
	}
	owner := deadOwner(t, "crashed", home)
	owner.Journalled = true
	executable := stageArtifact(t, dir, "crashed", &owner)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "crashed" {
		t.Fatalf("a resumable transaction must still block; got foreign=%q", foreign)
	}
}

// TestForeignTransaction_LiveOwnerStillBlocks covers the other survivor: a prepare
// still running has not written its journal yet, so an absent active.json would read
// as "finished" during the very window the interlock exists for.
func TestForeignTransaction_LiveOwnerStillBlocks(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir() // no active.json yet — mid-prepare
	self, err := captureArtifactOwner(home, "running")
	if err != nil {
		t.Fatalf("captureArtifactOwner: %v", err)
	}
	executable := stageArtifact(t, dir, "running", &self)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "running" {
		t.Fatalf("a live staging process must still block; got foreign=%q", foreign)
	}
}

// TestForeignTransaction_UnknownArtifactFailsClosed pins the bias. An artifact with
// no readable sidecar — one staged by an af predating this contract, or a truncated
// write — keeps blocking exactly as it does today. Unknown is never a licence to
// overwrite a binary.
func TestForeignTransaction_UnknownArtifactFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner *artifactOwner
	}{
		{"no sidecar at all", nil},
		{"incomplete record", &artifactOwner{ID: "old", HomeDir: "", PID: 0}},
		{"no pid namespace (pre-contract)", &artifactOwner{ID: "old", HomeDir: t.TempDir(), PID: 4242, StartID: 7, BootID: currentBootIDForTest(t)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable := stageArtifact(t, t.TempDir(), "old", tc.owner)
			foreign, err := foreignTransactionOver(executable, "mine")
			if err != nil {
				t.Fatalf("foreignTransactionOver: %v", err)
			}
			if foreign != "old" {
				t.Fatalf("an artifact we cannot read must keep blocking; got foreign=%q", foreign)
			}
		})
	}
}

// TestArtifactOwner_MismatchedIDIsNotAuthoritative: a sidecar left beside a DIFFERENT
// transaction's binaries must not speak for them, or a stale record could unblock an
// artifact it never described.
func TestArtifactOwner_MismatchedIDIsNotAuthoritative(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	executable := filepath.Join(dir, "af")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write executable: %v", err)
	}
	// A sidecar claiming id "other", written at id "dead"'s path.
	owner := deadOwner(t, "other", home)
	if err := writeArtifactOwner(artifactOwnerPath(executable, "dead"), owner); err != nil {
		t.Fatalf("write owner: %v", err)
	}
	if _, err := readArtifactOwner(artifactOwnerPath(executable, "dead"), "dead"); err == nil {
		t.Fatal("a sidecar naming another transaction must be rejected, not trusted")
	}
}

// TestForeignTransaction_SurvivingStagerDoesNotBlockAfterJournalRemoval is the
// headline case, and my first cut only fixed half of it.
//
// When AbortPreparedTransaction removes active.json and cleanup then fails before
// deleting `.previous` — a recovery unit that no longer matches, say — the initiating
// daemon is STILL RUNNING. Gating on stager liveness kept that daemon's own inert
// artifact blocking forever, which is exactly the failure this change exists to
// remove. Past the journal handoff the journal is the authority, whatever the stager
// is doing.
func TestForeignTransaction_SurvivingStagerDoesNotBlockAfterJournalRemoval(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir() // journal already removed
	if err := os.MkdirAll(upgradeRoot(home), 0o700); err != nil {
		t.Fatalf("mkdir upgrade root: %v", err)
	}
	// The stager is THIS process — alive by construction.
	self, err := captureArtifactOwner(home, "aborted")
	if err != nil {
		t.Fatalf("captureArtifactOwner: %v", err)
	}
	self.Journalled = true
	executable := stageArtifact(t, dir, "aborted", &self)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "" {
		t.Fatalf("a live daemon's OWN finished transaction must not block; got foreign=%q", foreign)
	}
}

// TestForeignTransaction_MissingHomeIsNotAFinishedTransaction: an unmounted or absent
// home makes the journal Lstat report ErrNotExist for a reason that says nothing about
// the transaction. Declaring it finished there would authorize overwriting a binary on
// the strength of a filesystem being unavailable.
func TestForeignTransaction_MissingHomeIsNotAFinishedTransaction(t *testing.T) {
	dir := t.TempDir()
	owner := deadOwner(t, "gone", filepath.Join(t.TempDir(), "not-mounted"))
	owner.Journalled = true
	executable := stageArtifact(t, dir, "gone", &owner)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "gone" {
		t.Fatalf("an unreachable home must not read as a finished transaction; got foreign=%q", foreign)
	}
}

// TestArtifactOwner_PIDOneIsValid: af runs as a container entrypoint, so pid 1 is a
// legitimate stager. Rejecting it made that home's sidecar unreadable, which means
// permanently blocking — the very outcome under repair.
func TestArtifactOwner_PIDOneIsValid(t *testing.T) {
	dir := t.TempDir()
	owner := deadOwner(t, "pid1", t.TempDir())
	owner.PID = 1
	executable := filepath.Join(dir, "af")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write executable: %v", err)
	}
	path := artifactOwnerPath(executable, "pid1")
	if err := writeArtifactOwner(path, owner); err != nil {
		t.Fatalf("write owner: %v", err)
	}
	if _, err := readArtifactOwner(path, "pid1"); err != nil {
		t.Fatalf("pid 1 is a legitimate container entrypoint owner: %v", err)
	}
}

// TestForeignTransaction_ADifferentActiveTransactionDoesNotRevive: a home that
// finished transaction A and later started B over another executable must not present
// B's active.json as proof that A is live. Presence alone would have blocked A's
// executable for every other home, permanently.
func TestForeignTransaction_ADifferentActiveTransactionDoesNotRevive(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	active := activeJournalPath(home)
	if err := os.MkdirAll(filepath.Dir(active), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(active, []byte(`{"id":"transaction-b"}`), 0o600); err != nil {
		t.Fatalf("write active journal: %v", err)
	}
	owner := deadOwner(t, "transaction-a", home)
	owner.Journalled = true
	executable := stageArtifact(t, dir, "transaction-a", &owner)

	foreign, err := foreignTransactionOver(executable, "mine")
	if err != nil {
		t.Fatalf("foreignTransactionOver: %v", err)
	}
	if foreign != "" {
		t.Fatalf("another transaction's journal must not revive a finished one; got foreign=%q", foreign)
	}
}
