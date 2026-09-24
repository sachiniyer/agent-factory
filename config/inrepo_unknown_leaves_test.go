package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInRepoUnknownLeafInteractiveWarning pins the CLI stderr surface (#4599):
// with an interactive writer installed, the unknown-leaf warning is printed
// there as well as logged, once per process across repeated loads, while the
// load through ResolveConfig succeeds. A nil writer (daemon, TUI) keeps it
// log-only.
func TestInRepoUnknownLeafInteractiveWarning(t *testing.T) {
	repoRoot := setupResolveTest(t, `{}`)
	warnings := captureLog(t, &log.WarningLog)
	var stderr bytes.Buffer
	SetInteractiveWarningWriter(&stderr)
	t.Cleanup(func() { SetInteractiveWarningWriter(nil) })
	path := writeInRepoTomlConfig(t, repoRoot, "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n")

	for range 2 {
		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, res.Docker)
		assert.Equal(t, "myimg", res.Docker.Image)
	}

	want := "warning: in-repo config " + path + `: unknown key "runargs" under "docker" is ignored; did you mean "run_args"? the config still loads (allowed docker keys: image, run_args)` + "\n"
	assert.Equal(t, want, stderr.String(), "printed once, not once per load")
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "runargs"`), "logged once, not once per load")

	SetInteractiveWarningWriter(nil)
	resetUnknownTableLeafWarnings()
	stderr.Reset()
	_, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "a nil writer keeps the warning log-only")
}

// TestInRepoUnknownLeavesReported: the loaded config reports each unknown leaf
// with its suggestion on every load — not just the first, which is all the
// memoized warning covers — so af doctor can list it. A clean file reports none.
func TestInRepoUnknownLeavesReported(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nrunargs = [\"x\"]\n[ssh]\nhots = \"h\"\nnetwork = \"n\"\n")
	for range 2 {
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		leaves := cfg.UnknownLeaves()
		require.Len(t, leaves, 3)
		assert.Equal(t, []string{"docker.runargs", "ssh.hots", "ssh.network"},
			[]string{leaves[0].Table + "." + leaves[0].Key, leaves[1].Table + "." + leaves[1].Key, leaves[2].Table + "." + leaves[2].Key})
		assert.Equal(t, "run_args", leaves[0].Suggestion)
		assert.Equal(t, "host", leaves[1].Suggestion)
		assert.Contains(t, leaves[0].Message, `did you mean "run_args"?`)
	}

	clean := t.TempDir()
	writeInRepoTomlConfig(t, clean, "[docker]\nImage = \"af\"\nrun_args = [\"--read-only\"]\n")
	cfg, _, err := LoadInRepoConfig(clean)
	require.NoError(t, err)
	assert.Empty(t, cfg.UnknownLeaves())
}
