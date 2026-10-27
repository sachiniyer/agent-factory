package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inRepoLeavesFrom loads a real in-repo config written to a temp repo, so the
// finding is built from what LoadInRepoConfig reports, not a hand-made fixture.
func inRepoLeavesFrom(t *testing.T, body string) func() []config.InRepoUnknownLeaf {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := t.TempDir()
	dir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.TomlConfigFileName), []byte(body), 0o644))
	return func() []config.InRepoUnknownLeaf {
		cfg, _, err := config.LoadInRepoConfig(repoRoot)
		require.NoError(t, err, "an unknown leaf must not fail the load")
		return cfg.UnknownLeaves()
	}
}

// TestDoctorWarnsOnInRepoUnknownLeaf: a typo'd [docker] leaf in the checked-in
// config is an advisory WARN naming the file, the key and the likely intended
// key (#4599) — advisory, so `af doctor` still exits 0 while the load succeeds.
func TestDoctorWarnsOnInRepoUnknownLeaf(t *testing.T) {
	ctx := &scanContext{opts: Options{inRepoUnknownLeaves: inRepoLeavesFrom(t,
		"[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n")}}
	report := &Report{}
	checkInRepoUnknownLeaves(ctx, report)

	rows := findCheckRows(report, "in-repo config")
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, StatusWarn, row.Status)
	assert.Equal(t, sectionConfig, row.Section)
	assert.Contains(t, row.Detail, filepath.Join(config.InRepoConfigDirName, config.TomlConfigFileName))
	assert.Contains(t, row.Detail, `unknown key "runargs" under "docker"`)
	assert.Contains(t, row.Detail, "did you mean \"run_args\"?")
	assert.Contains(t, row.Remediation, "rename docker.runargs to docker.run_args")
	assert.Zero(t, report.UnresolvedCount(), "advisory until the hard error lands (#4845)")
}

// TestDoctorSilentOnCleanInRepoConfig: a valid in-repo config adds no row.
func TestDoctorSilentOnCleanInRepoConfig(t *testing.T) {
	ctx := &scanContext{opts: Options{inRepoUnknownLeaves: inRepoLeavesFrom(t,
		"[docker]\nimage = \"myimg\"\nrun_args = [\"--read-only\"]\n")}}
	report := &Report{}
	checkInRepoUnknownLeaves(ctx, report)
	assert.Empty(t, findCheckRows(report, "in-repo config"))
}
