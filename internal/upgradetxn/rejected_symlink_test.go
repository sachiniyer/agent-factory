package upgradetxn

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A daemon started through a SYMLINK gets that link back from os.Executable,
// while Prepare canonicalizes before storing the path in the journal — so the
// supervisor records the rejection beside the resolved target. If the reader
// looked beside the link it would find nothing and reactivate the exact
// candidate a rollback had just rejected (#2212 review).
//
// The two paths must therefore resolve to ONE ledger.
func TestRejectedLedger_SymlinkAndTargetShareOneLedger(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("af binary"), 0o755))
	link := filepath.Join(dir, "af-link")
	require.NoError(t, os.Symlink(target, link))

	candidate := []byte("bad candidate bytes")
	require.NoError(t, RecordRejectedCandidate(target, digest(candidate), "1.0.300", "rolled back"))

	// The reader arrives holding the SYMLINK, which is what os.Executable hands a
	// daemon launched through one.
	rejected, entry, err := CandidateRejected(link, candidate)
	require.NoError(t, err)
	require.True(t, rejected,
		"a rejection recorded against the resolved executable must be visible to a caller holding the symlink, or the rolled-back candidate is reactivated")
	require.Equal(t, digest(candidate), entry.SHA256)

	// And the reverse, so neither side is privileged.
	require.NoError(t, RecordRejectedCandidate(link, digest([]byte("other")), "1.0.301", "rolled back"))
	rejectedViaTarget, _, err := CandidateRejected(target, []byte("other"))
	require.NoError(t, err)
	require.True(t, rejectedViaTarget)

	// Exactly one ledger file exists, not one per spelling.
	require.Equal(t, rejectedLedgerPath(target), rejectedLedgerPath(link))
}
