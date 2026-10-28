package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadTasksMigratesThroughASymlinkWithoutReplacingIt is the #3718 test.
//
// #3672 put tasks.json on the FOLLOW side: a user who keeps their task store in
// dotfiles gets the target rewritten and the link preserved. That answer was not
// total, because the store has a second writer nobody routed through it — the
// legacy-v0 write-back in config.LoadAndMigrateSchemaFile, which went out through
// the plain replacing writer. So the one moment a linked store is rewritten
// without the user asking, the load that migrates it, was the one moment that
// destroyed the link and stranded the dotfiles copy on the pre-migration content
// (the #3660 shape, in the store #3672 had just promised would follow).
//
// The load path is where it matters most: writeTasks runs because the user typed
// `af task add`, but this fires on a plain `af` start, so the user has no reason
// to look at their dotfiles afterwards.
func TestLoadTasksMigratesThroughASymlinkWithoutReplacingIt(t *testing.T) {
	afHome := t.TempDir()
	dotfiles := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	target := filepath.Join(dotfiles, "tasks.json")
	legacy := []byte(`[
  {"id":"legacy","name":"Old Task","prompt":"do it","cron_expr":"0 9 * * *","project_path":"/tmp","enabled":true,"created_at":"2025-01-01T00:00:00Z"}
]`)
	require.NoError(t, os.WriteFile(target, legacy, 0644))

	link := filepath.Join(afHome, tasksFileName)
	require.NoError(t, os.Symlink(target, link))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return link, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	tasks, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "legacy", tasks[0].ID)

	// The link survives, still naming the same file. Lstat is the assertion:
	// os.Stat would follow the link and report the target either way, which is
	// exactly the thing that made the old behaviour invisible.
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink,
		"the migration write-back replaced the symlink at %s with a regular file", link)
	readBack, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, readBack)

	// …and the migrated bytes landed in the user's file, not beside the link.
	migrated, err := os.ReadFile(target)
	require.NoError(t, err)
	var envelope struct {
		SchemaVersion int               `json:"schema_version"`
		Tasks         []json.RawMessage `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(migrated, &envelope))
	assert.Equal(t, TasksSchemaVersion, envelope.SchemaVersion)
	require.Len(t, envelope.Tasks, 1)
	assert.JSONEq(t, `{"id":"legacy","name":"Old Task","prompt":"do it","cron_expr":"0 9 * * *","project_path":"/tmp","enabled":true,"created_at":"2025-01-01T00:00:00Z"}`,
		string(envelope.Tasks[0]))

	// The pre-migration BACKUP stays beside the link, in af's own home. Following
	// the link is about the file af was asked to rewrite; the backup is af's
	// scratch copy, and dropping it into somebody's dotfiles working tree would
	// leave an untracked file in a repository af does not own.
	backup, err := os.ReadFile(link + ".bak.schema-v0")
	require.NoError(t, err)
	assert.Equal(t, legacy, backup)

	entries, err := os.ReadDir(dotfiles)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{"tasks.json"}, names,
		"the migration must leave nothing but the rewritten store in the user's directory")
}

// TestLoadTasksMigrationPreservesTheLinkTargetsMode pins the other half of
// following: the file af rewrites is the user's, so its mode is theirs too.
//
// Plans pass Perm 0644 because that is the mode for a tasks.json af itself
// created. A store the user keeps at 0600 in their dotfiles made a deliberate
// choice about a file af is only rewriting, and a migration must not quietly
// widen it on the one write the user did not ask for.
func TestLoadTasksMigrationPreservesTheLinkTargetsMode(t *testing.T) {
	afHome := t.TempDir()
	dotfiles := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	target := filepath.Join(dotfiles, "tasks.json")
	require.NoError(t, os.WriteFile(target, []byte(`[]`), 0600))

	link := filepath.Join(afHome, tasksFileName)
	require.NoError(t, os.Symlink(target, link))

	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return link, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	_, err := LoadTasks()
	require.NoError(t, err)

	// Assert the rewrite HAPPENED before asserting its mode. A write-back that
	// replaced the link would leave this target untouched at 0600 and pass a
	// bare mode check while doing the exact thing #3718 is about.
	migrated, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(migrated), `"schema_version"`,
		"the migration must have rewritten the link's target, not replaced the link")

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}
