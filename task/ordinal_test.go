package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Task.Ordinal is the row a record occupies in tasks.json, and it is what
// ApplyLiveArming pairs an observation to (#3680). These pin the four things
// that has to be true of it: every read numbers the rows, the numbering starts
// at one so the zero value can mean "no row", the file cannot supply one, and
// nothing that rewrites the store moves a row out from under its number.

func ordinalRow(id, name, cron, projectPath string) Task {
	return Task{
		ID: id, Name: name, Prompt: "p", CronExpr: cron,
		ProjectPath: projectPath, Program: "claude", Enabled: true, CreatedAt: time.Now(),
	}
}

// rowsByID is the assertion this file is built around: which row each task
// occupies, keyed by something that survives a rewrite.
func rowsByID(tasks []Task) map[string]int {
	rows := make(map[string]int, len(tasks))
	for _, t := range tasks {
		rows[t.ID] = t.Ordinal
	}
	return rows
}

func TestLoadTasksNumbersRowsFromOne(t *testing.T) {
	// From ONE, not from zero. The zero value has to stay available to mean "this
	// record occupies no row" — a record built in memory, or one decoded from an
	// older daemon's response that carries no ordinal field at all. Numbered from
	// zero, every such record would claim row 0 and collect its observation.
	setupTestTasks(t, []Task{
		ordinalRow("aaa00001", "first", "0 9 * * *", "/repo"),
		ordinalRow("bbb00002", "second", "0 10 * * *", "/repo"),
		ordinalRow("ccc00003", "third", "0 11 * * *", "/repo"),
	})

	tasks, err := LoadTasks()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"aaa00001": 1, "bbb00002": 2, "ccc00003": 3}, rowsByID(tasks))
}

func TestLoadTasksOverwritesAnOrdinalFoundInTheFile(t *testing.T) {
	// The number is computed BY the read, which is the whole reason it is an
	// ordinal rather than a stamped per-row id: tasks.json is hand-editable, so
	// any value the file supplies is exactly as duplicable as ID already is. Two
	// rows here claim to be row 1 and the read must believe neither.
	dir := t.TempDir()
	path := filepath.Join(dir, tasksFileName)
	require.NoError(t, os.WriteFile(path, []byte(`[
  {"id":"aaa00001","name":"first","prompt":"p","cron_expr":"0 9 * * *","project_path":"/repo","enabled":true,"created_at":"2026-01-01T00:00:00Z","ordinal":1},
  {"id":"bbb00002","name":"second","prompt":"p","cron_expr":"0 10 * * *","project_path":"/repo","enabled":true,"created_at":"2026-01-01T00:00:00Z","ordinal":1}
]`), 0644))
	origGetPath := getTasksPathFn
	getTasksPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { getTasksPathFn = origGetPath })

	tasks, err := LoadTasks()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"aaa00001": 1, "bbb00002": 2}, rowsByID(tasks),
		"a row number the file supplied is overwritten, never believed")
}

func TestOrdinalNeverReachesDisk(t *testing.T) {
	// Derived, never persisted — the rule the health fields already follow, and
	// one the row number needs even more than they do: a stored ordinal goes stale
	// the moment a row is inserted above it, leaving a record asserting an
	// identity that is no longer its own. UpdateTaskStatus is a real
	// load-modify-save, which is the path that would otherwise write one back.
	path := setupTestTasks(t, []Task{
		ordinalRow("aaa00001", "first", "0 9 * * *", "/repo"),
		ordinalRow("bbb00002", "second", "0 10 * * *", "/repo"),
	})

	ran := time.Now()
	require.NoError(t, UpdateTaskStatus("bbb00002", &ran, "started"))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var envelope struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Len(t, envelope.Tasks, 2)
	for i, stored := range envelope.Tasks {
		_, present := stored["ordinal"]
		assert.False(t, present, "row %d was written to disk carrying its row number", i+1)
	}

	// And the read still hands them back, so the strip did not cost the reader
	// anything.
	tasks, err := LoadTasks()
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"aaa00001": 1, "bbb00002": 2}, rowsByID(tasks))
}

func TestRepoFilteringKeepsEachSurvivorsRowNumber(t *testing.T) {
	// The property the whole design rests on. The rail loads ONE repo's rows while
	// the daemon answers about every repo, so the two lists have different lengths
	// and different indexes — and pairing them by index would be nonsense. The
	// number travels ON the record, so a filtered list still says exactly which
	// rows of the store it is holding.
	repoA := mkScopeRepo(t, "project-a")
	repoB := mkScopeRepo(t, "project-b")
	setupTestTasks(t, []Task{
		ordinalRow("aaa00001", "a-first", "0 9 * * *", repoA),
		ordinalRow("bbb00002", "b-only", "0 10 * * *", repoB),
		ordinalRow("aaa00003", "a-second", "0 11 * * *", repoA),
	})

	gotA, err := LoadTasksForRepo(repoA)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"aaa00001": 1, "aaa00003": 3}, rowsByID(gotA),
		"repo A's rows keep rows 1 and 3 even though they are now items 0 and 1")

	gotB, err := LoadTasksForRepo(repoB)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"bbb00002": 2}, rowsByID(gotB),
		"and repo B's only row is row 2 of the store, not row 1 of its own list")
}

func TestStableRepoBindingUpdatesDoNotMoveRows(t *testing.T) {
	// The binding-stabilisation path is the one rewrite that touches every legacy
	// record in the store, and it commits to disk. It does not itself feed
	// ApplyLiveArming — the observation comes from controlServer.ListTasks, which
	// loads through plain LoadTasks — but it can commit BETWEEN the rail's disk
	// read and the daemon's, and a rewrite that moved rows would leave the two
	// reads pairing records that are not the same task.
	//
	// It mutates in place by index and saves the same slice, so no row moves. That
	// is a property of the code rather than a promise, and this is what holds it
	// to it.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := mkScopeRepo(t, "project")
	// Ids and names deliberately out of alphabetical order, so a rewrite that
	// sorted by either would be visible rather than accidentally identity.
	setupTestTasks(t, []Task{
		ordinalRow("ccc00001", "zeta", "0 9 * * *", repo),
		ordinalRow("aaa00002", "yankee", "0 10 * * *", repo),
		ordinalRow("bbb00003", "xray", "0 11 * * *", repo),
	})

	before, err := LoadTasks()
	require.NoError(t, err)
	require.Len(t, before, 3)
	for _, tk := range before {
		require.Empty(t, tk.RepoID, "precondition: every row starts unbound, so the backfill has work to do")
	}
	wantRows := map[string]int{"ccc00001": 1, "aaa00002": 2, "bbb00003": 3}
	require.Equal(t, wantRows, rowsByID(before))

	authoritative, updated, err := LoadTasksWithStableRepoBindingUpdates()
	require.NoError(t, err)
	require.Len(t, updated, 3, "precondition: the backfill actually rewrote all three rows")

	// The list the rewrite returns is numbered. A future caller handed this slice
	// must not be handed three rows that all report having no identity — that
	// would read to ApplyLiveArming as "nothing to pair on" and silently blank
	// every answer.
	for _, tk := range authoritative {
		require.NotEmpty(t, tk.RepoID, "precondition: these are the records the backfill bound")
	}
	assert.Equal(t, wantRows, rowsByID(authoritative))
	assert.Equal(t, wantRows, rowsByID(updated),
		"and so are the records it publishes as EventTaskUpdated")

	// And the file it wrote reads back with every row exactly where it was.
	after, err := LoadTasks()
	require.NoError(t, err)
	assert.Equal(t, wantRows, rowsByID(after),
		"the backfill rewrote every row's repo_id and moved none of them")
}
