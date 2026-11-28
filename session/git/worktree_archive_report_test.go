package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func testRetainedTree(path string, inode uint64, entries ...ArchiveSkippedEntry) ArchiveRetainedTree {
	return ArchiveRetainedTree{
		Path: path, IdentityKnown: true, Device: 1, Inode: inode, FileType: 0o040000,
		Skipped: entries,
	}
}

func TestArchiveReportJSONPreservesInvalidUTF8PathBytes(t *testing.T) {
	rawRelative := "private/credential-\xff"
	rawRoot := "/worktrees/.af-source-0123456789abcdef0123456789abc\xff"
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{{
		Path: rawRoot, IdentityKnown: true,
		Device: 1, Inode: 2, FileType: 0o040000, Skipped: []ArchiveSkippedEntry{{
			Path: rawRelative, Reason: ArchiveSkipPermissionDenied,
		}},
	}}}

	payload, err := json.Marshal(report)
	require.NoError(t, err)
	var restored ArchiveReport
	require.NoError(t, json.Unmarshal(payload, &restored))
	require.Len(t, restored.RetainedTrees, 1)
	require.Len(t, restored.RetainedTrees[0].Skipped, 1)
	assert.Equal(t, rawRoot, restored.RetainedTrees[0].filesystemPath())
	assert.Equal(t, rawRelative, restored.RetainedTrees[0].Skipped[0].FilesystemPath())
	assert.NotEmpty(t, restored.RetainedTrees[0].Skipped[0].PathBytes,
		"invalid filename bytes need a lossless field beside the JSON display string")
}

func TestArchiveReportWarningBoundsSkippedPaths(t *testing.T) {
	entries := make([]ArchiveSkippedEntry, 100)
	for index := range entries {
		entries[index] = newArchiveSkippedEntry(
			fmt.Sprintf("generated/private-%03d", index), ArchiveSkipPermissionDenied,
		)
	}
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		testRetainedTree("/worktrees/.af-source-0123456789abcdef0123456789abcdef", 2, entries...),
	}}

	warning := report.Warning("archive")
	assert.Contains(t, warning, "skipped 100 unreadable files")
	assert.Contains(t, warning, "showing first 20 of 100")
	assert.Contains(t, warning, "private-019")
	assert.NotContains(t, warning, "private-020")
	assert.Less(t, len(warning), 5000, "wire warnings must stay bounded; the session JSON owns the full report")
}

// TestArchiveReportWarningKeepsUnknownReasonOnOneField pins the one field in
// this warning that is NOT a number and NOT %q-quoted. A stored reason is
// rendered with %s, and it is a decoded string rather than a compile-time
// constant: an unknown value from a newer or corrupt record reaches the format
// verbatim. The warning is a single-line diagnostic — one log line, one TUI row,
// one error string — so a newline in that value splits it, and a paren or a
// quote closes its field early.
//
// The bug-report redactor reads this format to take the user file names back out
// of a bundled log (#3553), and each of those characters breaks that read in a
// way that leaves a name behind: a newline puts the entries after it on a line
// with no anchor, and a quote desynchronizes the walk over %q tokens. Both are
// the emitter's to prevent — a single-line row cannot be reassembled downstream.
func TestArchiveReportWarningKeepsUnknownReasonOnOneField(t *testing.T) {
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{testRetainedTree(
		"/worktrees/.af-source-0", 2,
		ArchiveSkippedEntry{Path: "first.txt", Reason: ArchiveSkipReason("read failed\n  (detail) \"quoted\"")},
		ArchiveSkippedEntry{Path: "private/second.txt", Reason: ArchiveSkipPermissionDenied},
	)}}

	warning := report.Warning("restore")

	assert.NotContains(t, warning, "\n", "an embedded newline splits this single-line diagnostic across log lines and TUI rows")
	assert.NotContains(t, warning, `"read`, "a quote in a reason opens a field that is not a path")
	// The reason still says something: an unknown value is passed through so a
	// report written by another binary is not rendered as a blank.
	assert.Contains(t, warning, "read failed")
	// Both entries stay on the one line, each still its own "<path>" (<reason>).
	assert.Contains(t, warning, `"first.txt" (read failed`)
	assert.Contains(t, warning, `"private/second.txt" (permission denied)`)
	// The known reason is a constant and is untouched by the sanitizing.
	assert.Equal(t, "permission denied", skipReasonText(ArchiveSkipPermissionDenied))
}

func TestArchiveWorktreePreservesPriorOmissions(t *testing.T) {
	gw, _, source := archiveTestWorktree(t)
	prior := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		testRetainedTree("/worktrees/.af-source-0123456789abcdef0123456789abcdef", 9,
			newArchiveSkippedEntry("private/old", ArchiveSkipPermissionDenied)),
	}}
	gw.RestoreArchiveReport(prior)
	destination := filepath.Join(testguard.CanonicalTempDir(t), "archived", "repoid", "arch")

	require.NoError(t, gw.ArchiveWorktree(destination))
	assert.NoDirExists(t, source)
	assert.Equal(t, prior, gw.GetArchiveReport(),
		"a later complete copy cannot prove that bytes omitted by an earlier archive were recovered")
}

func TestCleanupRetainedArchiveTreesUsesPersistedIdentity(t *testing.T) {
	parent := t.TempDir()
	name := ".af-source-0123456789abcdef0123456789abcdef"
	path := filepath.Join(parent, name)
	require.NoError(t, os.Mkdir(path, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "credential"), []byte("secret"), 0o000))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	pin, err := inspectRelocationPathPin(path)
	require.NoError(t, err)

	gw := &GitWorktree{archiveReport: ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, pin, []ArchiveSkippedEntry{
			newArchiveSkippedEntry("credential", ArchiveSkipPermissionDenied),
		}),
	}}}
	require.NoError(t, gw.cleanupRetainedArchiveTrees())
	assert.NoDirExists(t, path)
	assert.True(t, gw.GetArchiveReport().Empty(), "a deleted retained tree must retire its durable handle")
}

func TestCleanupRetainedArchiveTreesRefusesReplacement(t *testing.T) {
	parent := t.TempDir()
	name := ".af-source-fedcba9876543210fedcba9876543210"
	path := filepath.Join(parent, name)
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	pin, err := inspectRelocationPathPin(path)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+"-original"))
	require.NoError(t, os.Mkdir(path, 0o700))

	gw := &GitWorktree{archiveReport: ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, pin, []ArchiveSkippedEntry{
			newArchiveSkippedEntry("credential", ArchiveSkipPermissionDenied),
		}),
	}}}
	err = gw.cleanupRetainedArchiveTrees()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different data")
	assert.DirExists(t, path, "a raced-in replacement is not owned by the report")
	assert.DirExists(t, path+"-original", "the original retained tree remains recoverable under its raced name")
	assert.False(t, gw.GetArchiveReport().Empty(), "cleanup refusal must keep the only durable ownership handle")
}

// TestArchiveReport_RetainedTreeAtPath pins #7816 R1's durable-ownership
// question: a `.af-source-*` tree the loaded report already retains — the same
// recorded path AND the same on-disk identity the durable entry pinned — is
// durably owned, so the dest-present arbitration neither blocks on it nor
// drops it. A matching pathname carrying different data, or an entry whose
// identity was never recorded, is NOT the report's tree.
func TestArchiveReport_RetainedTreeAtPath(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	pin, err := inspectRelocationPathPin(path)
	require.NoError(t, err)
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, pin, nil),
	}}

	tracked, err := report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.True(t, tracked, "the entry names the same path and the identity still matches the tree on disk")

	tracked, err = report.RetainedTreeAtPath(filepath.Join(parent, ".af-source-fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)
	assert.False(t, tracked, "a residue at a path the report does not name is not the report's tree")

	// A different tree raced in under the tracked pathname is not owned: the
	// durable identity names different data than what sits there now.
	require.NoError(t, os.Rename(path, path+"-original"))
	require.NoError(t, os.Mkdir(path, 0o700))
	tracked, err = report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.False(t, tracked, "a replacement at the tracked pathname is different data, not the report's tree")
}

// TestArchiveReport_RetainedTreeAtPath_DeleteRecreateReadsUntracked pins the
// #7900 property "tracked proves the same tree, not a reused (dev, ino)": a
// record captured with a creation pin must NOT read a delete-and-recreate at
// the tracked path as its tree. The pin is the only evidence the data
// changed when the filesystem hands the replacement the recycled inode slot
// — so the test simulates that slot by answering the recorded dev+ino+type
// for the new tree (exactly what a recycled slot makes the kernel answer,
// as the pin-less test does), leaving the pin to do the separating.
//
// What the pin can prove is bounded by the birth time's granularity: it is
// a per-tick stamp, so the test recreates until a probe reads a LATER tick
// than the recorded pin rather than assuming one recreation always lands on
// a different stamp. On kernels before the 6.13 multigrain floor — CI's
// ubuntu-24.04 runners included — the tick can be as coarse as a scheduler
// interval, hence the loop: a recreate in a later tick, not never.
func TestArchiveReport_RetainedTreeAtPath_DeleteRecreateReadsUntracked(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	pin, err := inspectRelocationPathPin(path)
	require.NoError(t, err)
	if pin == 0 {
		t.Skip("this filesystem answers no creation pin, so a record cannot capture one")
	}
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, pin, nil),
	}}

	// rm plus recreate at the same path in a later tick: keep replacing the
	// tree until the pin probe reads a later stamp than the record's, so the
	// comparison is proven on different-tick data whatever the granularity.
	var newPin int64
	for attempt := 0; attempt < 200; attempt++ {
		require.NoError(t, os.RemoveAll(path))
		require.NoError(t, os.Mkdir(path, 0o700))
		newPin, err = inspectRelocationPathPin(path)
		require.NoError(t, err)
		if newPin > pin {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Greater(t, newPin, pin,
		"the filesystem never stamped a later tick across 200 recreations — the pin cannot be exercised here")
	require.NoError(t, os.WriteFile(filepath.Join(path, "replacement"), []byte("different data"), 0o600))

	// The replacement holds the recycled inode slot — simulated by the
	// identity probe answering the recorded identity for the new tree.
	previousIdentity := relocationPathIdentity
	relocationPathIdentity = func(string) (pathIdentity, error) { return identity, nil }
	t.Cleanup(func() { relocationPathIdentity = previousIdentity })

	tracked, err := report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.False(t, tracked,
		"a delete-and-recreate at the tracked path in a later tick is different data; the creation pin proves it even with the recycled inode slot")
}

// TestArchiveReport_RetainedTreeAtPath_PinProbeMissReadsUntracked pins the
// fail-closed half of the pin contract: a record carrying a creation pin can
// only be matched by a probe that answers one. A filesystem that yields no
// pin, or a probe that fails, never satisfies a recorded pin — a missing
// value fails closed rather than proving sameness from the recyclable half
// of the identity.
func TestArchiveReport_RetainedTreeAtPath_PinProbeMissReadsUntracked(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	// The record was written where a pin was captured; the filesystem then
	// stopped answering one (or the pin-bearing build rolled back to a
	// pin-less one).
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, 424242, nil),
	}}

	previousPin := relocationPathPin
	t.Cleanup(func() { relocationPathPin = previousPin })
	relocationPathPin = func(string) (int64, error) { return 0, nil }
	tracked, err := report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.False(t, tracked,
		"a pin-less probe cannot satisfy a recorded pin, so the verdict fails closed to untracked")

	relocationPathPin = func(string) (int64, error) {
		return 0, errors.New("pin probe failed")
	}
	tracked, err = report.RetainedTreeAtPath(path)
	require.Error(t, err,
		"a failed pin probe must surface, not silently satisfy or deny the recorded pin")
	assert.False(t, tracked)
	assert.Contains(t, err.Error(), "cannot compare the retained archive tree entry")
}

// TestArchiveReport_RetainedTreeAtPath_LegacyRecordReadsTracked pins the
// compat decision #7900 asked for: a record written without a creation pin —
// every record from before the pin existed, or one captured on a filesystem
// that offers none — reads exactly as it always did, tracked by
// dev+ino+type. Reading those records as untracked instead would manufacture
// a refusal on every tree a previous release durably retained. The pin
// probe is never even consulted: 0 is not a pin.
func TestArchiveReport_RetainedTreeAtPath_LegacyRecordReadsTracked(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, 0, nil),
	}}

	previousPin := relocationPathPin
	pinProbed := false
	relocationPathPin = func(string) (int64, error) {
		pinProbed = true
		return 0, nil
	}
	t.Cleanup(func() { relocationPathPin = previousPin })

	tracked, err := report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.True(t, tracked, "a pre-pin record reads tracked by dev+ino+type, exactly as on master")
	assert.False(t, pinProbed, "a record without a pin never consults the pin probe")
}

// TestArchiveReport_RetainedTreeAtPath_PinlessRecordReadsRecycledInodeTracked
// documents the accepted gap a pin-less platform keeps: where the probe
// answers 0, the record proves only what dev+ino+type prove, so "rm plus
// recreate at the same path reads as tracked" wherever the filesystem lets
// the replacement keep the inode slot. The gap is accepted there and
// documented; on linux/darwin the pin closes it.
func TestArchiveReport_RetainedTreeAtPath_PinlessRecordReadsRecycledInodeTracked(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, ".af-source-0123456789abcdef0123456789abcdef")
	require.NoError(t, os.Mkdir(path, 0o700))
	identity, err := inspectRelocationPathIdentity(path)
	require.NoError(t, err)
	// A record written where no pin was available.
	report := ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, 0, nil),
	}}

	// The tree is deleted and recreated at the same path, and the
	// filesystem hands the replacement the recycled inode slot — simulated
	// by the identity probe answering the recorded identity for the new
	// tree, which is exactly what a recycled slot makes the kernel answer.
	require.NoError(t, os.RemoveAll(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	previousIdentity := relocationPathIdentity
	relocationPathIdentity = func(string) (pathIdentity, error) { return identity, nil }
	t.Cleanup(func() { relocationPathIdentity = previousIdentity })

	tracked, err := report.RetainedTreeAtPath(path)
	require.NoError(t, err)
	assert.True(t, tracked,
		"with no pin to compare, dev+ino+type is all the record can prove — the documented accepted gap")
}
