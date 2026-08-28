package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadAllRepoInstancesReportingSkipDetailsNamesPathAndCause pins the
// contract the #3476 fix depends on: a repo whose instances.json cannot be READ
// must come back as a skip carrying the file path and the underlying error, not
// merely as an absence from the result map.
//
// The repoID alone is an opaque hash, so a guard that refuses because of this
// skip has nothing actionable to print without these two fields.
func TestLoadAllRepoInstancesReportingSkipDetailsNamesPathAndCause(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	require.NoError(t, SaveRepoInstances("readable", json.RawMessage("[]")))
	require.NoError(t, SaveRepoInstances("blocked", json.RawMessage("[]")))
	blocked, err := RepoInstancesPath("blocked")
	require.NoError(t, err)

	// chmod 0000 is the realistic shape, but modes inherit the ambient umask and
	// root ignores them, so apply it and then PROVE the file is unreadable —
	// falling back to a directory in its place, which os.ReadFile refuses for
	// everyone. A missing file would not do: the loader maps that to "[]".
	require.NoError(t, os.Chmod(blocked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
	if _, err := os.ReadFile(blocked); err == nil {
		require.NoError(t, os.Remove(blocked))
		require.NoError(t, os.Mkdir(blocked, 0o755))
	}
	_, readErr := os.ReadFile(blocked)
	require.Error(t, readErr, "fixture did not take: %s is still readable", blocked)
	require.False(t, os.IsNotExist(readErr), "fixture must produce a READ error, not a missing file")

	result, skips, err := LoadAllRepoInstancesReportingSkipDetails()
	require.NoError(t, err)
	require.Contains(t, result, "readable")
	require.NotContains(t, result, "blocked", "an unreadable repo must not appear as loaded data")

	require.Len(t, skips, 1)
	require.Equal(t, "blocked", skips[0].RepoID)
	require.Equal(t, blocked, skips[0].Path, "the skip must name the file to repair")
	require.Error(t, skips[0].Err, "the skip must carry the underlying I/O error")
	require.Contains(t, skips[0].String(), blocked)
	require.Contains(t, FormatRepoInstancesSkips(skips), blocked)

	// The narrowing forms stay consistent with the primitive.
	_, ids, err := LoadAllRepoInstancesReportingSkips()
	require.NoError(t, err)
	require.Equal(t, []string{"blocked"}, ids)

	plain, err := LoadAllRepoInstances()
	require.NoError(t, err)
	require.NotContains(t, plain, "blocked")
}

// TestLoadAllRepoInstancesReportingSkipsStaysNilWhenNothingIsSkipped keeps the
// narrowing wrapper from handing existing callers an empty-but-non-nil slice
// where they used to get nil.
func TestLoadAllRepoInstancesReportingSkipsStaysNilWhenNothingIsSkipped(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	require.NoError(t, SaveRepoInstances("readable", json.RawMessage("[]")))

	_, ids, err := LoadAllRepoInstancesReportingSkips()
	require.NoError(t, err)
	require.Nil(t, ids)
}

// TestRepoInstancesSkipRemedyDoesNotSuggestDeletingNewerState is a data-loss
// guard, not a wording preference.
//
// A file this binary is merely too OLD to parse lands in the same skip list as
// one it cannot read, and the blanket "repair or remove the file(s)" advice
// therefore told users to delete a NEWER af's session records — orphaning live
// sessions and worktrees — while the underlying error was telling them to
// upgrade. The remedy has to follow the cause.
func TestRepoInstancesSkipRemedyDoesNotSuggestDeletingNewerState(t *testing.T) {
	newer := []RepoInstancesSkip{{
		RepoID: "future-repo",
		Path:   "/af/instances/future-repo/instances.json",
		Err: &UnsupportedSchemaVersionError{
			StoreName: InstancesFileName, Path: "/af/instances/future-repo/instances.json",
			FileVersion: 99, SupportedVersion: InstancesSchemaVersion,
		},
	}}
	remedy := RepoInstancesSkipRemedy(newer)
	// Assert on the ADVICE, not the vocabulary: "do not delete" is the opposite
	// of recommending it, so match the destructive suggestion itself.
	if strings.Contains(remedy, "repair or remove") {
		t.Fatalf("must not advise removing a newer schema's records; got: %q", remedy)
	}
	if !strings.Contains(remedy, "do not delete") {
		t.Errorf("a newer-schema skip should say plainly that deleting loses sessions; got: %q", remedy)
	}
	if !strings.Contains(remedy, "upgrade") {
		t.Errorf("a newer-schema skip should point at the upgrade the error itself names; got: %q", remedy)
	}

	unreadable := []RepoInstancesSkip{{
		RepoID: "blocked", Path: "/af/instances/blocked/instances.json", Err: os.ErrPermission,
	}}
	if remedy := RepoInstancesSkipRemedy(unreadable); !strings.Contains(remedy, "repair") {
		t.Errorf("a plain read failure should still offer the repair advice; got: %q", remedy)
	}

	// Mixed: the destructive advice must not reappear just because one skip is
	// an ordinary read failure.
	mixed := append(append([]RepoInstancesSkip{}, newer...), unreadable...)
	if remedy := RepoInstancesSkipRemedy(mixed); strings.Contains(remedy, "repair or remove") {
		t.Errorf("a newer-schema skip anywhere in the set must suppress removal advice; got: %q", remedy)
	}
}
