package config

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadTUIRepoViewStateMissingFileDefaults(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	state, ok := LoadTUIRepoViewState("repo-a")

	assert.False(t, ok)
	assert.Empty(t, state.OpenPanes)
}

func TestSaveTUIRepoViewStateWritesSchemaAndPreservesRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	stateA := TUIRepoViewState{
		Selected: &TUIStateTarget{InstanceID: "id-a", Title: "alpha", TabName: "agent"},
		OpenPanes: []TUIStateOpenPane{{
			Key:        "title:alpha:tab:agent",
			InstanceID: "id-a",
			Title:      "alpha",
			TabName:    "agent",
			FocusRank:  1,
		}},
	}
	stateB := TUIRepoViewState{
		Selected: &TUIStateTarget{InstanceID: "id-b", Title: "beta", TabName: "shell"},
	}

	require.NoError(t, SaveTUIRepoViewState("repo-a", stateA))
	require.NoError(t, SaveTUIRepoViewState("repo-b", stateB))

	path, err := TUIStatePath()
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var file TUIStateFile
	require.NoError(t, json.Unmarshal(raw, &file))
	assert.Equal(t, TUIStateSchemaVersion, file.SchemaVersion)
	require.Contains(t, file.Repos, "repo-a")
	require.Contains(t, file.Repos, "repo-b")
	assert.Equal(t, "alpha", file.Repos["repo-a"].Selected.Title)
	assert.Equal(t, "beta", file.Repos["repo-b"].Selected.Title)
	assert.NotContains(t, string(raw), `"version"`)
}

func TestTUIRepoViewStateCorruptFileDefaultsAndRepairsOnSave(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	path, err := TUIStatePath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(`{not-json`), 0644))

	state, ok := LoadTUIRepoViewState("repo-a")
	assert.False(t, ok)
	assert.Empty(t, state.OpenPanes)

	want := TUIRepoViewState{
		Selected: &TUIStateTarget{InstanceID: "id-a", Title: "alpha", TabName: "agent"},
	}
	require.NoError(t, SaveTUIRepoViewState("repo-a", want))

	got, ok := LoadTUIRepoViewState("repo-a")
	require.True(t, ok)
	require.NotNil(t, got.Selected)
	assert.Equal(t, "alpha", got.Selected.Title)
}
