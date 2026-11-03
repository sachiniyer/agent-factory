package config

// Compatibility tests for the removed Backlog feature's [backlog] config
// table (#6662), mirroring the auto_yes retirement tests: existing files load
// (with at most a one-time warning when the table did something), and new
// writes of backlog.* are refused with the migration error.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

// captureBacklogWarnings routes WarningLog through a buffer and clears the
// once-per-source memo so each test sees the first notice.
func captureBacklogWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var warnings bytes.Buffer
	old := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	resetRemovedBacklogWarnings()
	t.Cleanup(func() {
		log.WarningLog.SetOutput(old)
		resetRemovedBacklogWarnings()
	})
	return &warnings
}

func writeGlobalToml(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	path := filepath.Join(dir, TomlConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestRemovedBacklogTomlLoadsOnceWarnsOnce(t *testing.T) {
	warnings := captureBacklogWarnings(t)
	writeGlobalToml(t, "[backlog]\nplan_usage = true\n")

	_, err := LoadConfig()
	require.NoError(t, err, "a config written by the Backlog era must not block startup")
	require.Contains(t, warnings.String(), "Backlog")
	require.Contains(t, warnings.String(), "#6662")

	warnings.Reset()
	_, err = LoadConfig()
	require.NoError(t, err)
	require.NotContains(t, warnings.String(), "Backlog",
		"the migration notice is once per source, not once per load")
}

func TestRemovedBacklogPlanUsageFalseWarnsNothing(t *testing.T) {
	warnings := captureBacklogWarnings(t)
	writeGlobalToml(t, "[backlog]\nplan_usage = false\n")

	_, err := LoadConfig()
	require.NoError(t, err)
	require.NotContains(t, warnings.String(), "Backlog",
		"plan_usage=false asked for the same nothing the post-removal default does")
}

func TestRemovedBacklogOtherKeysWarn(t *testing.T) {
	warnings := captureBacklogWarnings(t)
	writeGlobalToml(t, "[backlog]\nrunner = 'watch'\n")

	_, err := LoadConfig()
	require.NoError(t, err)
	require.Contains(t, warnings.String(), "Backlog")
}

func TestRemovedBacklogJSONLoadsAndWarns(t *testing.T) {
	warnings := captureBacklogWarnings(t)
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName),
		[]byte(`{"backlog": {"plan_usage": true}}`), 0o644))

	_, err := LoadConfig()
	require.NoError(t, err)
	require.Contains(t, warnings.String(), "Backlog")
}

func TestRemovedBacklogInRepoLoadsStripped(t *testing.T) {
	captureBacklogWarnings(t)
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(InRepoTomlConfigPath(repo)), 0o755))
	require.NoError(t, os.WriteFile(InRepoTomlConfigPath(repo),
		[]byte("[backlog]\nplan_usage = true\n"), 0o644))
	// The table must not trip the in-repo allowlist.
	cfg, _, err := LoadInRepoConfig(repo)
	require.NoError(t, err)
	require.NotNil(t, cfg)
}

func TestRemovedBacklogNewWritesRefused(t *testing.T) {
	writeGlobalToml(t, "")
	_, err := SetGlobalConfigValue("backlog.plan_usage", "true")
	require.EqualError(t, err, RemovedBacklogMessage)
	_, err = UnsetGlobalConfigValue("backlog.plan_usage")
	require.EqualError(t, err, RemovedBacklogMessage)
}
