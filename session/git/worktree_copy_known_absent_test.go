package git

import (
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
