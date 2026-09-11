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

func TestMissingConfigKeepsDefaultAgentBinaryAdvisory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("PATH", t.TempDir())

	ctx := &scanContext{opts: Options{ConfigDir: home}}
	report := &Report{}
	cfg := checkConfigAndStorage(ctx, report)
	require.Nil(t, cfg, "a missing file must not make defaults look user-configured to every downstream check")

	checkAgentBinaries(cfg, report)
	claude := findCheck(t, report, "claude")
	require.Equal(t, StatusWarn, claude.Status)
	require.False(t, claude.Problem,
		"an absent config does not establish that the default agent binary is a user-configured requirement")
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

// TestCheckConfigAndStorage_EmptyStubDefaultHomeReadOnlyIsHealthy is the
// end-to-end `af doctor` guarantee for the chmod-repairable default home: a
// contentless config.toml in an owner-owned default ~/.agent-factory tightened
// to a write-less mode (0500) is a state af self-heals at startup, so doctor's
// CONFIG validity row must be an advisory WARN (problem=false), not the FAIL
// (problem=true) it raised before the fix when the read-only diagnostic errored
// on the unrepaired 0500 mode. The config row agreeing with startup is the
// in-scope fix; see the inline note about the separate hook-logs check.
//
// Unlike TestCheckConfigAndStorage_EmptyStubIsHealthy above, this stages the home
// as the CONCRETE default via $HOME (AGENT_FACTORY_HOME empty) at a write-less
// mode — the arrangement the gate's write-permission probe wrongly rejected and
// that seedHome-based tests never reach by pinning a custom home.
func TestCheckConfigAndStorage_EmptyStubDefaultHomeReadOnlyIsHealthy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses mode bits, so a 0500 home cannot be staged as non-writable")
	}
	t.Setenv("SHELL", "/bin/sh")
	userHome := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	tomlPath := filepath.Join(afHome, config.TomlConfigFileName)
	require.NoError(t, os.WriteFile(tomlPath, []byte("# placeholder\n"), 0o644))
	require.NoError(t, os.Chmod(afHome, 0o500))
	t.Cleanup(func() { _ = os.Chmod(afHome, 0o755) })
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	report := &Report{}
	cfg := checkConfigAndStorage(&scanContext{opts: Options{ConfigDir: afHome}}, report)
	require.NotNil(t, cfg, "EmptyStub carries DefaultConfig() so downstream diagnostics can evaluate the next-start posture")

	row := findCheck(t, report, "config")
	require.Equal(t, StatusWarn, row.Status, "an empty stub is advisory, not a failure")
	require.False(t, row.Problem, "a chmod-repairable default home self-heals at startup; the config row must not FAIL it")
	require.Contains(t, row.Detail, "empty config stub")

	// The config row is no longer the false FAIL it was before the fix. The
	// hook-logs storage check is a SEPARATE diagnostic that probes the home's
	// current writability for creating logs/hooks; like the gate this fix
	// replaces, it does not model secureAFHomeForPath's chmod repair, so it
	// may still report a problem on a write-less default home. That is outside
	// this bug report's scope (the empty-stub config gate); the guarantee here is
	// that the CONFIG row agrees with startup, not that every other check
	// passes on a read-only home.

	// No-write: the stub is untouched and the home stays read-only.
	stub, err := os.ReadFile(tomlPath)
	require.NoError(t, err)
	require.Equal(t, "# placeholder\n", string(stub), "doctor must not rewrite the stub it checks")
	info, err := os.Stat(afHome)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "doctor must not chmod-repair the home it reports on")
}
