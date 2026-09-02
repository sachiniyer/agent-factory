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
