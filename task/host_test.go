package task

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

func taskHostFixture(t *testing.T, hosts map[string]config.HostConfig) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	cfg := config.DefaultConfig()
	cfg.Hosts = hosts
	require.NoError(t, config.SaveConfig(cfg))
	return dir
}

func hostTask(id, dir, host string) Task {
	return Task{ID: id, CronExpr: "0 3 * * *", Prompt: "p", ProjectPath: dir, Program: "claude", Host: host}
}

// A task's host is a pick like any other: it must name a listed host, with the
// message every other surface gives (#5571 W6.3).
func TestAddTaskRefusesAnUnlistedHost(t *testing.T) {
	dir := taskHostFixture(t, map[string]config.HostConfig{"box1": {Host: "box1.example"}})

	_, err := AddTaskChecked(hostTask("a1", dir, "nope"), ActorCLI, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `host "nope" is not in the host list [box1]`)
	assert.Contains(t, err.Error(), "[hosts.nope]")

	_, err = AddTaskChecked(hostTask("a2", dir, "-oProxyCommand=x"), ActorCLI, nil)
	require.Error(t, err, "a malformed name never reaches the list lookup")

	created, err := AddTaskChecked(hostTask("a3", dir, "  box1 "), ActorCLI, nil)
	require.NoError(t, err)
	assert.Equal(t, "box1", created.Host)
}

// A task with no host serializes exactly as before the field existed.
func TestAddTaskWithoutHostStoresNoKey(t *testing.T) {
	dir := taskHostFixture(t, nil)
	_, err := AddTaskChecked(hostTask("b1", dir, ""), ActorCLI, nil)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "tasks.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"host"`)
}

func TestUpdateTaskHostPatch(t *testing.T) {
	dir := taskHostFixture(t, map[string]config.HostConfig{"box1": {Host: "box1.example"}})
	_, err := AddTaskChecked(hostTask("c1", dir, "box1"), ActorCLI, nil)
	require.NoError(t, err)

	bad := "ghost"
	_, err = UpdateTaskChecked("c1", TaskUpdate{Host: &bad}, ProjectExpectation{}, ActorCLI, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `host "ghost" is not in the host list`)

	// Clearing is always allowed.
	empty := ""
	got, err := UpdateTaskChecked("c1", TaskUpdate{Host: &empty}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Host)

	// A host removed from the list since must not wedge an unrelated edit.
	good := "box1"
	_, err = UpdateTaskChecked("c1", TaskUpdate{Host: &good}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	require.NoError(t, config.SaveConfig(cfg))
	prompt := "new prompt"
	got, err = UpdateTaskChecked("c1", TaskUpdate{Prompt: &prompt}, ProjectExpectation{}, ActorCLI, nil)
	require.NoError(t, err)
	assert.Equal(t, "box1", got.Host)
}

func TestDiffTaskCarriesHost(t *testing.T) {
	old := Task{ID: "x"}
	cur := Task{ID: "x", Host: "box1"}
	u := DiffTask(old, cur)
	require.NotNil(t, u.Host)
	assert.Equal(t, "box1", *u.Host)
	assert.False(t, u.IsEmpty())
	assert.Equal(t, cur.Host, u.apply(old).Host)
	assert.Nil(t, DiffTask(cur, cur).Host)
}
