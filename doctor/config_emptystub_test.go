package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// TestReportConfigValidity_EmptyStubIsWarnNotFail is the `af doctor` half of the
// fix's core guarantee: a contentless config.toml with no shadowing config.json
// is a state af self-heals at startup, so doctor reports it as an advisory WARN
// (problem=false) — never the FAIL row and exit 1 it raised before. A health
// check that FAILs a state af considers healthy disagrees with the very thing
// it diagnoses.
func TestReportConfigValidity_EmptyStubIsWarnNotFail(t *testing.T) {
	load := config.ReadOnlyConfigLoad{EmptyStub: true, Path: "/home/x/config.toml"}
	report := &Report{}
	reportConfigValidity(report, load, nil)

	row := findCheck(t, report, "config")
	require.Equal(t, StatusWarn, row.Status, "an empty stub is advisory, not a failure")
	require.False(t, row.Problem, "an empty stub self-heals at startup; it must not drive a non-zero exit")
	require.Contains(t, row.Detail, "empty config stub")
	require.Equal(t, sectionConfig, row.Section)
	require.Equal(t, 0, report.UnresolvedCount(), "an empty stub must not contribute to the exit code")
}

// TestReportConfigValidity_MissingStaysDistinct guards against the easy mistake
// of routing EmptyStub through the Missing path: the two states have different
// operator messages. The Missing message ("no config file at X") would be a
// lie for a file that plainly exists, so EmptyStub must keep its own row.
func TestReportConfigValidity_MissingStaysDistinct(t *testing.T) {
	load := config.ReadOnlyConfigLoad{Missing: true, Path: "/home/x/config.toml"}
	report := &Report{}
	reportConfigValidity(report, load, nil)

	row := findCheck(t, report, "config")
	require.Equal(t, StatusWarn, row.Status)
	require.False(t, row.Problem)
	require.Contains(t, row.Detail, "no config file at")
	require.NotContains(t, row.Detail, "empty config stub")
}

// TestWorktreeMode_EmptyStubDefaultsToSibling pins that an empty stub is treated
// like Missing for the worktree-root inference: both self-heal to DefaultConfig,
// whose WorktreeRoot is WorktreeRootSibling. Without this, an empty stub would
// fall through to "unknown" and doctor would emit the "config did not load"
// problem WARN — re-introducing a non-zero exit for a state af boots cleanly on.
func TestWorktreeMode_EmptyStubDefaultsToSibling(t *testing.T) {
	require.Equal(t, config.WorktreeRootSibling, worktreeMode(nil, false, true), "empty stub mirrors Missing's default")
	require.Equal(t, config.WorktreeRootSibling, worktreeMode(nil, true, false), "Missing regression: still the default")
	require.Equal(t, "unknown", worktreeMode(nil, false, false), "truly nothing loaded stays unknown")
	cfg := &config.Config{WorktreeRoot: config.WorktreeRootSubdirectory}
	require.Equal(t, config.WorktreeRootSubdirectory, worktreeMode(cfg, false, true), "a loaded config still wins over the inferred default")
}

// TestCheckConfigAndStorage_EmptyStubIsHealthy is the end-to-end guarantee:
// `af doctor` on a contentless config.toml (no config.json) reports the config
// row as an advisory WARN and produces NO actionable (problem) row — so
// doctorExitCode returns 0, matching a state af boots cleanly on. Before the
// fix the config row was a FAIL and doctor exited 1.
func TestCheckConfigAndStorage_EmptyStubIsHealthy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	tomlPath := filepath.Join(home, config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# placeholder\n"), 0o644))

	report := &Report{}
	cfg := checkConfigAndStorage(&scanContext{opts: Options{ConfigDir: home}}, report)
	require.NotNil(t, cfg, "EmptyStub carries DefaultConfig() so downstream diagnostics can evaluate the next-start posture")

	row := findCheck(t, report, "config")
	require.Equal(t, StatusWarn, row.Status)
	require.False(t, row.Problem, "doctor must not FAIL an empty stub")
	require.Contains(t, row.Detail, "empty config stub")

	// The af header reflects the post-self-heal channel, not "unknown" — the
	// buggy path surfaced "channel: unknown" because LoadConfigReadOnly errored.
	var afHeader string
	for _, h := range report.Header {
		if h.Label == "af" {
			afHeader = h.Value
		}
	}
	require.Contains(t, afHeader, "channel: stable", "an empty stub self-heals to defaults whose channel is stable")

	// No actionable row at all ⇒ UnresolvedCount == 0 ⇒ doctor exits 0.
	for _, c := range report.Checks {
		assert.Falsef(t, c.Problem, "problem row for a self-healing state: %q — %s", c.Name, c.Detail)
	}
	assert.Equal(t, 0, report.UnresolvedCount())

	// And it stayed no-write: the stub is untouched and nothing was created.
	stub, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.Equal(t, "# placeholder\n", string(stub), "doctor must not rewrite the stub it checks")
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Len(t, entries, 1, "doctor must not materialize any file beside the stub")
}
