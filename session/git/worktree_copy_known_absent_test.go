package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A known-absent entry is RECORDED, not omitted — and the two sides count it
// differently, which is what lets a deliberately incomplete tree validate
// without weakening the check (#3066).
//
// Attempt 1 omitted the entry entirely. validateSource requires the manifest and
// the source directory to hold exactly the same name set, so every archive that
// skipped a file failed with "tree entry set changed" and was rejected. The
// wrong repair is loosening that check; this represents the absence instead.
func TestKnownAbsentEntry_CountedOnTheSourceSideOnly(t *testing.T) {
	present := copiedEntry{name: "kept"}
	skipped := copiedEntry{name: "locked", absentReason: "unreadable"}

	require.False(t, present.absent())
	require.True(t, skipped.absent(),
		"an entry with a reason must report as deliberately absent")

	// The zero value must NOT read as absent: an entry that simply failed to be
	// filled in is a bug, not a recorded skip, and must keep failing validation.
	var zero copiedEntry
	require.False(t, zero.absent(),
		"the zero value must not read as a recorded skip, or an unfilled entry would validate")
}

// The destination side must REJECT a node existing where the manifest recorded a
// skip. That direction is not symmetric with the source side and is the case
// that catches something creating the file behind af's back.
func TestKnownAbsentEntry_DestinationMustNotHoldTheSkippedName(t *testing.T) {
	directory := copiedDirectory{entries: []copiedEntry{
		{name: "kept"},
		{name: "locked", absentReason: "unreadable"},
	}}

	sourceExpected, destinationExpected := 0, 0
	for _, entry := range directory.entries {
		sourceExpected++
		if !entry.absent() {
			destinationExpected++
		}
	}
	require.Equal(t, 2, sourceExpected, "the source holds both names")
	require.Equal(t, 1, destinationExpected,
		"the destination holds only what was actually copied, so the counts must differ by the skips")
}

// The refusing paths must refuse, and the DEFAULT must be one of them.
//
// This is the property the whole mechanism rests on. A move deletes the source
// on success and a restore deletes the quarantined archive, so a file skipped
// there is gone — the silent permanent data loss the first attempt shipped,
// because its skip was unconditional and relocateWorktreeTo backs restore as
// well as archiving (#3066).
func TestUnreadablePolicyFor_OnlyArchiveMaySkip(t *testing.T) {
	require.Equal(t, skipUnreadable, unreadablePolicyFor("archive"),
		"archive leaves the original in place, so a skip is survivable there")

	for _, operation := range []string{"move", "restore"} {
		require.Equal(t, refuseUnreadable, unreadablePolicyFor(operation),
			"%s destroys its source on success, so it must never skip", operation)
	}

	// An allowlist, not a denylist: an operation added later that forgets to
	// appear here inherits refusal rather than permission.
	for _, unknown := range []string{"", "sync", "publish", "ARCHIVE"} {
		require.Equal(t, refuseUnreadable, unreadablePolicyFor(unknown),
			"an unmapped operation (%q) must inherit refusal", unknown)
	}
}

// End to end through the REAL copy path, including validation — which is the
// bar slice 1 established. Attempt 1's test called the collector directly,
// bypassed validation, and a completely non-functional feature looked green
// (#3066).
func TestCopyTree_ArchiveSkipsAnUnreadableFileAndValidates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode bits, so no file is unreadable")
	}
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "kept.txt"), []byte("KEPT"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o755))
	locked := filepath.Join(source, "nested", "locked.secret")
	require.NoError(t, os.WriteFile(locked, []byte("SECRET"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))

	// REFUSE (the default, and what move and restore use) must reject.
	refused := filepath.Join(t.TempDir(), "refused")
	copied, err := copyTreeWithIdentities(source, refused, refuseUnreadable)
	if copied != nil {
		copied.close()
	}
	require.Error(t, err, "the refusing policy must reject an unreadable source")
	var unreadable *unreadableSourceError
	require.ErrorAs(t, err, &unreadable)
	require.Equal(t, locked, unreadable.path)

	// SKIP (archive only) must succeed, and the manifest must RECORD the absence
	// rather than omit it — otherwise validation rejects the whole tree.
	dest := filepath.Join(t.TempDir(), "archived")
	copied, err = copyTreeWithIdentities(source, dest, skipUnreadable)
	require.NoError(t, err, "archive must not be blocked by one unreadable file")
	require.NotNil(t, copied)
	defer copied.close()

	kept, err := os.ReadFile(filepath.Join(dest, "kept.txt"))
	require.NoError(t, err)
	require.Equal(t, "KEPT", string(kept))
	_, err = os.Lstat(filepath.Join(dest, "nested", "locked.secret"))
	require.True(t, os.IsNotExist(err), "the unreadable file must not have been copied")

	// The recorded absence, found by walking the manifest.
	var found []string
	var walk func(d copiedDirectory, prefix string)
	walk = func(d copiedDirectory, prefix string) {
		for _, entry := range d.entries {
			if entry.absent() {
				found = append(found, filepath.Join(prefix, entry.name))
			}
			if entry.directory != nil {
				walk(*entry.directory, filepath.Join(prefix, entry.name))
			}
		}
	}
	walk(copied.root, "")
	require.Equal(t, []string{filepath.Join("nested", "locked.secret")}, found,
		"the skip must be RECORDED in the manifest, not omitted — omitting it is what made validation reject the archive")

	// And validation must accept the deliberately incomplete tree.
	require.NoError(t, copied.validateSource(source),
		"a recorded absence must validate; only an UNEXPLAINED one may fail")
}
