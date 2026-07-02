package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHookBackendIsAliveListCmdOutputThenOrphan demonstrates a bug where
// a script that exits with code 0 and outputs valid JSON returns false
// from IsAlive because ErrWaitDelay is treated as a failure.
//
// This violates the documented protocol in docs/remote-hooks.md:24-25:
// "All scripts must: Return exit code 0 on success"
func TestHookBackendIsAliveListCmdOutputThenOrphan(t *testing.T) {
	dir := t.TempDir()
	// Script outputs valid JSON, then exits with code 0 while child holds stdout
	listCmd := writeScript(t, dir, "list.sh", `echo '[{"name":"test-session","status":"running"}]'
sleep 30 &
exit 0
`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	alive := b.IsAlive(i)

	// BUG DEMONSTRATION: This assertion FAILS with current code
	if !alive {
		t.Errorf("BUG: IsAlive returned false for script that exited with code 0 and output valid JSON")
	}
}

// TestHookBackendIsAliveOrphanNoJSON is the complement to the #676 fix and a
// guard against making the ErrWaitDelay tolerance too loose. A list_cmd that
// backgrounds a child holding stdout open (so CombinedOutput returns
// exec.ErrWaitDelay) but never emits parseable JSON must still return false:
// tolerating ErrWaitDelay must not short-circuit the extractJSON +
// json.Unmarshal validation. It also exercises the #666 contract that IsAlive
// returns promptly rather than blocking on the orphaned child's 30s sleep.
func TestHookBackendIsAliveOrphanNoJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout-bound test in short mode")
	}

	dir := t.TempDir()
	// Exits 0 with no JSON on stdout, but a backgrounded child holds the pipe
	// open past the parent's exit, triggering exec.ErrWaitDelay.
	listCmd := writeScript(t, dir, "list.sh", `echo "starting up..." >&2
sleep 30 &
exit 0
`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	start := time.Now()
	alive := b.IsAlive(i)
	elapsed := time.Since(start)

	assert.False(t, alive, "IsAlive must report false when list_cmd emits no parseable JSON, even on ErrWaitDelay")
	// Must not block on the orphaned child's sleep (#666): the parent exits
	// immediately and WaitDelay bounds the trailing pipe read at 500ms.
	assert.Less(t, elapsed, runtimeAliveTimeout,
		"IsAlive must return promptly when the script exits but a child holds the pipe (got %v)", elapsed)
}

// makeOrphanHooks builds a HookBackend whose launch_cmd exits 0 but emits the
// given output (simulating a remote session that was created but whose
// metadata we cannot parse), and whose delete_cmd appends the slug it was
// asked to delete to a marker file. The returned path is that marker file, so
// tests can assert whether delete_cmd ran and with what slug (#739).
func makeOrphanHooks(t *testing.T, launchOutput string) (*HookBackend, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "deleted.log")

	launchCmd := writeScript(t, dir, "launch.sh", "echo '"+launchOutput+"'\nexit 0\n")
	// $2 is the slug (delete_cmd is invoked as: --name <slug> --json).
	deleteCmd := writeScript(t, dir, "delete.sh", "echo \"$2\" >> \""+marker+"\"\n")
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached to $1"; sleep 0.1`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
	return b, marker
}

// TestHookBackendStartCleansUpOrphanOnNoJSON is the primary regression test for
// #739: launch_cmd exits 0 (creating a remote session) but emits no JSON.
// Start must invoke delete_cmd best-effort to clean up the orphaned remote
// session, then return the parse error to the user.
func TestHookBackendStartCleansUpOrphanOnNoJSON(t *testing.T) {
	b, marker := makeOrphanHooks(t, "starting up... done, but no json here")
	i := &Instance{Title: "orphan-session", Path: t.TempDir(), backend: b}

	err := b.Start(i, true)

	require.Error(t, err, "Start must surface the parse failure")
	assert.Contains(t, err.Error(), "no JSON")
	assert.False(t, i.Started(), "instance must not be marked Started on parse failure")
	assert.Nil(t, i.remoteMeta, "remoteMeta must remain unset on parse failure")

	deleted, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "delete_cmd must have run and written the marker")
	assert.Contains(t, string(deleted), Slugify("orphan-session"),
		"delete_cmd must be invoked with the slug launch_cmd received")
}

// TestHookBackendStartCleansUpOrphanOnInvalidJSON covers the second leg of
// #739: launch_cmd exits 0 and emits a parseable JSON value that is not the
// expected object (a JSON array), so json.Unmarshal into the metadata map
// fails. The orphan must still be cleaned up via delete_cmd.
func TestHookBackendStartCleansUpOrphanOnInvalidJSON(t *testing.T) {
	b, marker := makeOrphanHooks(t, "[1, 2, 3]")
	i := &Instance{Title: "orphan-session", Path: t.TempDir(), backend: b}

	err := b.Start(i, true)

	require.Error(t, err, "Start must surface the invalid-JSON error")
	assert.Contains(t, err.Error(), "invalid JSON")
	assert.False(t, i.Started())
	assert.Nil(t, i.remoteMeta)

	deleted, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "delete_cmd must have run on invalid JSON")
	assert.Contains(t, string(deleted), Slugify("orphan-session"))
}

// TestHookBackendStartHappyPathSkipsCleanup guards against over-eager cleanup:
// a launch_cmd that returns well-formed JSON must NOT trigger delete_cmd. The
// session is healthy and deleting it would defeat the create.
func TestHookBackendStartHappyPathSkipsCleanup(t *testing.T) {
	b, marker := makeOrphanHooks(t, `{"name": "orphan-session", "status": "running"}`)
	i := &Instance{Title: "orphan-session", Path: t.TempDir(), backend: b}

	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())
	require.NotNil(t, i.remoteMeta)
	assert.Equal(t, "running", i.remoteMeta["status"])
	b.closePTY(i.Title)

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr),
		"delete_cmd must not run on the happy path (marker file should not exist)")
}

// TestHookBackendStartCleanupBestEffort verifies the cleanup is best-effort:
// when launch_cmd produces no JSON AND delete_cmd itself fails, Start still
// returns the original parse error (not the delete failure) so the user sees
// the root cause. Cleanup failures are logged, not surfaced or retried.
func TestHookBackendStartCleanupBestEffort(t *testing.T) {
	dir := t.TempDir()
	launchCmd := writeScript(t, dir, "launch.sh", "echo 'no json'\nexit 0\n")
	deleteCmd := writeScript(t, dir, "delete.sh", "echo 'delete boom' >&2\nexit 1\n")
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
	i := &Instance{Title: "orphan-session", Path: t.TempDir(), backend: b}

	err := b.Start(i, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON",
		"the original parse error must surface even when best-effort cleanup fails")
	assert.NotContains(t, err.Error(), "delete boom",
		"delete_cmd failure must not replace the root-cause error")
}
