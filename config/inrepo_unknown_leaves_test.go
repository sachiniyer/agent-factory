package config

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInRepoUnknownLeafFailsTheLoad pins the owner's reject-after-warning
// decision (#4599 warned, #4845 rejects): an unknown [docker]/[ssh] leaf fails
// the load through the real resolver, with an error naming the file, the key and
// the closest valid key, and nothing is warned (an error is the one signal).
func TestInRepoUnknownLeafFailsTheLoad(t *testing.T) {
	repoRoot := setupResolveTest(t, `{}`)
	warnings := captureLog(t, &log.WarningLog)
	var stderr bytes.Buffer
	SetInteractiveWarningWriter(&stderr)
	t.Cleanup(func() { SetInteractiveWarningWriter(nil) })
	path := writeInRepoTomlConfig(t, repoRoot, "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n")

	for range 2 {
		_, err := ResolveConfig(repoRoot)
		require.Error(t, err, "an unknown [docker] leaf must fail the load")
		assert.Contains(t, err.Error(), path, "the error names the file")
		assert.Contains(t, err.Error(), `unknown key "runargs" under "docker"`)
		assert.Contains(t, err.Error(), `did you mean "run_args"?`, "and the closest valid key")
		assert.Contains(t, err.Error(), "allowed docker keys: image, run_args")
		assert.NotContains(t, err.Error(), "ignored", "the key is no longer ignored")
	}
	assert.Empty(t, stderr.String(), "a rejected file prints no separate warning")
	assert.Empty(t, warnings.String())
}

// TestInRepoUnknownLeavesErrorListsEveryLeaf: the error is typed, carries each
// leaf with its suggestion (sorted by table then key), and names them all in one
// message so a file with several typos is fixed in one pass. A clean file, and
// the case-folded spelling the typed decode accepts, still load.
func TestInRepoUnknownLeavesErrorListsEveryLeaf(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nrunargs = [\"x\"]\n[ssh]\nhots = \"h\"\nnetwork = \"n\"\n")

	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.Error(t, err)
	assert.Nil(t, cfg)
	var unknown *InRepoUnknownLeavesError
	require.True(t, errors.As(err, &unknown))
	require.Len(t, unknown.Leaves, 3)
	var keys []string
	for _, l := range unknown.Leaves {
		keys = append(keys, l.Table+"."+l.Key)
	}
	assert.Equal(t, []string{"docker.runargs", "ssh.hots", "ssh.network"}, keys)
	assert.Equal(t, "run_args", unknown.Leaves[0].Suggestion)
	assert.Equal(t, "host", unknown.Leaves[1].Suggestion)
	assert.Empty(t, unknown.Leaves[2].Suggestion, "no known key is near enough to suggest")
	for _, key := range []string{`"runargs"`, `"hots"`, `"network"`} {
		assert.Contains(t, err.Error(), key, "every leaf is named in the one error")
	}

	clean := t.TempDir()
	writeInRepoTomlConfig(t, clean, "[docker]\nImage = \"af\"\nrun_args = [\"--read-only\"]\n")
	_, _, err = LoadInRepoConfig(clean)
	require.NoError(t, err, "known leaves, in any case, are unaffected")
}
