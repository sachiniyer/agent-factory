package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
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
