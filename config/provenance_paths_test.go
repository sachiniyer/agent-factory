package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebaseProjectPathsForDisplayUpdatesEverySourceReference(t *testing.T) {
	container := t.TempDir()
	realParent := filepath.Join(container, "real")
	displayParent := filepath.Join(container, "selected")
	realRoot := filepath.Join(realParent, "repo")
	require.NoError(t, os.MkdirAll(realRoot, 0755))
	require.NoError(t, os.Symlink(realParent, displayParent))
	displayRoot := filepath.Join(displayParent, "repo")

	realSource := filepath.Join(realRoot, InRepoConfigDirName, TomlConfigFileName)
	displaySource := filepath.Join(displayRoot, InRepoConfigDirName, TomlConfigFileName)
	globalSource := filepath.Join(container, "home", TomlConfigFileName)
	resolved := &ResolvedConfig{
		ProjectRoot: realRoot,
		Resolution: []ResolvedValue{{
			Winner: &SourceRef{Layer: SourceRepoShared.String(), Path: realSource},
			Origins: map[string]SourceRef{
				"codex": {Layer: SourceRepoShared.String(), Path: realSource},
				"aider": {Layer: SourceGlobal.String(), Path: globalSource},
			},
			Candidates: []CandidateTrace{
				{Layer: SourceGlobal.String(), Path: globalSource},
				{Layer: SourceRepoShared.String(), Path: realSource},
			},
		}},
	}

	require.NoError(t, resolved.RebaseProjectPathsForDisplay(displayRoot))
	assert.Equal(t, displayRoot, resolved.ProjectRoot)
	assert.Equal(t, displaySource, resolved.Resolution[0].Winner.Path)
	assert.Equal(t, displaySource, resolved.Resolution[0].Origins["codex"].Path)
	assert.Equal(t, globalSource, resolved.Resolution[0].Origins["aider"].Path)
	assert.Equal(t, globalSource, resolved.Resolution[0].Candidates[0].Path)
	assert.Equal(t, displaySource, resolved.Resolution[0].Candidates[1].Path)
}

func TestRebaseProjectPathsForDisplayRejectsDifferentIdentityWithoutMutation(t *testing.T) {
	projectRoot := t.TempDir()
	otherRoot := t.TempDir()
	source := filepath.Join(projectRoot, InRepoConfigDirName, TomlConfigFileName)
	resolved := &ResolvedConfig{
		ProjectRoot: projectRoot,
		Resolution: []ResolvedValue{{
			Winner:     &SourceRef{Layer: SourceRepoShared.String(), Path: source},
			Candidates: []CandidateTrace{{Layer: SourceRepoShared.String(), Path: source}},
		}},
	}

	err := resolved.RebaseProjectPathsForDisplay(otherRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not resolve to project root")
	assert.Equal(t, projectRoot, resolved.ProjectRoot)
	assert.Equal(t, source, resolved.Resolution[0].Winner.Path)
	assert.Equal(t, source, resolved.Resolution[0].Candidates[0].Path)
}

func TestRebaseProjectPathsForDisplayValidatesEverySourceBeforeMutation(t *testing.T) {
	container := t.TempDir()
	realParent := filepath.Join(container, "real")
	displayParent := filepath.Join(container, "selected")
	projectRoot := filepath.Join(realParent, "repo")
	require.NoError(t, os.MkdirAll(projectRoot, 0755))
	require.NoError(t, os.Symlink(realParent, displayParent))
	displayRoot := filepath.Join(displayParent, "repo")
	inside := filepath.Join(projectRoot, InRepoConfigDirName, TomlConfigFileName)
	outside := filepath.Join(container, "outside.toml")
	resolved := &ResolvedConfig{
		ProjectRoot: projectRoot,
		Resolution: []ResolvedValue{{
			Winner: &SourceRef{Layer: SourceRepoShared.String(), Path: inside},
			Candidates: []CandidateTrace{
				{Layer: SourceRepoShared.String(), Path: inside},
				{Layer: SourceRepoShared.String(), Path: outside},
			},
		}},
	}

	err := resolved.RebaseProjectPathsForDisplay(displayRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside project root")
	assert.Equal(t, projectRoot, resolved.ProjectRoot)
	assert.Equal(t, inside, resolved.Resolution[0].Winner.Path)
	assert.Equal(t, inside, resolved.Resolution[0].Candidates[0].Path)
	assert.Equal(t, outside, resolved.Resolution[0].Candidates[1].Path)
}
