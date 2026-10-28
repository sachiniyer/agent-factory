package git

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

	gw := &GitWorktree{archiveReport: ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, []ArchiveSkippedEntry{
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
	require.NoError(t, os.Rename(path, path+"-original"))
	require.NoError(t, os.Mkdir(path, 0o700))

	gw := &GitWorktree{archiveReport: ArchiveReport{RetainedTrees: []ArchiveRetainedTree{
		newArchiveRetainedTree(path, identity, []ArchiveSkippedEntry{
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
