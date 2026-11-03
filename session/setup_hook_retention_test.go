package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// setup_commands (#6667) writes each entry's output to a per-run hook log
// under $AF_HOME/logs/hooks. The hooklog lifecycle contract — documented on
// hooklog.CloseAndReadTail and implemented by the post-worktree and on-archive
// runners — is that a successful entry removes its log file afterwards, while a
// failed entry keeps the file and names it in the durable note. These tests pin
// that contract for the setup hook: before the fix, every successful entry
// retained its log, eroding the 20-per-kind retention quota and evicting
// failed-run evidence logs whose paths are embedded verbatim in the
// setup_hook_failed session note.

// hookLogDir returns the directory holding per-run hook logs.
func hookLogDir(t *testing.T) string {
	t.Helper()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	return filepath.Join(home, "logs", "hooks")
}

// countSetupHookLogs counts retained setup-v1-*.log files in the hooks dir.
func countSetupHookLogs(t *testing.T) int {
	t.Helper()
	dir := hookLogDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		require.NoError(t, err)
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "setup-v1-") && strings.HasSuffix(name, ".log") {
			n++
		}
	}
	return n
}

// ageSetupHookLogs ages all setup-v1-* files (logs and .done markers) past the
// hooklog retention grace period (5s) so they participate in the per-kind
// quota on the next hooklog.Open. This simulates the passage of time that lets
// accumulated success logs evict a failed run's evidence log.
func ageSetupHookLogs(t *testing.T) {
	t.Helper()
	dir := hookLogDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		require.NoError(t, err)
	}
	past := time.Now().Add(-10 * time.Second)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "setup-v1-") {
			require.NoError(t, os.Chtimes(filepath.Join(dir, name), past, past))
		}
	}
}

// ageFile ages a specific file (and its .done marker if present) to the given
// age, so its retention-time is older than the grace period and it sorts as
// the oldest entry in the per-kind quota.
func ageFile(t *testing.T, path string, age time.Duration) {
	t.Helper()
	past := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(path, past, past))
	if _, err := os.Stat(path + ".done"); err == nil {
		require.NoError(t, os.Chtimes(path+".done", past, past))
	}
}

// A successful setup_commands entry must remove its hook log file — the same
// lifecycle the post-worktree and on-archive runners carry — so retained
// success logs do not erode the per-kind retention quota protecting
// failed-run evidence named in the durable note.
func TestSetupHookSuccessRemovesHookLog(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo ok"]
`)

	opts := InstanceOptions{ID: "sess-success-log", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)
	assert.Equal(t, 0, countSetupHookLogs(t),
		"a successful setup entry must remove its hook log so it does not consume the retention quota")
}

// Multiple successful entries must each remove their own hook log — no
// setup-v1-*.log files should accumulate across a run of several successful
// entries.
func TestSetupHookSuccessRemovesHookLogAcrossEntries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = [
  "echo one > \"$AF_SETUP_DEST/order\"",
  "echo two >> \"$AF_SETUP_DEST/order\"",
  "echo three >> \"$AF_SETUP_DEST/order\"",
]
`)

	opts := InstanceOptions{ID: "sess-success-multi", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	assert.Equal(t, "", note)
	assert.Equal(t, 0, countSetupHookLogs(t),
		"every successful entry must remove its hook log; no setup-v1-*.log should accumulate")
}

// A failed setup_commands entry must retain its hook log file — the note names
// that path as the full output, and the file must survive for the operator to
// inspect.
func TestSetupHookFailureRetainsHookLog(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo failure-output >&2; exit 3"]
`)

	opts := InstanceOptions{ID: "sess-fail-log", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	require.NotEqual(t, "", note)
	assert.Contains(t, note, "full output:")

	const marker = "full output: "
	idx := strings.Index(note, marker)
	require.NotEqual(t, -1, idx)
	logPath := strings.TrimSpace(note[idx+len(marker):])
	out, err := os.ReadFile(logPath)
	require.NoError(t, err, "the retained hook log must exist and hold the command's output")
	assert.Contains(t, string(out), "failure-output")

	assert.Equal(t, 1, countSetupHookLogs(t),
		"a failed setup entry must retain its hook log for inspection")
}

// A timed-out entry must also retain its hook log — the note names the path,
// and the operator needs the output to diagnose the hang.
func TestSetupHookTimeoutRetainsHookLog(t *testing.T) {
	prev := setupCommandTimeout
	setupCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { setupCommandTimeout = prev })

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["sleep 60"]
`)

	opts := InstanceOptions{ID: "sess-timeout-log", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	require.NotEqual(t, "", note)
	assert.Contains(t, note, "timed out")
	assert.Contains(t, note, "full output:")

	const marker = "full output: "
	idx := strings.Index(note, marker)
	require.NotEqual(t, -1, idx)
	logPath := strings.TrimSpace(note[idx+len(marker):])
	_, err := os.Stat(logPath)
	assert.NoError(t, err, "a timed-out entry must retain its hook log for inspection")
}

// The core retention guarantee: a failed entry's hook log — whose path is
// embedded verbatim in the durable setup_hook_failed note — must survive many
// subsequent successful entries. Before the fix, every successful entry
// retained its log, consuming the 20-per-kind quota and evicting the
// failure-evidence log after ~20 successes. After the fix, successful entries
// remove their logs, so the failure log is never evicted.
func TestSetupHookFailureLogSurvivesSuccessfulEntries(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)

	// Run a failing entry and capture the evidence log path.
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo evidence-output >&2; exit 3"]
`)
	opts := InstanceOptions{ID: "sess-evidence", Title: "sess", Path: repoRoot}
	note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
	require.NotEqual(t, "", note)
	const marker = "full output: "
	idx := strings.Index(note, marker)
	require.NotEqual(t, -1, idx)
	evidenceLogPath := strings.TrimSpace(note[idx+len(marker):])
	out, err := os.ReadFile(evidenceLogPath)
	require.NoError(t, err)
	assert.Contains(t, string(out), "evidence-output")

	// Age the evidence log well past the retention grace period (and well
	// older than the success logs below) so it is the oldest entry in the
	// per-kind quota and the first to be evicted when the quota fills. Before
	// the fix, accumulated success logs would push this log out after ~20
	// entries; after the fix, success logs are removed and this log survives.
	ageFile(t, evidenceLogPath, time.Hour)

	// Run 25 successful entries — past the 20-per-kind quota boundary. After
	// each, age any new success logs past the grace period (10s) so they
	// participate in the per-kind quota, then re-age the evidence log to keep
	// it the oldest. This simulates the accumulation that, before the fix,
	// evicted the evidence log after ~20 successes. With the fix, successful
	// entries remove their logs, so no new logs accumulate and the evidence
	// log is never evicted.
	writeSetupCommandsConfig(t, repoRoot, `setup_commands = ["echo ok"]
`)
	for i := 0; i < 25; i++ {
		opts := InstanceOptions{ID: "sess-success", Title: "sess", Path: repoRoot}
		note := runSessionSetupHook(opts, repoRoot, BackendLocal, &LocalBackend{}, false)
		assert.Equal(t, "", note, "successful entry %d must not produce a failure note", i)
		ageSetupHookLogs(t)
		// Keep the evidence log the oldest entry if it still exists. Without
		// the fix it gets evicted mid-loop, so guard the re-aging.
		if _, statErr := os.Stat(evidenceLogPath); statErr == nil {
			ageFile(t, evidenceLogPath, time.Hour)
		}
	}

	// The evidence log must still exist — its path is embedded in the durable
	// note, and the operator following that reference must find the file.
	_, err = os.Stat(evidenceLogPath)
	assert.NoError(t, err,
		"the failure-evidence hook log must survive subsequent successful entries; its path is embedded in the durable note")

	// Only the failed entry's log should remain; successful entries must
	// remove their logs.
	assert.Equal(t, 1, countSetupHookLogs(t),
		"only the failed entry's log should remain; successful entries must remove their logs")
}
